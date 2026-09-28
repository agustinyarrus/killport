package win

// Tests puros de los primitivos de lectura defensiva: ventanas fuera de rango,
// FILETIME ida y vuelta, UNICODE_STRING y cadenas terminadas en cero resueltas
// por desplazamiento relativo a una base sintética.

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

func isMalformed(err error) bool { return errors.Is(err, ErrMalformed) }

func TestWindowRangos(t *testing.T) {
	b := make([]byte, 10)
	if _, err := window(b, 0, 10, "todo"); err != nil {
		t.Errorf("la ventana exacta debería entrar: %v", err)
	}
	for _, c := range []struct{ off, n int }{{0, 11}, {5, 6}, {-1, 2}, {2, -1}, {11, 0}} {
		if _, err := window(b, c.off, c.n, "x"); err == nil {
			t.Errorf("window(%d,%d) sobre 10 bytes tendría que fallar", c.off, c.n)
		}
	}
}

func TestWindowNoSeExtiende(t *testing.T) {
	// Una ventana no puede crecer sobre los bytes vecinos con append (capacidad
	// recortada): así un parser no pisa la fila de al lado por error.
	b := make([]byte, 20)
	w, _ := window(b, 4, 4, "x")
	if cap(w) != 4 {
		t.Errorf("cap de la ventana = %d, esperaba 4 (recortada)", cap(w))
	}
}

func TestFileTimeIdaYVuelta(t *testing.T) {
	casos := []time.Time{
		time.Date(2026, 9, 27, 1, 2, 3, 456789100, time.UTC),
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2000, 2, 29, 12, 0, 0, 0, time.UTC),
	}
	for _, want := range casos {
		if got := fileTime(toFileTime(want)); !got.UTC().Equal(want) {
			t.Errorf("fileTime(toFileTime(%v)) = %v", want, got.UTC())
		}
	}
	if !fileTime(0).IsZero() {
		t.Error("FILETIME 0 debería ser la hora cero (desconocida)")
	}
	if !fileTime(-5).IsZero() {
		t.Error("un FILETIME negativo (basura) debería ser la hora cero")
	}
}

func TestUnicodeString(t *testing.T) {
	// Búfer: [UNICODE_STRING de 16 bytes][cadena UTF-16 "node.exe"].
	base := uintptr(0x1000)
	b := make([]byte, 16+16)
	s := "node.exe"
	binary.LittleEndian.PutUint16(b[0:], uint16(len(s)*2)) // Length en BYTES
	binary.LittleEndian.PutUint16(b[2:], uint16(len(s)*2)) // MaximumLength
	binary.LittleEndian.PutUint64(b[8:], uint64(base)+16)  // Buffer → offset 16
	for i, r := range s {
		binary.LittleEndian.PutUint16(b[16+2*i:], uint16(r))
	}
	got, err := unicodeString(b, base, 0, "nombre")
	if err != nil || got != "node.exe" {
		t.Errorf("unicodeString = %q, %v", got, err)
	}
}

func TestUnicodeStringVacia(t *testing.T) {
	// Length 0 con Buffer NULL es la cadena vacía (System Idle, entre otros).
	b := make([]byte, 16)
	got, err := unicodeString(b, 0x1000, 0, "x")
	if err != nil || got != "" {
		t.Errorf("cadena vacía: %q, %v", got, err)
	}
}

func TestUnicodeStringPunteroFuera(t *testing.T) {
	// Length dice 8 bytes pero Buffer apunta fuera del búfer: error, no pánico.
	base := uintptr(0x1000)
	b := make([]byte, 16)
	binary.LittleEndian.PutUint16(b[0:], 8)
	binary.LittleEndian.PutUint64(b[8:], uint64(base)+9000) // muy afuera
	if _, err := unicodeString(b, base, 0, "x"); !isMalformed(err) {
		t.Errorf("puntero fuera de rango: err = %v, esperaba ErrMalformed", err)
	}
}

func TestUnicodeStringLargoImpar(t *testing.T) {
	base := uintptr(0x1000)
	b := make([]byte, 16+4)
	binary.LittleEndian.PutUint16(b[0:], 3) // impar: imposible en UTF-16
	binary.LittleEndian.PutUint64(b[8:], uint64(base)+16)
	if _, err := unicodeString(b, base, 0, "x"); !isMalformed(err) {
		t.Errorf("largo impar: err = %v, esperaba ErrMalformed", err)
	}
}

func TestCString16(t *testing.T) {
	base := uintptr(0x2000)
	s := "MSSQLSERVER"
	b := make([]byte, len(s)*2+2)
	for i, r := range s {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(r))
	}
	// el terminador 0,0 ya está (b se creó con ceros al final)
	got, err := cString16(b, base, uint64(base), "servicio")
	if err != nil || got != s {
		t.Errorf("cString16 = %q, %v; esperaba %q", got, err, s)
	}
	// NULL → cadena vacía.
	if got, err := cString16(b, base, 0, "x"); err != nil || got != "" {
		t.Errorf("NULL: %q, %v", got, err)
	}
}

func TestCString16SinTerminador(t *testing.T) {
	// Sin 0,0 antes del final: error, no se sigue leyendo memoria ajena.
	base := uintptr(0x2000)
	b := []byte{'a', 0, 'b', 0} // "ab" sin terminador
	if _, err := cString16(b, base, uint64(base), "x"); !isMalformed(err) {
		t.Errorf("sin terminador: err = %v, esperaba ErrMalformed", err)
	}
}

func TestBe16OrdenDeRed(t *testing.T) {
	// El puerto 3000 = 0x0BB8; en orden de red los bytes son 0x0B, 0xB8.
	if p := be16([]byte{0x0B, 0xB8}, 0); p != 3000 {
		t.Errorf("be16 = %d, esperaba 3000", p)
	}
}

// toFileTime es la inversa exacta de fileTime para cualquier hora que haya
// salido de ella (la resolución de FILETIME es 100 ns). El cero vuelve a 0.
// Solo lo usan las pruebas (arman búferes crudos con horas).
func toFileTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()*10_000_000 + int64(t.Nanosecond())/100 + fileTimeEpoch
}
