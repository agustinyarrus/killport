//go:build windows

package win

// windows_close.go: cerrar ventanas de nivel superior de un proceso, como el
// aspa de la ventana. EnumWindows recorre TODAS las ventanas de nivel superior
// del escritorio; nos quedamos con las del PID buscado (GetWindowThreadProcessId)
// y les mandamos WM_CLOSE con PostMessage.
//
// PostMessage (no SendMessage): encola el mensaje y vuelve enseguida, sin
// bloquearse si la ventana no está atendiendo su cola. La app decide qué hacer
// con WM_CLOSE (guardar, preguntar, cerrar); es el cierre prolijo de un
// programa con interfaz gráfica, el equivalente al Ctrl+C de uno de consola.

import (
	"syscall"
	"unsafe"
)

var (
	procEnumWindows              = moduser32.NewProc("EnumWindows")
	procGetWindowThreadProcessId = moduser32.NewProc("GetWindowThreadProcessId")
	procPostMessageW             = moduser32.NewProc("PostMessageW")
	procIsWindowVisible          = moduser32.NewProc("IsWindowVisible")
)

const wmClose = 0x0010

// CloseWindows manda WM_CLOSE a las ventanas de nivel superior VISIBLES del
// proceso pid y devuelve cuántas recibieron el mensaje. Cero = el proceso no
// tiene ventanas propias (es de consola o de servicio): el motor sigue por
// otro peldaño. Solo las visibles, para no molestar ventanas-mensaje ocultas
// que algunos procesos usan para comunicarse.
func CloseWindows(pid uint32) (int, error) {
	var posted int
	var innerErr error
	// El callback corre en este mismo hilo mientras EnumWindows no vuelve, así
	// que puede cerrar sobre las variables de arriba sin sincronización.
	cb := syscall.NewCallback(func(hwnd, lparam uintptr) uintptr {
		var owner uint32
		call(procGetWindowThreadProcessId, hwnd, uintptr(unsafe.Pointer(&owner)))
		if owner != pid {
			return 1 // seguir enumerando
		}
		if r, _ := call(procIsWindowVisible, hwnd); r == 0 {
			return 1
		}
		if r, e := call(procPostMessageW, hwnd, wmClose, 0, 0); r == 0 {
			innerErr = lastError(e)
		} else {
			posted++
		}
		return 1
	})
	if r, e := call(procEnumWindows, cb, 0); r == 0 && posted == 0 {
		// EnumWindows devuelve 0 si el callback cortó la enumeración; acá el
		// callback nunca corta, así que un 0 con GetLastError real es un fallo.
		if e != 0 {
			return 0, lastError(e)
		}
	}
	return posted, innerErr
}
