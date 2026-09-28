//go:build windows

package win

// codepage.go: pasar a texto la salida de un comando de consola. netsh escribe
// en UTF-8 cuando su salida va a un pipe en los Windows actuales, y en la
// página de códigos OEM (850 en una máquina en español) en los anteriores.
// No hay forma de preguntárselo: se mira lo que mandó.

import (
	"unicode/utf8"
	"unsafe"
)

var procMultiByteToWideChar = modkernel32.NewProc("MultiByteToWideChar")

const cpOEMCP = 1 // CP_OEMCP: la página de códigos OEM del sistema

// ConsoleText decodifica la salida de un comando de consola: si es UTF-8
// válido se toma tal cual; si no, se la convierte desde la página de códigos
// OEM del sistema. Un texto en ASCII puro es las dos cosas y sale igual.
func ConsoleText(b []byte) string {
	if len(b) == 0 || utf8.Valid(b) {
		return string(b)
	}
	n, _ := call(procMultiByteToWideChar, cpOEMCP, 0, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 0, 0)
	if n == 0 {
		return string(b)
	}
	out := make([]uint16, n)
	m, _ := call(procMultiByteToWideChar, cpOEMCP, 0, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		uintptr(unsafe.Pointer(&out[0])), n)
	if m == 0 || m > n {
		return string(b)
	}
	return utf16Decode(out[:m])
}
