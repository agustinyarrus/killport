package reaper

import (
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Method es CÓMO terminó un proceso: sirve para explicarle al usuario qué pasó
// ("cerró solo con Ctrl+C", "lo terminé en seco").
type Method uint8

const (
	MethodNone      Method = iota // todavía no se actuó
	MethodCtrlC                   // salió tras el Ctrl+C que le mandamos
	MethodClose                   // salió tras el WM_CLOSE
	MethodSelf                    // salió solo, sin que le mandáramos señal
	MethodTerminate               // lo terminamos en seco (TerminateProcess; SIGKILL en Linux)
	MethodService                 // se detuvo el servicio que lo sostenía
	MethodSigInt                  // un proceso Linux salió tras SIGINT (el Ctrl+C de Linux)
	MethodSigTerm                 // un proceso Linux salió tras SIGTERM
	MethodEngine                  // un contenedor que su motor detuvo prolijo (docker stop)
	MethodNetsh                   // una regla de reenvío que se borró con netsh
)

func (m Method) String() string {
	switch m {
	case MethodCtrlC:
		return "Ctrl+C"
	case MethodClose:
		return "cierre de ventana"
	case MethodSelf:
		return "salió solo"
	case MethodTerminate:
		return "terminación"
	case MethodService:
		return "servicio detenido"
	case MethodSigInt:
		return "SIGINT"
	case MethodSigTerm:
		return "SIGTERM"
	case MethodEngine:
		return "stop del motor"
	case MethodNetsh:
		return "netsh"
	}
	return ""
}

// Outcome es en qué terminó una víctima. Un valor por cada final posible: los
// estados inválidos no se pueden representar.
type Outcome uint8

const (
	OutcomePending        Outcome = iota // aún no resuelto (estado transitorio interno)
	OutcomeClosed                        // cerró prolijo (Ctrl+C o WM_CLOSE)
	OutcomeTerminated                    // se terminó en seco
	OutcomeGone                          // ya no estaba al ir a abrirlo
	OutcomeRecycled                      // el PID se había reciclado: no se tocó
	OutcomeDenied                        // acceso denegado: candidato a elevación
	OutcomeProtected                     // protegido (PPL): ni con admin
	OutcomeServiceStopped                // se lo bajó deteniendo su servicio
	OutcomeError                         // otro error
	// OutcomeCritical: está marcado como proceso crítico
	// (ProcessBreakOnTermination): terminarlo tira un bugcheck. No se tocó. Es
	// la red que queda debajo de los impostores, que ya no están protegidos
	// por llamarse como un proceso de Windows.
	OutcomeCritical
)

func (o Outcome) String() string {
	switch o {
	case OutcomeClosed:
		return "cerró prolijo"
	case OutcomeTerminated:
		return "terminado en seco"
	case OutcomeGone:
		return "ya no estaba"
	case OutcomeRecycled:
		return "PID reciclado"
	case OutcomeDenied:
		return "acceso denegado"
	case OutcomeProtected:
		return "protegido"
	case OutcomeServiceStopped:
		return "servicio detenido"
	case OutcomeError:
		return "error"
	case OutcomeCritical:
		return "crítico"
	}
	return "pendiente"
}

// Done dice si la víctima ya no ocupa el puerto (el proceso se fue).
func (o Outcome) Done() bool {
	switch o {
	case OutcomeClosed, OutcomeTerminated, OutcomeGone, OutcomeServiceStopped:
		return true
	}
	return false
}

// VictimResult es cómo le fue a una víctima.
type VictimResult struct {
	ID      ID
	Name    string
	Outcome Outcome
	Method  Method
	Elapsed time.Duration // cuánto tardó desde que se le mandó la señal, si aplica
	Err     error         // el error crudo, cuando Outcome es Denied/Protected/Error
}

// PortStatus es cómo quedó un puerto pedido después de actuar y verificar.
type PortStatus uint8

const (
	PortFree      PortStatus = iota // liberado
	PortPhantom                     // sigue ocupado por un fantasma: hijos que heredaron el socket
	PortRespawned                   // lo retomó otro proceso (un supervisor lo relanzó)
	PortReserved                    // rango excluido / WSAEACCES: lo reserva Windows, no hay proceso
	PortBusy                        // sigue ocupado y no se pudo liberar (acceso denegado/protegido), o lo tiene alguien que no figura escuchando
	PortRefused                     // lo tiene un dueño que killport no toca (kernel, crítico, tu terminal, servicio de Windows)
	// PortAnswered: lo que se cerró ya no contesta la dirección pedida, pero
	// ahora la contesta otro que ya escuchaba el puerto y no se pidió
	// (Bystander): "killport localhost:5173" cerró al de [::1], y localhost
	// llega ahora al de 0.0.0.0. La dirección no quedó libre.
	PortAnswered
)

func (s PortStatus) String() string {
	switch s {
	case PortFree:
		return "libre"
	case PortPhantom:
		return "fantasma"
	case PortRespawned:
		return "lo retomó otro proceso"
	case PortReserved:
		return "reservado por Windows"
	case PortBusy:
		return "sigue ocupado"
	case PortRefused:
		return "no se toca"
	case PortAnswered:
		return "lo contesta otro"
	}
	return ""
}

// PortResult es el veredicto por puerto pedido.
type PortResult struct {
	Port      uint16
	Proto     ports.Proto // qué protocolo lo sigue ocupando (si Status != Free)
	Status    PortStatus
	Owner     ID // nuevo dueño (respawn) o el fantasma
	OwnerName string
	// Bystander: quién sigue en el puerto sin haberse pedido (Plan.Bystanders),
	// en un puerto que quedó libre PARA LO PEDIDO. Con una dirección pedida,
	// es quien la contesta ahora, si alguien la contesta. Zero si nadie.
	Bystander      ID
	BystanderName  string
	Supervisor     ID // quién relanza el respawn (el ancestro común); Zero si no se supo
	SupervisorName string
	// Starting (con PortRespawned): el relanzado (Owner) todavía no escucha;
	// el puerto está libre en este instante pero lo están por volver a tomar.
	Starting bool
	Heirs    []ID // fantasma: hijos candidatos a una nueva ronda
	Reserved ports.ExcludedRange
	// Dynamic: el rango dinámico de ese protocolo y familia, cuando es parte
	// de la explicación (arranca antes del 49152 y por eso WinNAT reserva
	// entre los puertos de desarrollo). El valor cero si no viene al caso.
	Dynamic ports.DynamicRange
	Reason  string // por qué quedó así (la negativa, el error, una salvedad), en español; "" si no hace falta
	Advice  string // consejo en español, que la UI muestra tal cual
	// Remedies: lo que se puede hacer, en orden de preferencia, cuando hay más
	// de un camino (un puerto que reserva Windows). Cada uno es un comando y
	// lo que cuesta; la UI los lista debajo del consejo.
	Remedies []Remedy
}

// RemedyKind es qué clase de salida es. Las que son acciones de killport
// (reservar un puerto, devolver el rango dinámico) no traen el texto del
// comando: lo arma la herramienta junto a la definición de sus flags, así un
// flag que cambia de nombre no deja a este paquete aconsejando un comando que
// ya no existe (y el compilador no se enteraría).
type RemedyKind uint8

const (
	RemedySteps      RemedyKind = iota // comandos de Windows, tal cual (Steps)
	RemedyReserve                      // reservar ese puerto para el usuario (Port, Proto)
	RemedyFixDynamic                   // devolver el rango dinámico al de fábrica
)

// Remedy es una salida posible para un puerto que no se libera cerrando un
// proceso.
type Remedy struct {
	Kind  RemedyKind
	Port  uint16      // RemedyReserve
	Proto ports.Proto // RemedyReserve: el protocolo tapado (el que hay que reservar)
	// Steps (RemedySteps): los comandos, en orden. Van JUNTOS: uno solo puede
	// dejar la máquina peor que antes (`net stop winnat` sin el `net start`
	// deja sin red a WSL, Docker y Hyper-V).
	Steps  []string
	Effect string // qué hace y qué cuesta, en español
}

// Report es el resultado completo de una corrida del motor.
type Report struct {
	Victims   []VictimResult
	Externals []ExternalResult // objetivos que no son procesos de Windows (contenedores, procesos Linux)
	Ports     []PortResult
	Canceled  bool // el usuario cortó con Ctrl+C a mitad de la escalada
	// Reached son los que recibieron el Ctrl+C sin ser víctimas: los
	// ancestros que compartían la consola con una (Event.Also).
	Reached []Proc
}

// FreedPorts cuenta cuántos puertos pedidos quedaron libres.
func (r Report) FreedPorts() (free, total int) {
	for _, p := range r.Ports {
		total++
		if p.Status == PortFree {
			free++
		}
	}
	return free, total
}
