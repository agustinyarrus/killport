package win

// Tests de bytes sintéticos de la carpeta de trabajo por el PEB (64 y 32
// bits) y del búfer de GetAdaptersAddresses: layouts, validaciones y
// búferes corruptos que terminan en error, nunca en pánico.

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
)

// memoria es la memoria falsa de otro proceso: bloques en direcciones.
type memoria map[uint64][]byte

func (m memoria) leer(addr uint64, n int) ([]byte, error) {
	for base, b := range m {
		if addr >= base && addr+uint64(n) <= base+uint64(len(b)) {
			out := make([]byte, n)
			copy(out, b[addr-base:])
			return out, nil
		}
	}
	return nil, errors.New("fuera de la memoria mapeada")
}

func u16le(s string) []byte {
	units := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[2*i:], u)
	}
	return b
}

// procesoFalso arma PEB → parámetros → carpeta con el layout l.
func procesoFalso(l pebLayout, cwd string, length, maxLength int) memoria {
	const peb, params, buf = 0x7ff0_0000, 0x0200_0000, 0x0300_0000
	m := memoria{}
	pb := make([]byte, 0x80)
	pp := make([]byte, 0x80)
	if l.ptr == 4 {
		binary.LittleEndian.PutUint32(pb[l.params:], params)
		binary.LittleEndian.PutUint32(pp[l.curdir+l.buffer:], buf)
	} else {
		binary.LittleEndian.PutUint64(pb[l.params:], params)
		binary.LittleEndian.PutUint64(pp[l.curdir+l.buffer:], buf)
	}
	binary.LittleEndian.PutUint16(pp[l.curdir:], uint16(length))
	binary.LittleEndian.PutUint16(pp[l.curdir+2:], uint16(maxLength))
	m[peb], m[params], m[buf] = pb, pp, u16le(cwd)
	return m
}

func TestReadCwd(t *testing.T) {
	const cwd = `C:\dev\shop\wéb\`
	n := len(u16le(cwd))
	for _, c := range []struct {
		nombre string
		l      pebLayout
	}{{"64 bits", peb64}, {"WOW64 (32 bits)", peb32}} {
		m := procesoFalso(c.l, cwd, n, n+2)
		got, err := readCwd(m.leer, 0x7ff0_0000, c.l)
		if err != nil || got != cwd {
			t.Errorf("%s: %q, %v", c.nombre, got, err)
		}
		// Largos imposibles: error, no una ruta inventada.
		for _, mal := range []struct{ length, max int }{{0, 10}, {n + 1, n + 2}, {n, n - 2}, {maxPathBytes + 2, maxPathBytes + 4}} {
			m := procesoFalso(c.l, cwd, mal.length, mal.max)
			if s, err := readCwd(m.leer, 0x7ff0_0000, c.l); err == nil {
				t.Errorf("%s: Length %d/%d dio %q sin error", c.nombre, mal.length, mal.max, s)
			}
		}
	}
	if _, err := readCwd(memoria{}.leer, 0, peb64); err == nil {
		t.Error("sin PEB tiene que dar error")
	}
	if _, err := readCwd(memoria{}.leer, 0x1000, peb64); err == nil {
		t.Error("memoria ilegible tiene que dar error")
	}
}

func TestCleanDir(t *testing.T) {
	casos := map[string]string{
		`C:\dev\shop\`:              `C:\dev\shop`,
		`C:\`:                       `C:\`,
		`C:\dev`:                    `C:\dev`,
		`\\?\C:\largo\`:             `C:\largo`,
		`\\servidor\recurso\`:       `\\servidor\recurso`,
		`\\?\UNC\servidor\recurso\`: `\\servidor\recurso`,
	}
	for in, want := range casos {
		if got := CleanDir(in); got != want {
			t.Errorf("CleanDir(%q) = %q, esperaba %q", in, got, want)
		}
	}
}

// Una sola regla para el prefijo de ruta larga (antes CleanDir y la ruta
// final de un archivo tenían cada una la suya).
func TestTrimLongPrefix(t *testing.T) {
	for in, want := range map[string]string{
		`\\?\C:\largo\x.exe`:         `C:\largo\x.exe`,
		`\\?\UNC\servidor\recurso\x`: `\\servidor\recurso\x`,
		`C:\dev`:                     `C:\dev`,
		`\\servidor\recurso`:         `\\servidor\recurso`,
	} {
		if got := TrimLongPrefix(in); got != want {
			t.Errorf("TrimLongPrefix(%q) = %q, esperaba %q", in, got, want)
		}
	}
}

// adaptadorFalso arma un búfer de GetAdaptersAddresses con dos adaptadores.
func adaptadoresFalsos(base uintptr) []byte {
	b := make([]byte, 0x400)
	put64 := func(off int, v uint64) { binary.LittleEndian.PutUint64(b[off:], v) }
	put32 := func(off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
	addr := func(off int) uint64 { return uint64(base) + uint64(off) }
	// Adaptador 1 en 0x000, adaptador 2 en 0x100.
	for _, a := range []struct {
		off, name, uni int
		idx            uint32
		next           int
	}{{0x000, 0x300, 0x200, 12, 0x100}, {0x100, 0x340, 0x260, 7, -1}} {
		put32(a.off+iaaLength, 0x178)
		put32(a.off+iaaIfIndex, a.idx)
		if a.next >= 0 {
			put64(a.off+iaaNext, addr(a.next))
		}
		put64(a.off+iaaFirstUnicast, addr(a.uni))
		put64(a.off+iaaFriendlyName, addr(a.name))
	}
	copy(b[0x300:], append(u16le("Wi-Fi"), 0, 0))
	copy(b[0x340:], append(u16le("vEthernet (WSL)"), 0, 0))
	// Adaptador 1: una IPv4 y una fe80:: con zona 12.
	put64(0x200+iuaSockaddr, addr(0x380))
	put32(0x200+iuaSockLen, 16)
	put64(0x200+iuaNext, addr(0x220))
	put64(0x220+iuaSockaddr, addr(0x390))
	put32(0x220+iuaSockLen, 28)
	binary.LittleEndian.PutUint16(b[0x380:], afInet)
	copy(b[0x384:], []byte{192, 168, 1, 5})
	binary.LittleEndian.PutUint16(b[0x390:], afInet6)
	copy(b[0x398:], netip.MustParseAddr("fe80::1").AsSlice())
	put32(0x390+24, 12)
	// Adaptador 2: una IPv4.
	put64(0x260+iuaSockaddr, addr(0x3c0))
	put32(0x260+iuaSockLen, 16)
	binary.LittleEndian.PutUint16(b[0x3c0:], afInet)
	copy(b[0x3c4:], []byte{172, 24, 1, 1})
	return b
}

func TestParseAdapters(t *testing.T) {
	base := uintptr(0x50000)
	b := adaptadoresFalsos(base)
	got, err := parseAdapters(b, base)
	if err != nil {
		t.Fatal(err)
	}
	want := []Interface{
		{Index: 12, Name: "Wi-Fi", Addrs: []netip.Addr{netip.MustParseAddr("192.168.1.5"), netip.MustParseAddr("fe80::1%12")}},
		{Index: 7, Name: "vEthernet (WSL)", Addrs: []netip.Addr{netip.MustParseAddr("172.24.1.1")}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	// Una lista que se muerde la cola: error, no un bucle.
	c := append([]byte(nil), b...)
	binary.LittleEndian.PutUint64(c[0x100+iaaNext:], uint64(base))
	// (se detecta al volver, no al agotar el tope de nodos)
	if _, err := parseAdapters(c, base); !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), "vuelve") {
		t.Errorf("lista circular: %v", err)
	}
	c = append([]byte(nil), b...)
	binary.LittleEndian.PutUint64(c[0x220+iuaNext:], uint64(base)+0x200)
	if _, err := parseAdapters(c, base); !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), "vuelve") {
		t.Errorf("direcciones en círculo: %v", err)
	}
	// Punteros afuera, nombre sin terminar, búfer truncado: error, sin pánico.
	for _, rompe := range []func([]byte){
		func(c []byte) { binary.LittleEndian.PutUint64(c[iaaNext:], uint64(base)+0x10000) },
		func(c []byte) { binary.LittleEndian.PutUint64(c[iaaFriendlyName:], uint64(base)-8) },
		func(c []byte) { binary.LittleEndian.PutUint64(c[0x200+iuaSockaddr:], uint64(base)+0x3ff) },
		func(c []byte) { binary.LittleEndian.PutUint32(c[0x220+iuaSockLen:], 8) },
		func(c []byte) { binary.LittleEndian.PutUint32(c[iaaLength:], 4) },
	} {
		c := append([]byte(nil), b...)
		rompe(c)
		if _, err := parseAdapters(c, base); !errors.Is(err, ErrMalformed) {
			t.Errorf("búfer roto sin error: %v", err)
		}
	}
	for n := 0; n < len(b); n += 7 {
		_, _ = parseAdapters(b[:n], base) // truncado en cualquier lado: jamás un pánico
	}
}
