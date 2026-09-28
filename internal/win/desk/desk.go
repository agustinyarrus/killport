//go:build windows

// Package desk es lo poco de la API de Windows que usan TODAS las
// herramientas: el modo y el tamaño de la consola (tui) y el portapapeles
// (clip2qr). Vive aparte de internal/win a propósito: win creció con lo de
// killport (tablas de red con net/netip, el cliente de named pipes, el
// ejecutor acotado con os/exec), y una herramienta que solo pinta en la
// consola no tiene por qué cargar con eso (medido en la revisión: ~105 KB de
// más en clip2qr, img y pdf-merge). Todo por syscall, sin CGO.
package desk

import (
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// kernel32 es una KnownDLL: Windows la carga siempre desde System32, así que
// se puede nombrar sin ruta. user32 se carga por ruta absoluta (con el nombre
// pelado, LoadLibrary busca primero junto al exe: DLL preloading).
var (
	modkernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemDirectoryW = modkernel32.NewProc("GetSystemDirectoryW")
	moduser32               = syscall.NewLazyDLL(systemDirectory() + `\user32.dll`)
)

func systemDirectory() string {
	buf := make([]uint16, 512)
	n, _, _ := syscall.SyscallN(procGetSystemDirectoryW.Addr(), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 || int(n) >= len(buf) {
		return `C:\Windows\System32`
	}
	return string(utf16.Decode(buf[:n]))
}

// call invoca un procedimiento y devuelve el resultado crudo más el errno.
// La directiva uintptrescapes lleva al heap los búferes que se pasan como
// uintptr (ver la explicación larga en internal/win/dll.go: sin ella, un
// búfer chico en la pila puede moverse entre la conversión y la llamada).
//
//go:uintptrescapes
func call(p *syscall.LazyProc, args ...uintptr) (uintptr, syscall.Errno) {
	r1, _, e := syscall.SyscallN(p.Addr(), args...)
	return r1, e
}

// lastError convierte el errno en error, nunca nil aunque la API haya fallado
// sin fijar GetLastError (pasa con varias de consola).
func lastError(e syscall.Errno) error {
	if e == 0 {
		return syscall.EINVAL
	}
	return e
}
