//go:build windows

package win

import (
	"syscall"
	"unsafe"
)

// Modos de consola que usan las herramientas.
const (
	EnableProcessedInput            = 0x0001
	EnableLineInput                 = 0x0002
	EnableEchoInput                 = 0x0004
	EnableVirtualTerminalProcessing = 0x0004 // mismo bit, pero en el handle de SALIDA
	CtrlCEvent                      = 0
	CtrlBreakEvent                  = 1
)

var (
	procGetConsoleMode             = modkernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = modkernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = modkernel32.NewProc("GetConsoleScreenBufferInfo")
	procGetConsoleProcessList      = modkernel32.NewProc("GetConsoleProcessList")
	procAttachConsole              = modkernel32.NewProc("AttachConsole")
	procFreeConsole                = modkernel32.NewProc("FreeConsole")
	procSetConsoleCtrlHandler      = modkernel32.NewProc("SetConsoleCtrlHandler")
	procGenerateConsoleCtrlEvent   = modkernel32.NewProc("GenerateConsoleCtrlEvent")
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

// ConsoleProcessList devuelve los PID adjuntos a la consola de este proceso.
// killport lo usa para no mandarle Ctrl+C a su propia terminal.
func ConsoleProcessList() ([]uint32, error) {
	pids := make([]uint32, 64)
	for {
		n, e := call(procGetConsoleProcessList, uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
		if n == 0 {
			return nil, lastError(e)
		}
		if int(n) <= len(pids) {
			return pids[:n], nil
		}
		pids = make([]uint32, n+16) // la lista creció entre llamadas
	}
}

// AttachConsole engancha este proceso a la consola de pid.
func AttachConsole(pid uint32) error {
	if r, e := call(procAttachConsole, uintptr(pid)); r == 0 {
		return lastError(e)
	}
	return nil
}

// FreeConsole suelta la consola actual (si había una).
func FreeConsole() error {
	if r, e := call(procFreeConsole); r == 0 {
		return lastError(e)
	}
	return nil
}

// IgnoreCtrlC hace que ESTE proceso ignore (true) o vuelva a atender (false)
// el Ctrl+C: SetConsoleCtrlHandler(NULL, TRUE/FALSE).
func IgnoreCtrlC(ignore bool) error {
	flag := uintptr(0)
	if ignore {
		flag = 1
	}
	if r, e := call(procSetConsoleCtrlHandler, 0, flag); r == 0 {
		return lastError(e)
	}
	return nil
}

// GenerateCtrlEvent manda Ctrl+C o Ctrl+Break a los procesos de la consola.
// Con group=0 le llega a TODOS los que comparten la consola del que llama.
func GenerateCtrlEvent(event, group uint32) error {
	if r, e := call(procGenerateConsoleCtrlEvent, uintptr(event), uintptr(group)); r == 0 {
		return lastError(e)
	}
	return nil
}

// IsConsole dice si el handle es una consola.
func IsConsole(h uintptr) bool {
	_, err := ConsoleMode(h)
	return err == nil
}

// ErrNotConsole se usa cuando una operación exige consola y no la hay.
var ErrNotConsole = syscall.Errno(6) // ERROR_INVALID_HANDLE
