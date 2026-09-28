//go:build windows

package desk

import "unsafe"

// EnableVirtualTerminalProcessing es el bit del modo del handle de SALIDA
// que hace que la consola entienda las secuencias de escape (color, cursor).
const EnableVirtualTerminalProcessing = 0x0004

var (
	procGetConsoleMode             = modkernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = modkernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = modkernel32.NewProc("GetConsoleScreenBufferInfo")
)

type coord struct{ X, Y int16 }

type smallRect struct{ Left, Top, Right, Bottom int16 }

type consoleScreenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

// ConsoleMode lee el modo del handle; falla si el handle no es una consola
// (salida redirigida a archivo o pipe), que es justo como se detecta eso.
func ConsoleMode(h uintptr) (uint32, error) {
	var mode uint32
	if r, e := call(procGetConsoleMode, h, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return 0, lastError(e)
	}
	return mode, nil
}

// SetConsoleMode fija el modo del handle.
func SetConsoleMode(h uintptr, mode uint32) error {
	if r, e := call(procSetConsoleMode, h, uintptr(mode)); r == 0 {
		return lastError(e)
	}
	return nil
}

// ConsoleSize devuelve columnas y filas VISIBLES (la ventana, no el búfer de
// scroll, que suele medir 9001 filas).
func ConsoleSize(h uintptr) (cols, rows int, err error) {
	var info consoleScreenBufferInfo
	if r, e := call(procGetConsoleScreenBufferInfo, h, uintptr(unsafe.Pointer(&info))); r == 0 {
		return 0, 0, lastError(e)
	}
	return int(info.Window.Right-info.Window.Left) + 1, int(info.Window.Bottom-info.Window.Top) + 1, nil
}

// IsConsole dice si el handle es una consola.
func IsConsole(h uintptr) bool {
	_, err := ConsoleMode(h)
	return err == nil
}
