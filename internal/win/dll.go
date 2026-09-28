//go:build windows

// Package win reúne las llamadas crudas a la API de Windows de killport: la
// consola de OTRO proceso (engancharse, mandarle Ctrl+C), procesos, tablas de
// red, servicios, el ejecutor acotado de comandos externos (Job Object, sin
// ventana, con plazo) y el cliente de named pipes. Lo que usan todas las
// herramientas (el modo y el tamaño de la consola propia, el portapapeles)
// vive en win/desk, para que una herramienta que solo pinta no cargue con
// esto. Todo por syscall, sin CGO y sin dependencias externas.
package win

import (
	"errors"
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
//
// La directiva uintptrescapes NO es decorativa. Todo el paquete pasa
// direcciones como call(p, uintptr(unsafe.Pointer(&buf[0]))). Sin ella, el
// análisis de escape deja en la PILA de la goroutine los búferes chicos de
// tamaño fijo (un [1024]uint16, un uint32 de salida), porque un uintptr no
// cuenta como puntero; y si la pila crece y se copia entre la conversión y la
// llamada al sistema (en el prólogo de call o de SyscallN), Windows escribe
// en la pila VIEJA. Medido: GetFinalPathNameByHandleW "tenía éxito" y el
// búfer volvía vacío, justo en la primera llamada de una goroutine nueva. Con
// la directiva, el compilador trata cada argumento uintptr de call como un
// puntero convertido: lo manda al heap (que no se mueve) y lo mantiene vivo
// hasta que call vuelve. Es lo mismo que hace syscall.(*LazyProc).Call.
//
//go:uintptrescapes
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

// errnoOf recupera el código numérico de Windows de un error que salió de
// lastError, para poder ramificar por código sin comparar strings.
func errnoOf(err error) (syscall.Errno, bool) {
	var e syscall.Errno
	if errors.As(err, &e) {
		return e, true
	}
	return 0, false
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
