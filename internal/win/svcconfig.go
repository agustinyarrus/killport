package win

// svcconfig.go: cómo está CONFIGURADO un servicio —con qué arranca, si vuelve
// solo, qué hace Windows si se cae—, leído de los búferes que llenan
// QueryServiceConfigW y QueryServiceConfig2W. Sin build tag: es aritmética
// sobre []byte y se prueba con bytes sintéticos. Las llamadas están en
// svcconfig_windows.go.
//
// Importa para killport porque detener un servicio no es el final de la
// historia: uno de inicio automático vuelve en el próximo arranque, y uno
// manual con disparadores (un evento del sistema lo despierta) también. Eso
// es lo que el usuario necesita saber ANTES de pelearse otra vez con el mismo
// puerto.

import "strings"

// StartType es el tipo de inicio de un servicio (dwStartType).
type StartType uint32

const (
	StartBoot     StartType = 0 // controlador que carga el cargador de arranque
	StartSystem   StartType = 1 // controlador que carga el kernel al iniciar
	StartAuto     StartType = 2 // automático: arranca con Windows
	StartDemand   StartType = 3 // manual: arranca cuando alguien lo pide
	StartDisabled StartType = 4 // deshabilitado
)

// Tipos de servicio (dwServiceType), los que importan para decidir.
const (
	ServiceKernelDriver     = 0x00000001
	ServiceFileSystemDriver = 0x00000002
	ServiceWin32OwnProcess  = 0x00000010
	ServiceWin32ShareProc   = 0x00000020
	ServiceUserService      = 0x00000040 // plantilla/instancia de un servicio por usuario
)

// RecoveryAction es lo que hace el administrador de servicios si el servicio
// se cae (SC_ACTION_TYPE).
type RecoveryAction uint32

const (
	RecoveryNone    RecoveryAction = 0
	RecoveryRestart RecoveryAction = 1 // reiniciar el servicio
	RecoveryReboot  RecoveryAction = 2 // reiniciar la máquina
	RecoveryCommand RecoveryAction = 3 // correr un comando
)

// ServiceConfig es la configuración de un servicio. Los campos que no se
// pudieron leer quedan en cero y se dice cuáles en Partial.
type ServiceConfig struct {
	Name        string
	DisplayName string
	Type        uint32    // dwServiceType
	Start       StartType // dwStartType
	Delayed     bool      // automático con inicio retrasado
	BinaryPath  string    // la línea de comandos con que arranca (lpBinaryPathName)
	Account     string    // con qué cuenta corre (lpServiceStartName)
	// Triggers es cuántos disparadores tiene registrados: con alguno, el
	// servicio puede arrancar solo aunque su inicio sea manual.
	Triggers int
	// Recovery son las acciones de recuperación, en orden (primera falla,
	// segunda, siguientes). Vacío = no hace nada si se cae.
	Recovery []RecoveryAction
	// Partial: no se pudo leer la parte opcional (retraso, disparadores o
	// recuperación): los campos de arriba valen como "no se sabe", no como "no".
	Partial bool
}

// Driver dice si es un controlador (del kernel o de sistema de archivos), no
// un servicio de Win32: killport no les cambia la configuración jamás.
func (c ServiceConfig) Driver() bool {
	return c.Type&(ServiceKernelDriver|ServiceFileSystemDriver) != 0
}

// OwnProcess dice si el servicio corre en su propio proceso.
func (c ServiceConfig) OwnProcess() bool { return c.Type&ServiceWin32OwnProcess != 0 }

// Restarts dice si Windows lo vuelve a levantar cuando se cae. (Detenerlo por
// el administrador de servicios no es caerse: no dispara la recuperación.)
func (c ServiceConfig) Restarts() bool {
	for _, a := range c.Recovery {
		if a == RecoveryRestart {
			return true
		}
	}
	return false
}

// Image es la ruta del ejecutable del servicio, sacada de BinaryPath: lo que
// está entre comillas, o, sin comillas, el prefijo más corto que termina en
// ".exe" (las rutas con espacios sin comillas son un clásico de los
// instaladores). "" si BinaryPath no nombra un .exe (los controladores).
func (c ServiceConfig) Image() string {
	s := strings.TrimSpace(c.BinaryPath)
	if rest, ok := strings.CutPrefix(s, `"`); ok {
		if i := strings.IndexByte(rest, '"'); i >= 0 {
			return rest[:i]
		}
		return rest
	}
	low := strings.ToLower(s)
	for from := 0; ; {
		i := strings.Index(low[from:], ".exe")
		if i < 0 {
			return ""
		}
		end := from + i + len(".exe")
		if end == len(s) || s[end] == ' ' || s[end] == '\t' {
			return s[:end]
		}
		from = end
	}
}

// QUERY_SERVICE_CONFIGW en 64 bits (0x40 bytes): tres DWORD, relleno, y
// punteros a cadenas que viven en el mismo búfer. SERVICE_FAILURE_ACTIONSW
// (0x28 bytes): el período, dos punteros, la cantidad y el puntero a las
// acciones.
const (
	qscServiceType  = 0x00 // DWORD  dwServiceType
	qscStartType    = 0x04 // DWORD  dwStartType
	qscBinaryPath   = 0x10 // LPWSTR lpBinaryPathName (0x08 dwErrorControl, 0x0C relleno)
	qscStartName    = 0x30 // LPWSTR lpServiceStartName (0x18 grupo, 0x20 etiqueta, 0x28 dependencias)
	qscDisplayName  = 0x38 // LPWSTR lpDisplayName
	qscSize         = 0x40
	sfaActionCount  = 0x18 // SERVICE_FAILURE_ACTIONSW.cActions (0x00 período, 0x08 y 0x10 punteros)
	sfaActions      = 0x20 // SC_ACTION *lpsaActions
	sfaSize         = 0x28
	scActionSize    = 8 // SC_ACTION: tipo (4) y demora en ms (4)
	maxRecoverySane = 64
)

// parseServiceConfig lee el QUERY_SERVICE_CONFIGW de b (que empieza en la
// dirección base).
func parseServiceConfig(b []byte, base uintptr) (ServiceConfig, error) {
	e, err := window(b, 0, qscSize, "configuración del servicio")
	if err != nil {
		return ServiceConfig{}, err
	}
	c := ServiceConfig{Type: le32(e, qscServiceType), Start: StartType(le32(e, qscStartType))}
	if c.BinaryPath, err = cString16(b, base, le64(e, qscBinaryPath), "servicio: ruta del binario"); err != nil {
		return ServiceConfig{}, err
	}
	if c.Account, err = cString16(b, base, le64(e, qscStartName), "servicio: cuenta"); err != nil {
		return ServiceConfig{}, err
	}
	if c.DisplayName, err = cString16(b, base, le64(e, qscDisplayName), "servicio: nombre visible"); err != nil {
		return ServiceConfig{}, err
	}
	return c, nil
}

// parseFailureActions lee el SERVICE_FAILURE_ACTIONSW de b: la lista de
// acciones de recuperación, en orden.
func parseFailureActions(b []byte, base uintptr) ([]RecoveryAction, error) {
	e, err := window(b, 0, sfaSize, "recuperación del servicio")
	if err != nil {
		return nil, err
	}
	n := int(le32(e, sfaActionCount))
	ptr := le64(e, sfaActions)
	if n == 0 || ptr == 0 {
		return nil, nil
	}
	if n > maxRecoverySane {
		return nil, malformed("recuperación del servicio: %d acciones", n)
	}
	acts, err := pointee(b, base, ptr, n*scActionSize, "recuperación del servicio: acciones")
	if err != nil {
		return nil, err
	}
	out := make([]RecoveryAction, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, RecoveryAction(le32(acts, i*scActionSize)))
	}
	return out, nil
}
