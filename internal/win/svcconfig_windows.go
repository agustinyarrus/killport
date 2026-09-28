//go:build windows

package win

// svcconfig_windows.go: leer la configuración de un servicio, dejarlo en
// inicio manual, arrancarlo y consultar su estado, todo POR NOMBRE. Por
// nombre se llega también a los controladores del kernel (winnat, HTTP), que
// no aparecen en la enumeración de servicios de Win32.
//
// Leer no pide administrador (SERVICE_QUERY_CONFIG se le concede a cualquier
// usuario: es lo que hace `sc qc`). Cambiar, arrancar y detener, sí.

import (
	"context"
	"fmt"
	"time"
	"unsafe"
)

var (
	procQueryServiceConfigW  = modadvapi32.NewProc("QueryServiceConfigW")
	procQueryServiceConfig2W = modadvapi32.NewProc("QueryServiceConfig2W")
	procChangeServiceConfigW = modadvapi32.NewProc("ChangeServiceConfigW")
	procStartServiceW        = modadvapi32.NewProc("StartServiceW")
)

const (
	serviceQueryConfig  = 0x0001
	serviceChangeConfig = 0x0002
	serviceStart        = 0x0010

	serviceNoChange = 0xFFFFFFFF // SERVICE_NO_CHANGE

	serviceConfigFailureActions   = 2 // SERVICE_CONFIG_FAILURE_ACTIONS
	serviceConfigDelayedAutoStart = 3 // SERVICE_CONFIG_DELAYED_AUTO_START_INFO
	serviceConfigTriggerInfo      = 8 // SERVICE_CONFIG_TRIGGER_INFO

	// errInsufficientBuffer (122) ya está en sockets.go.
	errServiceAlreadyRunning = 1056
	errServiceDisabled       = 1058

	// El tope que documenta QueryServiceConfig para su búfer.
	serviceConfigMax = 8 * 1024
)

// ErrServiceDisabled: el servicio está deshabilitado y no se puede arrancar.
var ErrServiceDisabled = fmt.Errorf("el servicio está deshabilitado")

// openService abre un servicio por nombre con los permisos pedidos. Quien
// llama cierra los dos handles con closeService.
func openService(name string, access uintptr) (scm, svc uintptr, err error) {
	scm, e := call(procOpenSCManagerW, 0, 0, scManagerConnect)
	if scm == 0 {
		return 0, 0, fmt.Errorf("no pude abrir el administrador de servicios: %w", scmError(e))
	}
	svc, e = call(procOpenServiceW, scm, uintptr(unsafe.Pointer(UTF16Ptr(name))), access)
	if svc == 0 {
		call(procCloseServiceHandle, scm)
		return 0, 0, openServiceError(e)
	}
	return scm, svc, nil
}

func closeService(scm, svc uintptr) {
	call(procCloseServiceHandle, svc)
	call(procCloseServiceHandle, scm)
}

// QueryServiceConfig lee la configuración del servicio name. No pide
// administrador. Errores: ErrServiceNotFound, ErrAccessDenied. La parte
// opcional (inicio retrasado, disparadores, recuperación) es best-effort: si
// no se pudo leer, Partial queda en true.
func QueryServiceConfig(name string) (ServiceConfig, error) {
	scm, svc, err := openService(name, serviceQueryConfig)
	if err != nil {
		return ServiceConfig{}, err
	}
	defer closeService(scm, svc)

	buf := alignedBuffer(serviceConfigMax)
	var needed uint32
	if r, e := call(procQueryServiceConfigW, svc, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&needed))); r == 0 {
		return ServiceConfig{}, fmt.Errorf("no pude leer la configuración de %q: %w", name, lastError(e))
	}
	c, err := parseServiceConfig(buf, uintptr(unsafe.Pointer(&buf[0])))
	if err != nil {
		return ServiceConfig{}, err
	}
	c.Name = name

	if b, ok := queryConfig2(svc, serviceConfigDelayedAutoStart); ok && len(b) >= 4 {
		c.Delayed = c.Start == StartAuto && le32(b, 0) != 0
	} else if c.Start == StartAuto {
		c.Partial = true
	}
	// SERVICE_TRIGGER_INFO empieza con cTriggers (DWORD).
	if b, ok := queryConfig2(svc, serviceConfigTriggerInfo); ok && len(b) >= 4 {
		c.Triggers = int(le32(b, 0))
	} else {
		c.Partial = true
	}
	if b, ok := queryConfig2(svc, serviceConfigFailureActions); ok {
		if acts, err := parseFailureActions(b, uintptr(unsafe.Pointer(&b[0]))); err == nil {
			c.Recovery = acts
		} else {
			c.Partial = true
		}
	} else {
		c.Partial = true
	}
	return c, nil
}

// queryConfig2 lee un bloque de QueryServiceConfig2W: primero pregunta el
// tamaño, después pide el búfer. ok=false si el servicio no tiene ese bloque
// o no se pudo leer.
func queryConfig2(svc uintptr, level uintptr) ([]byte, bool) {
	var needed uint32
	r, e := call(procQueryServiceConfig2W, svc, level, 0, 0, uintptr(unsafe.Pointer(&needed)))
	if r != 0 || e != errInsufficientBuffer || needed == 0 || needed > serviceConfigMax*8 {
		return nil, false
	}
	buf := alignedBuffer(int(needed))
	if r, _ := call(procQueryServiceConfig2W, svc, level, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&needed))); r == 0 {
		return nil, false
	}
	return buf[:needed], true
}

// SetServiceDemandStart deja el servicio name en inicio MANUAL, sin tocar nada
// más de su configuración (todo lo demás va con SERVICE_NO_CHANGE). Pide
// administrador. Errores: ErrServiceNotFound, ErrAccessDenied.
//
// No valida si conviene: eso lo decide quien llama (killport solo lo hace con
// servicios de terceros, nunca con controladores ni servicios de Windows).
func SetServiceDemandStart(name string) error {
	scm, svc, err := openService(name, serviceChangeConfig)
	if err != nil {
		return err
	}
	defer closeService(scm, svc)
	if r, e := call(procChangeServiceConfigW, svc, serviceNoChange, uintptr(StartDemand), serviceNoChange, 0, 0, 0, 0, 0, 0, 0); r == 0 {
		if e == 5 {
			return ErrAccessDenied
		}
		return fmt.Errorf("no pude cambiar el inicio de %q: %w", name, lastError(e))
	}
	return nil
}

// ServiceState es el estado de un servicio (o controlador) y el proceso que
// lo aloja (0 si no corre, o si es un controlador).
type ServiceState struct {
	State   uint32 // SERVICE_STOPPED (1), SERVICE_RUNNING (4), …
	PID     uint32
	Accepts uint32 // dwControlsAccepted: qué órdenes acepta ahora
}

// serviceAcceptStop (SERVICE_ACCEPT_STOP): el servicio acepta que lo detengan.
const serviceAcceptStop = 0x00000001

// Running dice si está corriendo.
func (s ServiceState) Running() bool { return s.State == serviceRunning }

// AcceptsStop dice si el servicio acepta la orden de detenerse. Los que
// sostienen a Windows (RpcSs, DcomLaunch, el cliente de directivas…) corren
// sin aceptarla: `net stop` sobre ellos falla siempre, sea quien sea el que lo
// pide. Solo tiene sentido para un servicio que corre.
func (s ServiceState) AcceptsStop() bool { return s.Accepts&serviceAcceptStop != 0 }

// Stopped dice si está detenido.
func (s ServiceState) Stopped() bool { return s.State == serviceStopped }

// QueryServiceState consulta el estado del servicio name. No pide
// administrador. Errores: ErrServiceNotFound, ErrAccessDenied.
func QueryServiceState(name string) (ServiceState, error) {
	scm, svc, err := openService(name, serviceQueryStatus)
	if err != nil {
		return ServiceState{}, err
	}
	defer closeService(scm, svc)
	var s [9]uint32 // SERVICE_STATUS_PROCESS
	var needed uint32
	if r, e := call(procQueryServiceStatusEx, svc, scStatusProcessInfo,
		uintptr(unsafe.Pointer(&s[0])), uintptr(len(s)*4), uintptr(unsafe.Pointer(&needed))); r == 0 {
		return ServiceState{}, fmt.Errorf("no pude consultar el estado de %q: %w", name, lastError(e))
	}
	return ServiceState{State: s[1], Accepts: s[2], PID: s[7]}, nil
}

// StartService arranca el servicio name y espera hasta deadline a que quede
// corriendo, guiándose por el dwWaitHint que informa el propio servicio (la
// misma espera que StopService, y por el mismo motivo). Si ya corría, nil.
// Errores: ErrAccessDenied, ErrServiceNotFound, ErrServiceDisabled,
// ErrServiceTimeout, ctx.Err().
func StartService(ctx context.Context, name string, deadline time.Time) error {
	scm, svc, err := openService(name, serviceStart|serviceQueryStatus)
	if err != nil {
		return err
	}
	defer closeService(scm, svc)
	if r, e := call(procStartServiceW, svc, 0, 0); r == 0 {
		switch e {
		case errServiceAlreadyRunning:
			return nil
		case errServiceDisabled:
			return ErrServiceDisabled
		case 5:
			return ErrAccessDenied
		default:
			return fmt.Errorf("no pude arrancar %q: %w", name, lastError(e))
		}
	}
	return waitService(ctx, func() (uint32, uint32, error) { return queryServiceState(svc) }, deadline,
		func(st uint32) (bool, error) {
			switch st {
			case serviceRunning:
				return true, nil
			case serviceStopped:
				return true, fmt.Errorf("%q arrancó y se detuvo enseguida", name)
			}
			return false, nil
		})
}

// ActiveDependents lista los servicios ACTIVOS que dependen de name (lo que
// muestra Get-Service -DependentServices). Sirve para nombrar a los que usan
// un controlador: los que dependen de HTTP son los sospechosos de tener un
// puerto de http.sys. No pide administrador. nil si no se pudo.
func ActiveDependents(name string) []string {
	scm, _ := call(procOpenSCManagerW, 0, 0, scManagerConnect)
	if scm == 0 {
		return nil
	}
	defer call(procCloseServiceHandle, scm)
	return activeDependents(scm, name)
}
