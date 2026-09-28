package win

// rawbuf.go: lectura defensiva de los búferes crudos que llena la API nativa.
// Sin build tag: es aritmética pura sobre []byte y se prueba en cualquier
// plataforma con bytes sintéticos.
//
// Todo lo que viene del sistema se trata como dato no confiable. La invariante
// de este archivo y de los parsers que lo usan:
//
//	ninguna lectura ocurre sin que antes se haya validado que la ventana
//	[desplazamiento, desplazamiento+largo) cabe entera en el búfer.
//
// Así un búfer truncado o corrupto termina en ErrMalformed, nunca en un pánico
// ni en una lectura fuera de rango.
//
// Los punteros que el sistema escribe DENTRO del mismo búfer (el Buffer de un
// UNICODE_STRING, los nombres de un servicio, el módulo dueño de un socket) no
// se reinterpretan como punteros de Go: se les resta la dirección base del
// búfer y se leen como un desplazamiento más, con la misma validación. Es la
// filosofía del portapapeles (copiar, nunca convertir uintptr → unsafe.Pointer)
// llevada a los parsers: si algún día el búfer se moviera, el desplazamiento
// caería afuera y daría error, no memoria ajena.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
	"unicode/utf16"
)

// ErrMalformed indica que un búfer devuelto por el sistema no respeta el
// layout esperado (truncado, desplazamientos que se salen, largos imposibles).
var ErrMalformed = errors.New("el sistema devolvió datos con un formato inesperado")

// malformed arma el error con el detalle de qué no cuadró.
func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMalformed, fmt.Sprintf(format, args...))
}

// window devuelve b[off:off+n] con capacidad recortada (nadie puede extenderla
// sobre los datos vecinos), o un error si no cabe. Es el único lugar donde se
// valida un rango: los lectores de campos trabajan sobre ventanas ya validadas.
func window(b []byte, off, n int, what string) ([]byte, error) {
	if off < 0 || n < 0 || off > len(b) || n > len(b)-off {
		return nil, malformed("%s: se pide [%d, %d) y el búfer mide %d bytes", what, off, off+n, len(b))
	}
	return b[off : off+n : off+n], nil
}

func le16(b []byte, off int) uint16 { return binary.LittleEndian.Uint16(b[off:]) }
func le32(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }
func le64(b []byte, off int) uint64 { return binary.LittleEndian.Uint64(b[off:]) }

// be16 lee un entero de 16 bits en orden de red (big-endian): así guarda el
// sistema los puertos en las tablas de sockets.
func be16(b []byte, off int) uint16 { return binary.BigEndian.Uint16(b[off:]) }

// pointee resuelve un puntero nativo que apunta DENTRO de b (cuya dirección de
// inicio es base) a la ventana de n bytes que empieza ahí.
func pointee(b []byte, base uintptr, ptr uint64, n int, what string) ([]byte, error) {
	if ptr < uint64(base) {
		return nil, malformed("%s: el puntero 0x%x apunta antes del búfer (0x%x)", what, ptr, base)
	}
	off := ptr - uint64(base)
	if off > uint64(len(b)) {
		return nil, malformed("%s: el puntero 0x%x apunta después del búfer (0x%x + %d)", what, ptr, base, len(b))
	}
	return window(b, int(off), n, what)
}

// utf16le decodifica UTF-16 little-endian (los sustitutos sueltos pasan a
// U+FFFD, como en el resto de la biblioteca estándar).
func utf16le(b []byte) string {
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = le16(b, 2*i)
	}
	return string(utf16.Decode(units))
}

// utf16Decode decodifica una porción de unidades UTF-16 (ya en el orden de la
// máquina) a string.
func utf16Decode(units []uint16) string { return string(utf16.Decode(units)) }

// unicodeStringSize es sizeof(UNICODE_STRING) en 64 bits:
// USHORT Length, USHORT MaximumLength, 4 de relleno, PWSTR Buffer.
const unicodeStringSize = 16

// unicodeString lee el UNICODE_STRING que empieza en off. Length está en BYTES
// (no en caracteres) y la cadena NO termina en cero: se respeta Length y nada
// más. Length 0 es la cadena vacía aunque Buffer sea NULL.
func unicodeString(b []byte, base uintptr, off int, what string) (string, error) {
	us, err := window(b, off, unicodeStringSize, what)
	if err != nil {
		return "", err
	}
	length := int(le16(us, 0))
	if length == 0 {
		return "", nil
	}
	if length%2 != 0 {
		return "", malformed("%s: largo impar (%d bytes) en una cadena UTF-16", what, length)
	}
	s, err := pointee(b, base, le64(us, 8), length, what)
	if err != nil {
		return "", err
	}
	return utf16le(s), nil
}

// cString16 lee una cadena UTF-16 terminada en cero apuntada por ptr (dentro
// de b). NULL es la cadena vacía. Si no hay terminador antes del final del
// búfer, es un error: no se sigue leyendo.
func cString16(b []byte, base uintptr, ptr uint64, what string) (string, error) {
	if ptr == 0 {
		return "", nil
	}
	// Ventana de largo 0: valida que el inicio caiga dentro del búfer. Desde
	// ahí se busca el terminador sin salir de b.
	if _, err := pointee(b, base, ptr, 0, what); err != nil {
		return "", err
	}
	rest := b[ptr-uint64(base):]
	for i := 0; i+1 < len(rest); i += 2 {
		if rest[i] == 0 && rest[i+1] == 0 {
			return utf16le(rest[:i]), nil
		}
	}
	return "", malformed("%s: la cadena no termina antes del final del búfer", what)
}

// fileTimeEpoch es 1970-01-01 medido en FILETIME: intervalos de 100 ns desde
// el 1601-01-01 UTC, la época de Windows.
const fileTimeEpoch = 116_444_736_000_000_000

// fileTime convierte ticks de FILETIME a time.Time sin pasar por un producto
// que pueda desbordar int64. Cero (o negativo: basura) es "desconocido", que
// en Go es el time.Time cero.
func fileTime(ticks int64) time.Time {
	if ticks <= 0 {
		return time.Time{}
	}
	u := ticks - fileTimeEpoch
	return time.Unix(u/10_000_000, (u%10_000_000)*100)
}
