package reaper

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// OS es TODO lo que el motor necesita del sistema operativo. El núcleo se
// escribe y se prueba contra esta interfaz; el integrador la implementa con
// internal/win (una capacidad por método, en el orden del diseño), y los tests
// la implementan con un simulador de eventos discretos de reloj virtual. El
// motor NUNCA llama al reloj real ni duerme por su cuenta: pide la hora a Now,
// y toda espera pasa por Wait o Sleep, que en el simulador adelantan el reloj
// virtual. Así los tests fijan tiempos exactos.
//
// Los errores que devuelven los métodos se distinguen con errors.Is contra los
// centinelas de este archivo: el motor decide según CUÁL fue el error (ya no
// existe / se recicló / acceso denegado / protegido), no según su texto.
type OS interface {
	// Now es la hora actual (real en producción, virtual en los tests). O(1).
	Now() time.Time

	// Sleep espera d o hasta que se cancele ctx; devuelve false si se canceló.
	// Es el único retardo del motor (el backoff de la verificación). En el
	// simulador adelanta el reloj virtual. (capacidad: reloj inyectado)
	Sleep(ctx context.Context, d time.Duration) bool

	// Sockets lee las tablas TCP y UDP (IPv4 e IPv6) con el PID dueño, la hora
	// de creación del socket y, si se supo, el servicio dueño. (capacidad 1)
	Sockets(ctx context.Context) ([]ports.Socket, error)

	// Snapshot toma la foto de TODOS los procesos en una sola llamada, sin abrir
	// ninguno: PID, PID padre, hora de creación, sesión, nombre, y si es crítico
	// cuando se pudo saber. (capacidad 2)
	Snapshot(ctx context.Context) ([]Proc, error)

	// Detail trae datos caros de un proceso, best-effort (campos vacíos si no
	// hay permiso). Error solo si el proceso YA NO EXISTE (ErrGone). El motor lo
	// usa para enriquecer lo que muestra, nunca para decidir. (capacidad 3)
	Detail(id ID) (Detail, error)

	// Services mapea PID → servicios que corren en ese proceso (nombre corto,
	// nombre visible, si comparte proceso como svchost). (capacidad 8)
	Services() (map[uint32][]Service, error)

	// Excluded lee los rangos de puertos que Windows reserva
	// (netsh ... excludedportrange), para diagnosticar puertos sin dueño.
	// (capacidad 10, rangos excluidos)
	Excluded() (ports.Exclusions, error)

	// DynamicRanges lee el rango de puertos dinámicos de cada protocolo y
	// familia (netsh ... dynamicport): de ahí saca WinNAT los bloques que
	// reserva. Un rango que arranca antes del 49152 explica por qué esos
	// bloques caen entre los puertos de desarrollo. (capacidad 10, diagnóstico)
	DynamicRanges() (ports.DynamicRanges, error)

	// Open abre un handle CON IDENTIDAD: compara la hora de creación con la
	// esperada y, si no coincide, el PID se recicló → ErrRecycled. Mientras el
	// handle vive, el PID no se puede reciclar, así que el motor lo sostiene
	// desde que identifica hasta el final. Errores: ErrGone (ya no existe),
	// ErrRecycled (otro proceso con ese PID), ErrAccessDenied (sin permiso),
	// ErrProtected (PPL: ni con admin), ErrCritical (está marcado como proceso
	// crítico: terminarlo colgaría Windows, así que ni se abre). (capacidad 4)
	Open(id ID) (Handle, error)

	// Wait espera SIN POLLING la salida de cualquiera de hs, hasta deadline o
	// hasta que se cancele ctx. Devuelve el handle que salió con su código
	// (outcome WaitExited), o nil con WaitDeadline / WaitCanceled. Se llama en
	// bucle quitando los que ya salieron. Con hs vacío es una espera pura hasta
	// deadline. En producción es WaitForMultipleObjects + un evento atado a
	// ctx.Done(); en el simulador, adelanta el reloj al primer evento. Tras
	// devolver un WaitExited, Now() es el instante EXACTO de esa salida.
	// (capacidad 5)
	Wait(ctx context.Context, hs []Handle, deadline time.Time) (Handle, ExitInfo, WaitOutcome)

	// CloseWindows le manda WM_CLOSE a las ventanas de nivel superior del
	// proceso y devuelve cuántas encontró (0 = no tiene ventanas). (capacidad 6)
	CloseWindows(id ID) (int, error)

	// ConsolePIDs devuelve los PID adjuntos a la consola del proceso, para
	// agrupar víctimas por consola. ErrNoConsole si no tiene. (capacidad 9)
	ConsolePIDs(id ID) ([]uint32, error)

	// CtrlC le manda UN Ctrl+C a la consola del proceso, pero solo si TODOS los
	// procesos de esa consola están en allow: si hay un ajeno, devuelve
	// *SharedConsoleError con la lista de intrusos y NO manda nada (apretar
	// Ctrl+C ahí mataría a terceros). ErrNoConsole si no tiene consola.
	//
	// allow son IDENTIDADES, no PID: el conjunto se arma con el bosque de
	// antes (ancestros, raíz, subárbol), y entre la foto y el envío un ancestro
	// pudo morir y su PID reciclarse. Un miembro de la consola cuenta como
	// permitido solo si su PID está en allow Y su hora de creación coincide
	// (Created cero en allow = identidad desconocida, basta el PID). La
	// comprobación se hace pegada al envío, del lado que manda. (capacidad 7)
	CtrlC(id ID, allow []ID) error

	// StopService detiene un servicio y espera a que quede STOPPED, hasta
	// deadline. Errores: ErrServiceAccessDenied (necesita admin),
	// *ServiceDependentsError (servicios dependientes activos, con sus nombres),
	// ErrServiceTimeout, ErrGone (no existe). (capacidad 8, detener servicio)
	StopService(ctx context.Context, name string, deadline time.Time) error

	// ProbeBind intenta bind() sin listen() en el puerto, en la dirección
	// comodín de esa familia y con uso EXCLUSIVO (SO_EXCLUSIVEADDRUSE, y en
	// IPv6 sin doble pila): libre, en uso, o prohibido. No dispara el aviso del
	// Firewall. Es el desempate de un puerto que ninguna tabla atribuye a
	// nadie; la tabla de sockets sigue siendo la fuente primaria, porque con
	// uso exclusivo un WSAEACCES es ambiguo (rango excluido, o un socket ajeno
	// que no se deja compartir). (capacidad 10, sondeo de bind)
	ProbeBind(proto ports.Proto, fam ports.Family, port uint16) ports.BindResult

	// StopExternal detiene objetivos que NO son procesos de Windows (un
	// contenedor, un proceso Linux) con la operación de su dominio, TODOS A LA
	// VEZ y con un mismo plazo: deadline es el fin del cierre prolijo; pasado
	// eso, el dominio fuerza (SIGKILL) y la llamada puede tardar un poco más
	// en volver. Con force, sin cierre prolijo. Llama a done(i, r) por cada
	// externo apenas termina, SIEMPRE desde la goroutine que llamó —el motor
	// sigue siendo secuencial y el simulador determinista— y vuelve cuando
	// terminaron todos o se canceló ctx (a los que quedaron sin done, el motor
	// los da por pendientes). Tras cada done, Now() es el instante de ese
	// final. Resultados: Closed (con el método con que cedió), Terminated
	// (hubo que forzarlo), Gone (ya no estaba), Denied (sin permiso), Error.
	// (capacidad 11, dominios externos)
	StopExternal(ctx context.Context, xs []External, deadline time.Time, force bool, done func(i int, r ExternalStop))
}

// Handle es un proceso abierto con identidad confirmada. Mientras no se cierre,
// el PID no se recicla: la identidad se mantiene.
type Handle interface {
	// ID es la identidad confirmada del proceso abierto.
	ID() ID
	// CanTerminate dice si el handle tiene el permiso PROCESS_TERMINATE. Si es
	// false, solo se puede esperar su salida (se abrió sin ese permiso porque
	// se negó), y terminarlo requiere elevación.
	CanTerminate() bool
	// Terminate pide TerminateProcess. ErrAccessDenied si falta el permiso,
	// ErrGone si ya salió.
	Terminate() error
	// Close libera el handle. Idempotente.
	Close()
}

// Detail son los datos caros y best-effort de un proceso, para mostrar.
type Detail struct {
	CommandLine   string // línea de comandos completa; "" si no se pudo leer
	User          string // usuario dueño del token; "" si no se pudo
	Critical      bool   // marcado ProcessBreakOnTermination
	CriticalKnown bool   // si se pudo consultar de verdad (si no, Critical no vale)
	Cwd           string // carpeta de trabajo actual; "" si no se pudo leer
}

// Service describe un servicio de Windows que corre en un proceso.
type Service struct {
	Name    string // nombre corto, el de "net stop NOMBRE"
	Display string // nombre visible
	Shared  bool   // corre en un proceso compartido (svchost)
}

// ExitInfo acompaña la salida de un proceso.
type ExitInfo struct {
	Code uint32
	// At es cuándo salió según el kernel (GetProcessTimes); cero si no se
	// sabe. La espera se entera un rato después (lo que tardó en volver), y
	// medir hasta At da cuánto tardó de verdad.
	At time.Time
}

// WaitOutcome dice por qué volvió Wait.
type WaitOutcome uint8

const (
	WaitExited   WaitOutcome = iota // un proceso salió (el handle devuelto)
	WaitDeadline                    // venció el plazo, nadie salió
	WaitCanceled                    // se canceló ctx (Ctrl+C del usuario)
)

// Centinelas de error, comparados con errors.Is. El motor decide por el tipo
// de error, no por su texto.
var (
	ErrGone                = errors.New("el proceso ya no existe")
	ErrRecycled            = errors.New("el PID se recicló: ahora es otro proceso")
	ErrAccessDenied        = errors.New("acceso denegado")
	ErrProtected           = errors.New("proceso protegido (PPL): ni un administrador puede terminarlo")
	ErrCritical            = errors.New("proceso marcado como crítico: terminarlo colgaría Windows")
	ErrNoConsole           = errors.New("el proceso no tiene consola")
	ErrServiceAccessDenied = errors.New("detener el servicio necesita permisos de administrador")
	ErrServiceTimeout      = errors.New("el servicio no llegó a detenerse a tiempo")
	// ErrStopPending: el usuario cortó (Ctrl+C) mientras se esperaba que el
	// servicio quedara detenido. El pedido ya se había mandado: el servicio
	// sigue deteniéndose, y la víctima queda sin veredicto.
	ErrStopPending = errors.New("cancelaste mientras se detenía: el pedido ya se mandó y el servicio sigue deteniéndose")
)

// SharedConsoleError se devuelve cuando la consola del objetivo la comparten
// procesos ajenos: mandar Ctrl+C ahí los mataría también.
type SharedConsoleError struct {
	Intruders []uint32 // PID de la consola que NO están en el conjunto permitido
}

func (e *SharedConsoleError) Error() string {
	ids := make([]string, len(e.Intruders))
	for i, p := range e.Intruders {
		ids[i] = strconv.Itoa(int(p))
	}
	return "la consola la comparten otros procesos (PID " + strings.Join(ids, ", ") + ")"
}

// ServiceDependentsError se devuelve cuando el servicio no se puede detener
// porque otros dependen de él.
type ServiceDependentsError struct {
	Names []string
}

func (e *ServiceDependentsError) Error() string {
	return "otros servicios dependen de este: " + strings.Join(e.Names, ", ")
}
