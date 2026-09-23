//go:build windows

// Package win reúne las llamadas crudas a la API de Windows que comparten las
// herramientas: consola, portapapeles, procesos, tablas de red y servicios.
// Todo por syscall, sin CGO y sin dependencias externas.
package win

import (
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// kernel32 es una KnownDLL: Windows la carga siempre desde System32, así que
// se puede nombrar sin ruta. El resto se carga por ruta absoluta porque con el
// nombre pelado LoadLibrary busca primero junto al exe, y un DLL plantado ahí
// ganaría (DLL preloading).
var (
	modkernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemDirectoryW = modkernel32.NewProc("GetSystemDirectoryW")
)

var systemDirectory = func() string {
	buf := make([]uint16, 512)
	n, _, _ := syscall.SyscallN(procGetSystemDirectoryW.Addr(), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 || int(n) >= len(buf) {
		return `C:\Windows\System32`
	}
	return string(utf16.Decode(buf[:n]))
}()

func systemDLL(name string) *syscall.LazyDLL {
	return syscall.NewLazyDLL(systemDirectory + `\` + name)
}

var (
	moduser32   = systemDLL("user32.dll")
	modadvapi32 = systemDLL("advapi32.dll")
	modiphlpapi = systemDLL("iphlpapi.dll")
	modntdll    = systemDLL("ntdll.dll")
	modshell32  = systemDLL("shell32.dll")
)

// call invoca un procedimiento y devuelve el resultado crudo más el errno que
// dejó GetLastError. Quien llama decide qué valor de retorno es un fallo.
func call(p *syscall.LazyProc, args ...uintptr) (uintptr, syscall.Errno) {
	r1, _, e := syscall.SyscallN(p.Addr(), args...)
	return r1, e
}

// lastError convierte el errno en error, garantizando que nunca sea nil aunque
// la API haya fallado sin fijar GetLastError (pasa con varias de consola).
func lastError(e syscall.Errno) error {
	if e == 0 {
		return syscall.EINVAL
	}
	return e
}

// UTF16Ptr convierte a UTF-16 terminado en cero para pasar a la API W.
func UTF16Ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil { // s tiene un NUL embebido: se corta ahí, como haría C
		for i := 0; i < len(s); i++ {
			if s[i] == 0 {
				p, _ = syscall.UTF16PtrFromString(s[:i])
				break
			}
		}
	}
	return p
}

// UTF16ToString lee una cadena UTF-16 terminada en cero desde un puntero.
// maxUnits acota la lectura para no recorrer memoria ajena si falta el cero.
func UTF16ToString(p *uint16, maxUnits int) string {
	if p == nil {
		return ""
	}
	units := unsafe.Slice(p, maxUnits)
	for i, u := range units {
		if u == 0 {
			return string(utf16.Decode(units[:i]))
		}
	}
	return string(utf16.Decode(units))
}
