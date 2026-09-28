package ports

import (
	"net/netip"
	"strconv"
	"time"
)

// Proto es el protocolo de transporte de un socket.
type Proto uint8

const (
	TCP Proto = iota + 1
	UDP
)

func (p Proto) String() string {
	switch p {
	case TCP:
		return "TCP"
	case UDP:
		return "UDP"
	}
	return "Proto(" + strconv.Itoa(int(p)) + ")"
}

// Family es la familia de direcciones: las tablas del sistema, los rangos
// excluidos y el sondeo de bind van por separado para IPv4 e IPv6.
type Family uint8

const (
	IPv4 Family = iota + 1
	IPv6
)

func (f Family) String() string {
	switch f {
	case IPv4:
		return "IPv4"
	case IPv6:
		return "IPv6"
	}
	return "Family(" + strconv.Itoa(int(f)) + ")"
}

// State es el estado TCP con los valores de MIB_TCP_STATE (1..12), tal cual
// vienen en la tabla del sistema. UDP no tiene estado: StateNone.
type State uint8

const (
	StateNone   State = 0
	Closed      State = 1
	Listen      State = 2
	SynSent     State = 3
	SynRcvd     State = 4
	Established State = 5
	FinWait1    State = 6
	FinWait2    State = 7
	CloseWait   State = 8
	Closing     State = 9
	LastAck     State = 10
	TimeWait    State = 11
	DeleteTCB   State = 12
)

var stateNames = [...]struct{ mib, label string }{
	StateNone:   {"", ""},
	Closed:      {"CLOSED", "cerrado"},
	Listen:      {"LISTEN", "escuchando"},
	SynSent:     {"SYN_SENT", "conectando"},
	SynRcvd:     {"SYN_RCVD", "aceptando"},
	Established: {"ESTABLISHED", "conectado"},
	FinWait1:    {"FIN_WAIT_1", "cerrando"},
	FinWait2:    {"FIN_WAIT_2", "cerrando"},
	CloseWait:   {"CLOSE_WAIT", "el otro extremo cerró"},
	Closing:     {"CLOSING", "cerrando"},
	LastAck:     {"LAST_ACK", "cerrando"},
	TimeWait:    {"TIME_WAIT", "TIME_WAIT"},
	DeleteTCB:   {"DELETE_TCB", "borrándose"},
}

// Valid dice si el valor es uno de MIB_TCP_STATE o StateNone.
func (s State) Valid() bool { return int(s) < len(stateNames) }

// String es el nombre de la RFC 793 / MIB ("LISTEN", "TIME_WAIT"), el que
// reconoce cualquiera que haya leído un netstat.
func (s State) String() string {
	if !s.Valid() {
		return "State(" + strconv.Itoa(int(s)) + ")"
	}
	return stateNames[s].mib
}

// Label es el nombre para mostrar, en español: "escuchando", "conectado".
// TIME_WAIT queda en inglés: es un término técnico sin traducción que ayude.
func (s State) Label() string {
	if !s.Valid() {
		return s.String()
	}
	return stateNames[s].label
}

// Socket es una fila de las tablas de TCP/UDP del sistema.
type Socket struct {
	Proto  Proto
	Local  netip.AddrPort // IPv6 con zona si el scope no es 0
	Remote netip.AddrPort // solo TCP conectado; cero en LISTEN y en UDP
	State  State          // solo TCP; StateNone en UDP
	PID    uint32         // dueño según la tabla: el proceso que CREÓ el socket
	// Since es desde cuándo existe el socket (liCreateTimestamp); cero si el
	// sistema no lo informó. Sirve para desenmascarar PID reciclados: un
	// proceso nacido DESPUÉS del socket no puede ser quien lo creó.
	Since time.Time
	// Service es el módulo dueño que informa GetOwnerModuleFrom*Entry. En un
	// svchost es el NOMBRE DEL SERVICIO (así sabe netstat -b cuál de los
	// servicios compartidos tiene el puerto); "" si no se supo.
	Service string
	// Ephemeral (solo UDP): el puerto lo eligió el sistema (bind al 0, o el
	// bind implícito de un sendto), no el programa. Un servidor UDP siempre
	// pide SU puerto; los efímeros son clientes (DNS, QUIC, mDNS de ida) y en
	// el listado general son ruido. Es el bit SpecificPortBind de la tabla,
	// negado (verificado: 1 con puerto explícito, 0 con ":0" y con Dial). El
	// valor cero (false) es el caso que se muestra, así que un Socket armado a
	// mano sin este dato no se pierde.
	Ephemeral bool
}

// Port es el puerto local.
func (s Socket) Port() uint16 { return s.Local.Port() }

// Family dice si el socket es IPv4 o IPv6 (una IPv4 mapeada en IPv6 cuenta
// como IPv6: así la informa la tabla de IPv6).
func (s Socket) Family() Family {
	if s.Local.Addr().Is4() {
		return IPv4
	}
	return IPv6
}

// Relation es qué tiene que ver un socket con lo que se pidió.
type Relation uint8

const (
	// Unrelated: otro puerto, otro protocolo, o un estado que no cuenta.
	Unrelated Relation = iota
	// Holds: ocupa el puerto y tiene un dueño a quien cerrar.
	Holds
	// Residue: resto de una conexión que ya no tiene dueño (TIME_WAIT y
	// compañía, que la tabla le atribuye al PID 0). Se informa; no hay a
	// quién cerrar, y el kernel lo borra solo al vencer el temporizador.
	Residue
)

// Filter describe qué sockets importan.
type Filter struct {
	Ports Set // vacío = todos los puertos (el listado general)
	// TCP y UDP eligen protocolos; los dos en false equivale a los dos en true
	// (así el valor cero de Filter es "todo", como --tcp/--udp sin usar).
	TCP, UDP bool
	// AllStates suma las conexiones cuyo puerto LOCAL es el pedido, en
	// cualquier estado (--all-states). Sin él, solo cuenta lo que ocupa el
	// puerto para un servidor nuevo: TCP en LISTEN y cualquier UDP.
	AllStates bool
}

// Wants dice si el filtro incluye el protocolo p.
func (f Filter) Wants(p Proto) bool {
	if !f.TCP && !f.UDP {
		return true
	}
	return (p == TCP && f.TCP) || (p == UDP && f.UDP)
}

// Relate clasifica un socket. Solo mira el puerto LOCAL: un cliente conectado
// A un 3000 remoto no ocupa el 3000 de esta máquina.
func (f Filter) Relate(s Socket) Relation {
	if !f.Wants(s.Proto) || (!f.Ports.IsEmpty() && !f.Ports.Contains(s.Port())) {
		return Unrelated
	}
	switch {
	case s.Proto == UDP:
		return Holds
	case s.State == Listen:
		return Holds
	case !f.AllStates:
		return Unrelated
	case s.PID == 0:
		return Residue
	}
	return Holds
}
