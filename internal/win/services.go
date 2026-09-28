//go:build windows

package win

// services.go: enumerar los servicios de Windows y detener uno. Importa para
// killport porque un puerto puede estar en manos de un servicio (SQL Server,
// PostgreSQL, Docker): matar el proceso no alcanza, el administrador de
// servicios lo revive por su política de recuperación. Hay que DETENER el
// servicio.
//
// Además, un svchost.exe aloja varios servicios de Windows a la vez: el PID no
// dice cuál tiene el puerto. El mapa PID → servicios de acá, cruzado con el
// nombre que devuelve Socket.Owner (que en un svchost ES el nombre del
// servicio), permite nombrar al culpable exacto sin matar el host compartido.

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	procOpenSCManagerW         = modadvapi32.NewProc("OpenSCManagerW")
	procEnumServicesStatusExW  = modadvapi32.NewProc("EnumServicesStatusExW")
	procOpenServiceW           = modadvapi32.NewProc("OpenServiceW")
	procControlService         = modadvapi32.NewProc("ControlService")
	procQueryServiceStatusEx   = modadvapi32.NewProc("QueryServiceStatusEx")
	procCloseServiceHandle     = modadvapi32.NewProc("CloseServiceHandle")
	procEnumDependentServicesW = modadvapi32.NewProc("EnumDependentServicesW")
)

const (
	scManagerConnect           = 0x0001
	scManagerEnumerate         = 0x0004
	serviceQueryStatus         = 0x0004
	serviceStop                = 0x0020
	serviceEnumerateDependents = 0x0008
	serviceWin32               = 0x00000030 // SERVICE_WIN32_OWN_PROCESS | SHARE_PROCESS
	serviceActive              = 0x00000001 // SERVICE_ACTIVE
	scEnumProcessInfo          = 0          // SC_ENUM_PROCESS_INFO
	scStatusProcessInfo        = 0          // SC_STATUS_PROCESS_INFO

	serviceControlStop = 0x00000001
	serviceStopped     = 0x00000001
	serviceStopPending = 0x00000003
	serviceRunning     = 0x00000004

	errServiceNotActive       = 1062 // ERROR_SERVICE_NOT_ACTIVE: ya estaba detenido
	errDependentServices      = 1051 // ERROR_DEPENDENT_SERVICES_RUNNING
	errServiceDoesNotExist    = 1060 // ERROR_SERVICE_DOES_NOT_EXIST
	errServiceCannotStop      = 1052 // ERROR_INVALID_SERVICE_CONTROL / no acepta STOP
	errServiceRequestTimeout  = 1053
	errServiceMarkedForDelete = 1072
)

// Service es un servicio activo y el proceso que lo aloja.
type Service struct {
	Name        string // nombre del servicio (el de "net stop NOMBRE")
	DisplayName string // nombre visible
	PID         uint32 // proceso que lo aloja (0 si no está corriendo)
	Shared      bool   // true si comparte proceso (svchost) con otros servicios
	State       uint32 // SERVICE_RUNNING, STOP_PENDING, …
}

// ENUM_SERVICE_STATUS_PROCESSW en 64 bits: dos punteros a cadena (que apuntan
// dentro del mismo búfer) y una SERVICE_STATUS_PROCESS embebida.
// ENUM_SERVICE_STATUS_PROCESSW (0x38 bytes en x64): dos punteros y una
// SERVICE_STATUS_PROCESS embebida (9 DWORD). Offsets verificados contra
// Win32_Service (ProcessId, State): dwProcessId es el 8º DWORD, en 0x2C.
const (
	essServiceName  = 0x00 // LPWSTR lpServiceName
	essDisplayName  = 0x08 // LPWSTR lpDisplayName
	essServiceType  = 0x10 // SERVICE_STATUS_PROCESS.dwServiceType
	essCurrentState = 0x14 // dwCurrentState
	// 0x18 dwControlsAccepted · 0x1C dwWin32ExitCode · 0x20 specific ·
	// 0x24 dwCheckPoint · 0x28 dwWaitHint
	essPID            = 0x2C // dwProcessId (¡no 0x28: ese es dwWaitHint!)
	essEntrySize      = 0x38 // + 0x30 dwServiceFlags, y relleno a 8 por los punteros
	serviceTypeShared = 0x20 // SERVICE_WIN32_SHARE_PROCESS
)

// Services devuelve los servicios activos con el PID que los aloja.
// EnumServicesStatusExW llena un búfer con la lista; se hace crecer con
// ERROR_MORE_DATA (234), igual que las tablas de sockets.
func Services() ([]Service, error) {
	return servicesFrom(64 * 1024)
}

// servicesFrom es Services con el tamaño inicial del búfer a elección (las
// pruebas arrancan con uno chico para recorrer el bucle de crecimiento).
//
// Trampa: con ERROR_MORE_DATA, pcbBytesNeeded NO es el total sino lo que
// falta para las entradas que no entraron (las RESTANTES). Tomarlo como el
// total hace oscilar el tamaño (grande, chico, grande…) y rendirse a los 6
// intentos: pasaba con unos 450 servicios activos o más. El total está
// acotado por lo que ya se ofreció más lo que falta, así que el búfer nuevo
// es el anterior + lo que falta + holgura (por si la lista crece entre una
// llamada y otra). Cada llamada arranca de cero (resume = 0): un búfer
// entero, sin juntar pedazos de listas tomadas en instantes distintos.
func servicesFrom(size uint32) ([]Service, error) {
	scm, e := call(procOpenSCManagerW, 0, 0, scManagerConnect|scManagerEnumerate)
	if scm == 0 {
		return nil, fmt.Errorf("no pude abrir el administrador de servicios: %w", lastError(e))
	}
	defer call(procCloseServiceHandle, scm)

	for attempt := 0; attempt < 6; attempt++ {
		buf := alignedBuffer(int(size))
		var needed, returned, resume uint32
		r, e := call(procEnumServicesStatusExW, scm, scEnumProcessInfo, serviceWin32, serviceActive,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
			uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&returned)),
			uintptr(unsafe.Pointer(&resume)), 0)
		if r != 0 {
			return parseServices(buf, uintptr(unsafe.Pointer(&buf[0])), int(returned))
		}
		if e == 234 { // ERROR_MORE_DATA
			size = uint32(len(buf)) + needed + needed/8 + 4096
			continue
		}
		return nil, fmt.Errorf("no pude enumerar los servicios: %w", lastError(e))
	}
	return nil, fmt.Errorf("no pude enumerar los servicios: la lista no dejó de crecer")
}

// parseServices lee las `count` entradas del búfer. Cada una es de tamaño fijo
// (essEntrySize); los nombres son punteros a cadenas terminadas en cero en el
// mismo búfer.
func parseServices(b []byte, base uintptr, count int) ([]Service, error) {
	out := make([]Service, 0, count)
	for i := 0; i < count; i++ {
		off := i * essEntrySize
		e, err := window(b, off, essEntrySize, "servicio")
		if err != nil {
			return nil, err
		}
		name, err := cString16(b, base, le64(e, essServiceName), "servicio: nombre")
		if err != nil {
			return nil, err
		}
		disp, err := cString16(b, base, le64(e, essDisplayName), "servicio: nombre visible")
		if err != nil {
			return nil, err
		}
		out = append(out, Service{
			Name:        name,
			DisplayName: disp,
			PID:         le32(e, essPID),
			Shared:      le32(e, essServiceType)&serviceTypeShared != 0,
			State:       le32(e, essCurrentState),
		})
	}
	return out, nil
}

// Errores distinguibles al detener un servicio.
var (
	ErrServiceNotFound   = fmt.Errorf("no existe un servicio con ese nombre")
	ErrServiceDependents = fmt.Errorf("hay servicios que dependen de este y están activos")
	ErrServiceTimeout    = fmt.Errorf("el servicio no terminó de detenerse a tiempo")
)

// DependentsError es ErrServiceDependents con los nombres de los servicios
// activos que dependen del que se quiso detener (errors.Is(err,
// ErrServiceDependents) sigue valiendo). Names vacío si no se pudieron leer.
type DependentsError struct{ Names []string }

func (e *DependentsError) Error() string {
	if len(e.Names) == 0 {
		return ErrServiceDependents.Error()
	}
	return ErrServiceDependents.Error() + ": " + strings.Join(e.Names, ", ")
}

func (e *DependentsError) Is(target error) bool { return target == ErrServiceDependents }

// StopService detiene el servicio name y espera hasta deadline a que quede en
// STOPPED, guiándose por dwWaitHint que informa el propio servicio (no un
// sleep fijo). Se corta al instante si se cancela ctx. Errores distinguibles:
//   - ErrAccessDenied: hace falta administrador;
//   - ErrServiceNotFound: no existe;
//   - *DependentsError (errors.Is ErrServiceDependents): otros servicios
//     activos dependen de este, con sus nombres;
//   - ErrServiceTimeout: venció el plazo sin llegar a STOPPED;
//   - ctx.Err() si se canceló.
//
// Si ya estaba detenido, devuelve nil (idempotente).
func StopService(ctx context.Context, name string, deadline time.Time) error {
	scm, e := call(procOpenSCManagerW, 0, 0, scManagerConnect)
	if scm == 0 {
		return fmt.Errorf("no pude abrir el administrador de servicios: %w", scmError(e))
	}
	defer call(procCloseServiceHandle, scm)

	svc, e := call(procOpenServiceW, scm, uintptr(unsafe.Pointer(UTF16Ptr(name))), serviceStop|serviceQueryStatus)
	if svc == 0 {
		return openServiceError(e)
	}
	defer call(procCloseServiceHandle, svc)

	// SERVICE_STATUS (28 bytes) que llena ControlService.
	var status [7]uint32
	if r, e := call(procControlService, svc, serviceControlStop, uintptr(unsafe.Pointer(&status[0]))); r == 0 {
		switch e {
		case errServiceNotActive:
			return nil // ya estaba detenido
		case errDependentServices:
			return &DependentsError{Names: activeDependents(scm, name)}
		case 5:
			return ErrAccessDenied
		default:
			return fmt.Errorf("no pude detener %q: %w", name, lastError(e))
		}
	}
	return waitService(ctx, func() (uint32, uint32, error) { return queryServiceState(svc) }, deadline,
		func(st uint32) (bool, error) { return st == serviceStopped, nil })
}

// waitService consulta el estado de un servicio hasta que settled dice que
// llegó a donde se lo mandó (o que no va a llegar, con el error) o vence el
// plazo. Es la espera de StopService y de StartService. El intervalo entre
// consultas sale de dwWaitHint (acotado a [100 ms, 2 s]), como recomienda la
// documentación de ControlService: es el propio servicio el que dice cuánto
// puede tardar. El SCM no ofrece un aviso de "ya se detuvo" que se pueda
// esperar junto con un evento (NotifyServiceStatusChange entrega el aviso por
// APC, que un hilo de Go no puede atender de forma confiable), así que se
// consulta. Es una de las dos esperas por consulta de killport; la otra es la
// de --wait, que relee la tabla de sockets (reaper/watch.go dice por qué). La
// pausa entre consultas escucha ctx, así que un Ctrl+C no espera nada. query
// se inyecta para probar el bucle sin un servicio de verdad.
func waitService(ctx context.Context, query func() (state, waitHint uint32, err error), deadline time.Time, settled func(state uint32) (bool, error)) error {
	for {
		st, waitHint, err := query()
		if err != nil {
			return err
		}
		if done, err := settled(st); done || err != nil {
			return err
		}
		until := time.Until(deadline)
		if until <= 0 {
			return ErrServiceTimeout
		}
		pause := min(max(time.Duration(waitHint)*time.Millisecond, 100*time.Millisecond), 2*time.Second, until)
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// activeDependents lista los servicios ACTIVOS que dependen de name, para
// decir cuáles impiden detenerlo (lo que muestra Get-Service -DependentServices).
// Abre un handle aparte con solo SERVICE_ENUMERATE_DEPENDENTS, que suele
// concederse aun sin administrador. Best-effort: nil si no se pudo.
func activeDependents(scm uintptr, name string) []string {
	svc, _ := call(procOpenServiceW, scm, uintptr(unsafe.Pointer(UTF16Ptr(name))), serviceEnumerateDependents)
	if svc == 0 {
		return nil
	}
	defer call(procCloseServiceHandle, svc)
	var needed, count uint32
	call(procEnumDependentServicesW, svc, serviceActive, 0, 0, uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count)))
	if needed == 0 {
		return nil
	}
	buf := alignedBuffer(int(needed))
	if r, _ := call(procEnumDependentServicesW, svc, serviceActive, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&count))); r == 0 {
		return nil
	}
	names, err := parseDependents(buf, uintptr(unsafe.Pointer(&buf[0])), int(count))
	if err != nil {
		return nil
	}
	return names
}

// ENUM_SERVICE_STATUSW en x64: dos punteros a cadena y una SERVICE_STATUS
// (7 DWORD = 28 bytes) desde 0x10: 44 bytes, rellenados a 48 por la
// alineación de los punteros.
const (
	esNameOffset = 0x00
	esEntrySize  = 0x30
)

// parseDependents lee los nombres de las count entradas de ENUM_SERVICE_STATUSW.
func parseDependents(b []byte, base uintptr, count int) ([]string, error) {
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		e, err := window(b, i*esEntrySize, esEntrySize, "servicio dependiente")
		if err != nil {
			return nil, err
		}
		name, err := cString16(b, base, le64(e, esNameOffset), "servicio dependiente: nombre")
		if err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, nil
}

// queryServiceState lee el estado actual y el dwWaitHint del servicio.
func queryServiceState(svc uintptr) (state, waitHint uint32, err error) {
	// SERVICE_STATUS_PROCESS: dwServiceType, dwCurrentState, dwControlsAccepted,
	// dwWin32ExitCode, dwServiceSpecificExitCode, dwCheckPoint, dwWaitHint,
	// dwProcessId, dwServiceFlags (9 DWORD).
	var s [9]uint32
	var needed uint32
	if r, e := call(procQueryServiceStatusEx, svc, scStatusProcessInfo,
		uintptr(unsafe.Pointer(&s[0])), uintptr(len(s)*4), uintptr(unsafe.Pointer(&needed))); r == 0 {
		return 0, 0, fmt.Errorf("no pude consultar el estado del servicio: %w", lastError(e))
	}
	return s[1], s[6], nil
}

func scmError(e syscall.Errno) error {
	if e == 5 {
		return ErrAccessDenied
	}
	return lastError(e)
}

func openServiceError(e syscall.Errno) error {
	switch e {
	case errServiceDoesNotExist:
		return ErrServiceNotFound
	case 5:
		return ErrAccessDenied
	default:
		return fmt.Errorf("no pude abrir el servicio: %w", lastError(e))
	}
}
