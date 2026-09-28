//go:build windows

package win

import "time"

// console_ctrlc.go: mandarle Ctrl+C a la consola de OTRO proceso, con la misma
// prudencia que apretar Ctrl+C a mano en esa ventana.
//
// El problema: GenerateConsoleCtrlEvent(CTRL_C_EVENT, 0) le llega a TODOS los
// procesos de la consola del que llama, no se puede dirigir a uno. Y un proceso
// solo puede estar enganchado a UNA consola. Por eso killport no puede mandar
// Ctrl+C desde su propia consola (mataría a su terminal y a sus vecinos): lanza
// un AYUDANTE con DETACHED_PROCESS (sin consola propia), que se engancha a la
// consola del objetivo y manda ahí el evento. SendCtrlCToConsole es lo que
// corre ese ayudante.
//
// La regla de oro: solo se manda si TODOS los procesos de esa consola están en
// el conjunto permitido (el objetivo, su árbol y sus ancestros). Si hay un
// intruso, apretar Ctrl+C ahí también lo golpearía: no se manda nada y se
// devuelven los intrusos para que el motor lo explique.

// CtrlCResult distingue los desenlaces del envío.
type CtrlCResult int

const (
	// CtrlCSent: el evento se mandó a la consola del objetivo.
	CtrlCSent CtrlCResult = iota
	// CtrlCNoConsole: el proceso no está atado a ninguna consola (es de
	// ventanas, de servicio, o ya cerró la suya). No hay dónde mandar Ctrl+C.
	CtrlCNoConsole
	// CtrlCShared: la consola la comparten procesos fuera del conjunto
	// permitido. No se mandó nada; los intrusos vuelven en la lista.
	CtrlCShared
)

const (
	errAccessDenied     = 5  // ERROR_ACCESS_DENIED: no se pudo enganchar (proceso de otra sesión, o GUI del Store)
	errInvalidHandle    = 6  // ERROR_INVALID_HANDLE: el proceso no tiene consola
	errInvalidParameter = 87 // ERROR_INVALID_PARAMETER: no existe ese PID
)

// noConsole son los códigos de AttachConsole que significan, todos, "no hay
// una consola de ese proceso que podamos manejar": ninguna consola (6), no
// existe el PID (87), o no se pudo enganchar por permisos/sesión (5). En los
// tres casos el desenlace es el mismo: no hay a quién mandarle Ctrl+C.
func noConsole(errno uint32) bool {
	return errno == errAccessDenied || errno == errInvalidHandle || errno == errInvalidParameter
}

// SendCtrlCToConsole corre en el proceso AYUDANTE (lanzado con DETACHED_PROCESS,
// sin consola propia). Se engancha a la consola de pid, comprueba que todos sus
// miembros estén en allowed y, si es así, manda Ctrl+C a esa consola. allowed es
// el conjunto de PID a los que ese Ctrl+C puede golpear sin dañar a terceros.
//
// Pasos y por qué:
//  1. FreeConsole por las dudas (un ayudante DETACHED no tiene, pero si tuviera,
//     AttachConsole fallaría con ACCESS_DENIED).
//  2. AttachConsole(pid): si el proceso no tiene consola → CtrlCNoConsole.
//  3. GetConsoleProcessList: quiénes comparten esa consola.
//  4. Si alguno no está en allowed → soltar la consola y devolver los intrusos.
//  5. IgnoreCtrlC(true): el propio ayudante ignora el Ctrl+C que va a generar
//     (si no, se mataría a sí mismo antes de que el evento llegue a los demás).
//  6. GenerateCtrlEvent(CTRL_C_EVENT, 0): a toda la consola.
//
// allowed va por IDENTIDAD: PID → hora de creación esperada. Un miembro cuenta
// como permitido si su PID está y, cuando la hora no es cero, el proceso que
// hoy tiene ese PID nació en ese instante (se abre con QUERY_LIMITED y se lee
// GetProcessTimes). Así un ancestro que murió y cuyo PID lo reusó otro proceso
// de la misma consola es un intruso, aunque el número coincida. Un miembro que
// no se puede abrir para confirmarlo también es intruso: ante la duda, no se
// manda.
func SendCtrlCToConsole(pid uint32, allowed map[uint32]time.Time) (CtrlCResult, []uint32, error) {
	members, attached, err := attachAndList(pid)
	if !attached {
		return CtrlCNoConsole, nil, err
	}
	defer FreeConsole()

	var intruders []uint32
	for _, m := range members {
		want, ok := allowed[m]
		if !ok || (!want.IsZero() && !isProcess(m, want)) {
			intruders = append(intruders, m)
		}
	}
	if len(intruders) > 0 {
		return CtrlCShared, intruders, nil
	}

	if err := IgnoreCtrlC(true); err != nil {
		return CtrlCNoConsole, nil, err
	}
	if err := GenerateCtrlEvent(CtrlCEvent, 0); err != nil {
		return CtrlCNoConsole, nil, err
	}
	return CtrlCSent, nil, nil
}

// ConsoleMembers devuelve los PID de la consola a la que está atado pid (sin
// contar al que pregunta), o hasConsole=false si no tiene una que se pueda
// manejar. Corre en el ayudante DETACHED, como SendCtrlCToConsole: un proceso
// con consola propia no puede engancharse a otra sin soltar la suya. Se puede
// llamar varias veces seguidas (engancha, lee, suelta).
func ConsoleMembers(pid uint32) (members []uint32, hasConsole bool, err error) {
	members, attached, err := attachAndList(pid)
	if attached {
		FreeConsole()
	}
	return members, attached, err
}

// attachAndList se engancha a la consola de pid y lee quiénes la comparten.
// Si attached es true, quien llama tiene que soltarla (FreeConsole).
//
// Pasos y por qué: FreeConsole por las dudas (con una consola propia,
// AttachConsole fallaría con ACCESS_DENIED); AttachConsole(pid); y
// GetConsoleProcessList. Al engancharse, el propio ayudante pasa a ser miembro
// y aparece en la lista: se lo excluye (es transitorio y está por soltarla),
// o se detectaría siempre a sí mismo como intruso.
func attachAndList(pid uint32) (members []uint32, attached bool, err error) {
	FreeConsole() // sin consola no falla de forma relevante; se ignora
	if err := AttachConsole(pid); err != nil {
		if errno, ok := errnoOf(err); ok && noConsole(uint32(errno)) {
			return nil, false, nil
		}
		return nil, false, err
	}
	all, err := ConsoleProcessList()
	if err != nil {
		FreeConsole()
		return nil, false, err
	}
	self := CurrentProcessID()
	for _, m := range all {
		if m != self {
			members = append(members, m)
		}
	}
	return members, true, nil
}

// isProcess confirma que el proceso que hoy tiene ese PID es el que nació en
// created. false si no se puede abrir (ni con QUERY_LIMITED) o no coincide.
func isProcess(pid uint32, created time.Time) bool {
	h, err := openProcess(processQueryLimitedInfo, pid)
	if err != nil {
		return false
	}
	defer closeHandle(h)
	return sameInstant(handleCreated(h), created)
}
