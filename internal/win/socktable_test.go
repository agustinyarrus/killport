package win

// Tests de bytes sintéticos para el parseo de las tablas de sockets. Sin build
// tag: corren en cualquier plataforma. Prueban PROPIEDADES (orden de red del
// puerto, IPv4 en orden de red, zona IPv6, FILETIME, relleno de alineación,
// tabla vacía, buffer truncado → error sin pánico), no un solo ejemplo.

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"
)

// buildTable arma una tabla: DWORD n, 4 de relleno, y n filas de rowSize bytes
// producidas por fill. Es el layout exacto que llena el sistema.
func buildTable(n int, rowSize int, fill func(i int, row []byte)) []byte {
	b := make([]byte, tableHeader+n*rowSize)
	binary.LittleEndian.PutUint32(b, uint32(n))
	// b[4:8] queda en cero: el relleno de alineación que precede a las filas.
	for i := 0; i < n; i++ {
		off := tableHeader + i*rowSize
		fill(i, b[off:off+rowSize])
	}
	return b
}

// putPort escribe un puerto en orden de red (2 bytes altos) dentro de un DWORD
// cuyos 2 bytes altos se ensucian a propósito: el parser debe ignorarlos.
func putPort(row []byte, off int, p uint16) {
	row[off] = byte(p >> 8) // byte alto del puerto, primero (orden de red)
	row[off+1] = byte(p)    // byte bajo
	row[off+2] = 0xDE       // basura en los 2 bytes altos del DWORD
	row[off+3] = 0xAD
}

// filetimeTicks es una hora conocida en ticks de FILETIME, ida y vuelta exacta.
func filetimeTicks(t time.Time) uint64 { return uint64(toFileTime(t)) }

func TestParseTCP4Table(t *testing.T) {
	// Una hora con precisión de 100 ns (la de FILETIME): así vuelve exacta.
	when := time.Date(2026, 9, 27, 1, 2, 3, 456789100, time.UTC)
	b := buildTable(2, tcp4RowSize, func(i int, row []byte) {
		if i == 0 { // un LISTEN
			binary.LittleEndian.PutUint32(row[tcp4State:], TCPListen)
			copy(row[tcp4LocalAddr:], []byte{0, 0, 0, 0}) // 0.0.0.0
			putPort(row, tcp4LocalPort, 3000)
			copy(row[tcp4RemoteAddr:], []byte{0, 0, 0, 0})
			putPort(row, tcp4RemotePort, 0)
			binary.LittleEndian.PutUint32(row[tcp4PID:], 4242)
			binary.LittleEndian.PutUint64(row[tcp4Created:], filetimeTicks(when))
		} else { // una conexión establecida a 192.0.2.68:139 → 10.0.0.5:51000
			binary.LittleEndian.PutUint32(row[tcp4State:], TCPEstablished)
			copy(row[tcp4LocalAddr:], []byte{192, 0, 2, 68})
			putPort(row, tcp4LocalPort, 139)
			copy(row[tcp4RemoteAddr:], []byte{10, 0, 0, 5})
			putPort(row, tcp4RemotePort, 51000)
			binary.LittleEndian.PutUint32(row[tcp4PID:], 4)
		}
	})
	socks, err := parseSocketTable(b, layoutTCP4)
	if err != nil {
		t.Fatal(err)
	}
	if len(socks) != 2 {
		t.Fatalf("esperaba 2 filas, hay %d", len(socks))
	}
	s0 := socks[0]
	if s0.Local != netip.MustParseAddrPort("0.0.0.0:3000") {
		t.Errorf("local = %v, esperaba 0.0.0.0:3000 (puerto en orden de red)", s0.Local)
	}
	if s0.State != TCPListen || s0.PID != 4242 {
		t.Errorf("state=%d pid=%d", s0.State, s0.PID)
	}
	if !s0.Created.UTC().Equal(when) {
		t.Errorf("created = %v, esperaba %v (FILETIME ida y vuelta)", s0.Created.UTC(), when)
	}
	if s0.Proto != TCP || s0.IPv6() {
		t.Errorf("proto=%v ipv6=%v", s0.Proto, s0.IPv6())
	}
	s1 := socks[1]
	if s1.Local != netip.MustParseAddrPort("192.0.2.68:139") {
		t.Errorf("local = %v, esperaba 192.0.2.68:139 (IPv4 en orden de red)", s1.Local)
	}
	if s1.Remote != netip.MustParseAddrPort("10.0.0.5:51000") {
		t.Errorf("remote = %v, esperaba 10.0.0.5:51000", s1.Remote)
	}
	if !s1.Created.IsZero() {
		t.Errorf("created de una fila sin timestamp debería ser cero, es %v", s1.Created)
	}
}

func TestParseTCP6TableConZona(t *testing.T) {
	b := buildTable(1, tcp6RowSize, func(_ int, row []byte) {
		binary.LittleEndian.PutUint32(row[tcp6State:], TCPListen)
		a := netip.MustParseAddr("fe80::4b0e:a8bd:4b4f:f050").As16()
		copy(row[tcp6LocalAddr:], a[:])
		binary.LittleEndian.PutUint32(row[tcp6LocalScope:], 15) // zona %15
		putPort(row, tcp6LocalPort, 1900)
		binary.LittleEndian.PutUint32(row[tcp6PID:], 6528)
	})
	socks, err := parseSocketTable(b, layoutTCP6)
	if err != nil {
		t.Fatal(err)
	}
	got := socks[0].Local
	want := netip.MustParseAddrPort("[fe80::4b0e:a8bd:4b4f:f050%15]:1900")
	if got != want {
		t.Errorf("local = %v, esperaba %v (zona IPv6 del scope)", got, want)
	}
	if !socks[0].IPv6() {
		t.Error("debería ser IPv6")
	}
}

func TestParseTCP6ScopeCeroSinZona(t *testing.T) {
	b := buildTable(1, tcp6RowSize, func(_ int, row []byte) {
		binary.LittleEndian.PutUint32(row[tcp6State:], TCPListen)
		a := netip.IPv6Unspecified().As16()
		copy(row[tcp6LocalAddr:], a[:]) // [::]
		binary.LittleEndian.PutUint32(row[tcp6LocalScope:], 0)
		putPort(row, tcp6LocalPort, 8080)
	})
	socks, _ := parseSocketTable(b, layoutTCP6)
	if socks[0].Local.Addr().Zone() != "" {
		t.Errorf("scope 0 no debería poner zona, puso %q", socks[0].Local.Addr().Zone())
	}
	if socks[0].Local != netip.MustParseAddrPort("[::]:8080") {
		t.Errorf("local = %v, esperaba [::]:8080", socks[0].Local)
	}
}

func TestParseUDP4Table(t *testing.T) {
	b := buildTable(1, udp4RowSize, func(_ int, row []byte) {
		copy(row[udp4LocalAddr:], []byte{0, 0, 0, 0})
		putPort(row, udp4LocalPort, 123)
		binary.LittleEndian.PutUint32(row[udp4PID:], 29512)
		binary.LittleEndian.PutUint32(row[udp4Flags:], 1) // SpecificPortBind
	})
	socks, err := parseSocketTable(b, layoutUDP4)
	if err != nil {
		t.Fatal(err)
	}
	s := socks[0]
	if s.Proto != UDP || s.Local != netip.MustParseAddrPort("0.0.0.0:123") {
		t.Errorf("proto=%v local=%v", s.Proto, s.Local)
	}
	if s.Remote.IsValid() {
		t.Errorf("UDP no tiene remoto, tiene %v", s.Remote)
	}
	if !s.SpecificPortBind {
		t.Error("el bit 0 de dwFlags debería marcar SpecificPortBind")
	}
	if s.State != 0 {
		t.Errorf("UDP no tiene estado TCP, tiene %d", s.State)
	}
}

func TestParseUDP6Table(t *testing.T) {
	b := buildTable(1, udp6RowSize, func(_ int, row []byte) {
		a := netip.MustParseAddr("::1").As16()
		copy(row[udp6LocalAddr:], a[:])
		binary.LittleEndian.PutUint32(row[udp6LocalScope:], 0)
		putPort(row, udp6LocalPort, 5353)
		binary.LittleEndian.PutUint32(row[udp6PID:], 6528)
		binary.LittleEndian.PutUint32(row[udp6Flags:], 0) // efímero
	})
	socks, err := parseSocketTable(b, layoutUDP6)
	if err != nil {
		t.Fatal(err)
	}
	s := socks[0]
	if s.Local != netip.MustParseAddrPort("[::1]:5353") {
		t.Errorf("local = %v, esperaba [::1]:5353", s.Local)
	}
	if s.SpecificPortBind {
		t.Error("dwFlags 0 no debería marcar SpecificPortBind")
	}
}

func TestParseTablaVacia(t *testing.T) {
	// El sistema puede devolver solo el DWORD n=0 (a veces con el relleno, a
	// veces sin él). Ambas formas dan cero filas, sin error.
	for _, b := range [][]byte{
		{0, 0, 0, 0},             // solo n
		{0, 0, 0, 0, 0, 0, 0, 0}, // n + relleno
	} {
		socks, err := parseSocketTable(b, layoutTCP4)
		if err != nil || len(socks) != 0 {
			t.Errorf("tabla vacía (%d bytes): socks=%v err=%v", len(b), socks, err)
		}
	}
}

func TestParseTablaTruncadaNoEntraEnPanico(t *testing.T) {
	// Dice 3 filas pero solo trae bytes para una y media: error, no pánico ni
	// lectura fuera de rango.
	b := buildTable(3, tcp4RowSize, func(_ int, _ []byte) {})
	b = b[:tableHeader+tcp4RowSize+tcp4RowSize/2]
	socks, err := parseSocketTable(b, layoutTCP4)
	if err == nil {
		t.Fatalf("una tabla truncada tiene que dar error, dio %d filas", len(socks))
	}
	if !isMalformed(err) {
		t.Errorf("error = %v, esperaba ErrMalformed", err)
	}
}

func TestParseHeaderTruncado(t *testing.T) {
	// Ni el DWORD de la cantidad entra: error, no pánico.
	if _, err := parseSocketTable([]byte{1, 2}, layoutTCP4); err == nil {
		t.Error("un búfer de 2 bytes tiene que dar error")
	}
}

func TestParseTablaDejaHolgura(t *testing.T) {
	// El búfer puede venir más grande que las filas declaradas (se pidió con
	// holgura): los bytes de más se ignoran, no cuentan como fila.
	b := buildTable(1, tcp4RowSize, func(_ int, row []byte) {
		putPort(row, tcp4LocalPort, 80)
	})
	b = append(b, make([]byte, 500)...) // holgura al final
	socks, err := parseSocketTable(b, layoutTCP4)
	if err != nil || len(socks) != 1 {
		t.Errorf("con holgura al final: socks=%d err=%v (esperaba 1 fila)", len(socks), err)
	}
}
