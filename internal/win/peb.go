package win

// peb.go: la carpeta de trabajo de OTRO proceso, leída de su memoria. Sin
// build tag: la cadena de lecturas es pura sobre una función que lee memoria,
// y se prueba con una memoria falsa armada byte a byte.
//
// Windows no tiene una API para preguntar la carpeta actual de otro proceso.
// Está en su PEB (Process Environment Block), en los parámetros con los que
// arrancó y que SetCurrentDirectory actualiza:
//
//	PEB ──ProcessParameters──▶ RTL_USER_PROCESS_PARAMETERS
//	                             └─ CurrentDirectory.DosPath: UNICODE_STRING ──Buffer──▶ "C:\dev\shop\"
//
// Son tres lecturas chicas con ReadProcessMemory (lo mismo que hace Process
// Explorer), con PROCESS_QUERY_LIMITED_INFORMATION | PROCESS_VM_READ: alcanza
// para los procesos del mismo usuario, sin administrador (verificado).
//
// Un proceso de 32 bits en Windows de 64 (WOW64) tiene DOS PEB: el nativo y el
// de 32 bits, que es el que su código usa y actualiza. Manda el de 32 bits
// (ProcessWow64Information devuelve su dirección; 0 si no es WOW64).
//
// La memoria es ajena y puede cambiar mientras se lee (el proceso hace chdir
// en ese instante, o termina): cada valor se valida antes de seguirlo, y
// cualquier cosa rara es "no se pudo", nunca una ruta inventada.

import (
	"strings"
)

// pebLayout es dónde está cada cosa según los bits del proceso.
type pebLayout struct {
	params int // desplazamiento de ProcessParameters en el PEB
	ptr    int // tamaño de un puntero
	curdir int // desplazamiento de CurrentDirectory.DosPath en los parámetros
	ustr   int // tamaño de un UNICODE_STRING
	buffer int // desplazamiento de Buffer dentro del UNICODE_STRING
}

var (
	// peb64: PEB+0x20 → RTL_USER_PROCESS_PARAMETERS+0x38, UNICODE_STRING de
	// 16 bytes (Length, MaximumLength, 4 de relleno, Buffer de 8).
	peb64 = pebLayout{params: 0x20, ptr: 8, curdir: 0x38, ustr: 16, buffer: 8}
	// peb32: PEB32+0x10 → RTL_USER_PROCESS_PARAMETERS32+0x24, UNICODE_STRING32
	// de 8 bytes (Length, MaximumLength, Buffer de 4).
	peb32 = pebLayout{params: 0x10, ptr: 4, curdir: 0x24, ustr: 8, buffer: 4}
)

// maxPathBytes es el tope de una ruta de Windows en bytes UTF-16 (32.767
// caracteres): un Length mayor es basura.
const maxPathBytes = 32767 * 2

// memReader lee n bytes de la memoria del otro proceso a partir de addr.
type memReader func(addr uint64, n int) ([]byte, error)

// readCwd sigue la cadena PEB → parámetros → carpeta actual con el layout
// dado y devuelve la ruta tal cual está (con la barra final). Pura.
func readCwd(read memReader, peb uint64, l pebLayout) (string, error) {
	if peb == 0 {
		return "", malformed("el proceso no tiene PEB")
	}
	b, err := read(peb+uint64(l.params), l.ptr)
	if err != nil {
		return "", err
	}
	params := readPtr(b, l.ptr)
	if params == 0 {
		return "", malformed("el PEB no tiene parámetros de proceso")
	}
	us, err := read(params+uint64(l.curdir), l.ustr)
	if err != nil {
		return "", err
	}
	length, maxLength := int(le16(us, 0)), int(le16(us, 2))
	buf := readPtr(us[l.buffer:], l.ptr)
	switch {
	case length == 0:
		return "", malformed("la carpeta actual está vacía")
	case length%2 != 0 || length > maxLength || length > maxPathBytes:
		return "", malformed("carpeta actual con largo imposible (%d de %d bytes)", length, maxLength)
	case buf == 0:
		return "", malformed("la carpeta actual no tiene búfer")
	}
	s, err := read(buf, length)
	if err != nil {
		return "", err
	}
	return utf16le(s), nil
}

// readPtr lee un puntero de n bytes (4 u 8), little-endian.
func readPtr(b []byte, n int) uint64 {
	if n == 4 {
		return uint64(le32(b, 0))
	}
	return le64(b, 0)
}

// TrimLongPrefix le quita a una ruta el prefijo de ruta larga:
// "\\?\C:\x" → "C:\x", "\\?\UNC\srv\share" → "\\srv\share". Es la de todo win
// (la carpeta de trabajo de otro proceso, la ruta final de un archivo);
// reaper.UnderDir, que no depende de win, aplica la misma regla al comparar.
func TrimLongPrefix(p string) string {
	if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
		return `\\` + rest
	}
	return strings.TrimPrefix(p, `\\?\`)
}

// CleanDir normaliza una carpeta como la guarda Windows: sin la barra final
// (salvo en la raíz de una unidad, "C:\") y sin el prefijo de ruta larga.
func CleanDir(p string) string {
	p = TrimLongPrefix(p)
	if len(p) > 3 || (len(p) == 3 && p[1] != ':') {
		p = strings.TrimRight(p, `\/`)
	}
	return p
}
