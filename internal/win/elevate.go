//go:build windows

package win

// elevate.go: saber si el proceso ya corre como administrador y relanzar un
// programa elevado (el diálogo del UAC) esperando su resultado.
//
// killport necesita esto porque cerrar procesos de otro usuario, detener
// servicios o tocar procesos de sesión 0 requiere el token de administrador.
// La estrategia es sacar los objetivos que necesitan permiso, y si los hay y no
// somos admin, relanzarse elevado para esa parte.

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

var (
	procGetCurrentProcess   = modkernel32.NewProc("GetCurrentProcess")
	procOpenProcessToken    = modadvapi32.NewProc("OpenProcessToken")
	procGetTokenInformation = modadvapi32.NewProc("GetTokenInformation")
	procShellExecuteExW     = modshell32.NewProc("ShellExecuteExW")
	procWaitForSingleObject = modkernel32.NewProc("WaitForSingleObject")
	procGetProcessId        = modkernel32.NewProc("GetProcessId")
)

const (
	tokenQuery     = 0x0008
	tokenElevation = 20 // TokenElevation (TOKEN_INFORMATION_CLASS)

	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	swHide                = 0
	swShowNormal          = 1

	errCancelled = 1223 // ERROR_CANCELLED: el usuario dijo que no en el UAC
)

// IsElevated dice si el proceso corre con el token de administrador (elevado).
// Abre el token propio y lee TokenElevation. Falso también si no se pudo saber
// (mejor asumir no-admin y pedir elevación que creer que se puede y fallar).
func IsElevated() bool {
	proc, _ := call(procGetCurrentProcess)
	var token uintptr
	if r, _ := call(procOpenProcessToken, proc, tokenQuery, uintptr(unsafe.Pointer(&token))); r == 0 {
		return false
	}
	defer closeHandle(token)
	var elevated uint32
	var ret uint32
	if r, _ := call(procGetTokenInformation, token, tokenElevation,
		uintptr(unsafe.Pointer(&elevated)), 4, uintptr(unsafe.Pointer(&ret))); r == 0 {
		return false
	}
	return elevated != 0
}

// shellExecuteInfo es SHELLEXECUTEINFOW. En amd64 mide 112 bytes; se verifica
// en un test con unsafe.Sizeof para que un cambio de layout no pase inadvertido.
// El relleno entre nShow/hInstApp y entre dwHotKey/hIcon lo pone Go solo al
// alinear los punteros a 8.
type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         uintptr
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     uintptr
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    uintptr
	dwHotKey     uint32
	hIcon        uintptr
	hProcess     uintptr
}

// RunResult es el desenlace de un ShellExecuteEx que esperó al proceso.
type RunResult struct {
	ExitCode uint32
}

// ErrUACDeclined: el usuario cerró/rechazó el diálogo del UAC.
var ErrUACDeclined = fmt.Errorf("se canceló el pedido de administrador")

// ShellExecute lanza exe con args a través de ShellExecuteExW y, si
// wait=true, espera a que termine y devuelve su código de salida. verb es
// "runas" para pedir elevación (UAC) o "open" para lanzar normal. hidden oculta
// la ventana. Los argumentos se citan con las reglas de CommandLineToArgvW
// (syscall.EscapeArg), las mismas que usa el sistema para volver a partirlos.
//
// El clic del UAC no se puede automatizar; con verb "runas" esta función deja
// todo listo y el rechazo del usuario vuelve como ErrUACDeclined. Con verb
// "open" se prueba de punta a punta (ver _oracle).
func ShellExecute(verb, exe string, args []string, hidden, wait bool) (RunResult, error) {
	h, err := shellExecute(verb, exe, args, hidden)
	if err != nil || h == 0 {
		// h == 0: sin SEE_MASK_NOCLOSEPROCESS efectivo (algún manejador
		// ligero), no hay a quién esperar. Éxito sin código.
		return RunResult{}, err
	}
	defer closeHandle(h)
	if !wait {
		return RunResult{}, nil
	}
	call(procWaitForSingleObject, h, infinite)
	var code uint32
	call(procGetExitCodeProcess, h, uintptr(unsafe.Pointer(&code)))
	return RunResult{ExitCode: code}, nil
}

// ShellExecuteProcess es ShellExecute sin esperar: devuelve el handle del
// proceso lanzado, para esperarlo con WaitAny (cancelable con el Ctrl+C del
// usuario, a diferencia de un WaitForSingleObject INFINITE) y leer su código
// con ExitCode. CanTerminate queda en false: el handle que entrega el sistema
// para un proceso elevado no trae permiso de terminarlo, y killport tampoco lo
// necesita. nil sin error si el sistema no devolvió proceso. Quien llama lo
// cierra con Close.
func ShellExecuteProcess(verb, exe string, args []string, hidden bool) (*Handle, error) {
	h, err := shellExecute(verb, exe, args, hidden)
	if err != nil || h == 0 {
		return nil, err
	}
	pid, _ := call(procGetProcessId, h)
	return &Handle{PID: uint32(pid), Created: handleCreated(h), h: h}, nil
}

// shellExecute arma SHELLEXECUTEINFOW y lanza. Devuelve el handle del proceso
// (0 si el sistema no dio uno). SEE_MASK_NOASYNC: la llamada vuelve recién
// cuando el lanzamiento terminó (incluido el diálogo del UAC).
func shellExecute(verb, exe string, args []string, hidden bool) (uintptr, error) {
	params := make([]string, len(args))
	for i, a := range args {
		params[i] = syscall.EscapeArg(a)
	}
	show := int32(swShowNormal)
	if hidden {
		show = swHide
	}
	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		lpVerb:       UTF16Ptr(verb),
		lpFile:       UTF16Ptr(exe),
		lpParameters: UTF16Ptr(strings.Join(params, " ")),
		nShow:        show,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))

	if r, e := call(procShellExecuteExW, uintptr(unsafe.Pointer(&info))); r == 0 {
		if e == errCancelled {
			return 0, ErrUACDeclined
		}
		return 0, fmt.Errorf("no pude lanzar %s: %w", exe, lastError(e))
	}
	return info.hProcess, nil
}
