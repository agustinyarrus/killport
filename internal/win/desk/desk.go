//go:build windows

// Package desk es lo poco de la API de Windows que necesita tui para pintar:
// el modo y el tamaño de la consola propia. Vive aparte de internal/win a
// propósito: win es la capa de sistema de killport (tablas de red con
// net/netip, el cliente de named pipes, el ejecutor acotado con os/exec) y la
// presentación no tiene por qué depender de ella. Todo por syscall, sin CGO.
package desk

import "syscall"

// kernel32 es una KnownDLL: Windows la carga siempre desde System32, así que
// se puede nombrar sin ruta (con cualquier otra DLL, LoadLibrary buscaría
// primero junto al exe: DLL preloading; ver internal/win/dll.go).
var modkernel32 = syscall.NewLazyDLL("kernel32.dll")

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
