//go:build windows

package win

import (
	"syscall"
	"unsafe"
)

// Los eventos de consola que se mandan (GenerateConsoleCtrlEvent). El modo y
// el tamaño de la consola, que usan todas las herramientas, viven en
// internal/win/desk.
const (
	CtrlCEvent     = 0
	CtrlBreakEvent = 1
)

var (
	procGetConsoleProcessList    = modkernel32.NewProc("GetConsoleProcessList")
	procAttachConsole            = modkernel32.NewProc("AttachConsole")
	procFreeConsole              = modkernel32.NewProc("FreeConsole")
	procSetConsoleCtrlHandler    = modkernel32.NewProc("SetConsoleCtrlHandler")
	procGenerateConsoleCtrlEvent = modkernel32.NewProc("GenerateConsoleCtrlEvent")
)

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

// ErrNotConsole se usa cuando una operación exige consola y no la hay.
var ErrNotConsole = syscall.Errno(6) // ERROR_INVALID_HANDLE
