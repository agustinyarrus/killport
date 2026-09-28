//go:build windows

package win

// bindprobe.go: averiguar si un puerto se puede usar, sin dejar nada abierto ni
// disparar el aviso del Firewall. Se crea un socket y se hace bind SIN listen:
// bind ya falla si el puerto está tomado o reservado, y sin listen Windows no
// registra al proceso como servidor, así que el Firewall no pregunta nada.
//
// El bind es EXCLUSIVO (SO_EXCLUSIVEADDRUSE). Un bind común a la dirección
// comodín no choca con un socket atado a una dirección específica: sondear
// 0.0.0.0:3000 daba "libre" con un servidor escuchando en 127.0.0.1:3000, y
// [::]:3000 contra [::1]:3000 también. Con uso exclusivo, el bind a la
// comodín falla si CUALQUIER socket tiene ese puerto, en la dirección que
// sea: es la pregunta que hay que hacer ("¿está libre de verdad?").
//
// En IPv6 se fija además IPV6_V6ONLY. Un socket de doble pila ata también el
// lado IPv4, y un bind exclusivo de doble pila contra un IPv4 en loopback da
// WSAEACCES: se leería "lo reserva Windows" donde hay un servidor común y
// corriente. Cada familia se sondea por lo suyo.
//
// Lo que el uso exclusivo cambia para quien lee el resultado: WSAEACCES deja
// de significar solo "rango excluido". También lo da un socket ajeno que tiene
// el puerto con SO_REUSEADDR, o uno de otro usuario. Por eso killport no
// decide con el sondeo: la tabla de sockets es la fuente primaria y el sondeo
// es el desempate de un puerto que ninguna tabla atribuye a nadie.

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	procWSAStartup  = modws2_32.NewProc("WSAStartup")
	procSocket      = modws2_32.NewProc("socket")
	procBind        = modws2_32.NewProc("bind")
	procSetsockopt  = modws2_32.NewProc("setsockopt")
	procCloseSocket = modws2_32.NewProc("closesocket")
)

// ws2_32 no es KnownDLL; se carga por ruta absoluta de System32 como el resto.
var modws2_32 = systemDLL("ws2_32.dll")

const (
	sockStream = 1 // SOCK_STREAM (TCP)
	sockDgram  = 2 // SOCK_DGRAM (UDP)
	ipprotoTCP = 6
	ipprotoUDP = 17

	solSocket          = 0xFFFF     // SOL_SOCKET
	soExclusiveAddrUse = 0xFFFFFFFB // SO_EXCLUSIVEADDRUSE = ~SO_REUSEADDR, como int de 32 bits
	ipprotoIPv6        = 41         // IPPROTO_IPV6
	ipv6V6Only         = 27         // IPV6_V6ONLY

	invalidSocket = ^uintptr(0)

	wsaEACCES        = 10013 // WSAEACCES: prohibido (rango excluido, o tomado por quien no comparte)
	wsaEADDRINUSE    = 10048 // WSAEADDRINUSE: en uso
	wsaEADDRNOTAVAIL = 10049
)

// BindStatus es el resultado de sondear un puerto.
type BindStatus int

const (
	// BindFree: el puerto está libre para atar, en todas las direcciones de
	// esa familia.
	BindFree BindStatus = iota
	// BindInUse: ya hay algo atado ahí (WSAEADDRINUSE), en la dirección que sea.
	BindInUse
	// BindForbidden: el sistema no deja atarlo (WSAEACCES): cae en un rango
	// excluido (WinNAT/Hyper-V/WSL/Docker) o reservado, o lo tiene un socket
	// que no se deja compartir. Quien llama desempata con las tablas.
	BindForbidden
)

func (s BindStatus) String() string {
	switch s {
	case BindFree:
		return "libre"
	case BindInUse:
		return "en uso"
	case BindForbidden:
		return "prohibido"
	}
	return "desconocido"
}

var wsaStarted = func() error {
	// WSAStartup(MAKEWORD(2,2), &data). WSADATA mide 408 bytes en x64; con un
	// búfer holgado alcanza y no hace falta leerlo.
	var data [512]byte
	if r, _ := call(procWSAStartup, 0x0202, uintptr(unsafe.Pointer(&data[0]))); r != 0 {
		return fmt.Errorf("no pude inicializar Winsock: código %d", r)
	}
	return nil
}()

// sockaddrIn4 es SOCKADDR_IN (IPv4): familia, puerto en orden de red, IP, ceros.
type sockaddrIn4 struct {
	family uint16
	port   uint16 // orden de red
	addr   [4]byte
	zero   [8]byte
}

// sockaddrIn6 es SOCKADDR_IN6 (IPv6).
type sockaddrIn6 struct {
	family   uint16
	port     uint16
	flowinfo uint32
	addr     [16]byte
	scopeID  uint32
}

// ProbeBind sondea el puerto en 0.0.0.0 (o [::]) para el protocolo dado, con
// uso exclusivo. tcp=true prueba TCP; false, UDP. ipv6=true usa [::] (solo
// IPv6, sin doble pila), si no 0.0.0.0. No deja el socket abierto: lo cierra
// siempre. Si no se pudo pedir el uso exclusivo devuelve error: un sondeo
// común contestaría otra pregunta.
func ProbeBind(portNum uint16, tcp, ipv6 bool) (BindStatus, error) {
	if wsaStarted != nil {
		return BindFree, wsaStarted
	}
	af, typ, proto := uintptr(afINET), uintptr(sockDgram), uintptr(ipprotoUDP)
	if ipv6 {
		af = afINET6
	}
	if tcp {
		typ, proto = sockStream, ipprotoTCP
	}
	s, e := call(procSocket, af, typ, proto)
	if s == invalidSocket {
		return BindFree, fmt.Errorf("no pude crear el socket de sondeo: %w", lastError(e))
	}
	defer call(procCloseSocket, s)

	if err := setSockOptBool(s, solSocket, soExclusiveAddrUse); err != nil {
		return BindFree, fmt.Errorf("no pude pedir el uso exclusivo del puerto %d: %w", portNum, err)
	}
	if ipv6 {
		if err := setSockOptBool(s, ipprotoIPv6, ipv6V6Only); err != nil {
			return BindFree, fmt.Errorf("no pude sondear el puerto %d solo en IPv6: %w", portNum, err)
		}
	}

	var sa unsafe.Pointer
	var salen uintptr
	netPort := portNum<<8 | portNum>>8 // a orden de red
	if ipv6 {
		v6 := sockaddrIn6{family: afINET6, port: netPort}
		sa, salen = unsafe.Pointer(&v6), unsafe.Sizeof(v6)
	} else {
		v4 := sockaddrIn4{family: afINET, port: netPort}
		sa, salen = unsafe.Pointer(&v4), unsafe.Sizeof(v4)
	}

	r, e := call(procBind, s, uintptr(sa), salen)
	// bind devuelve 0 al atar y SOCKET_ERROR al fallar. OJO: SOCKET_ERROR es
	// -1 como int de 32 bits, y la syscall lo entrega como 0xFFFFFFFF en los 32
	// bits bajos de un registro de 64 — NO extendido con signo. Por eso el
	// fallo se detecta con r != 0 (cualquier no-cero), no comparando con -1.
	if r != 0 {
		switch e {
		case wsaEADDRINUSE:
			return BindInUse, nil
		case wsaEACCES:
			return BindForbidden, nil
		default:
			return BindFree, fmt.Errorf("sondeo del puerto %d: %w", portNum, wsaError(e))
		}
	}
	return BindFree, nil
}

// setSockOptBool enciende una opción booleana del socket (un DWORD en 1).
func setSockOptBool(s uintptr, level, name uintptr) error {
	on := uint32(1)
	if r, e := call(procSetsockopt, s, level, name, uintptr(unsafe.Pointer(&on)), 4); r != 0 {
		return wsaError(e)
	}
	return nil
}

// wsaError envuelve un código de Winsock (los errno de bind salen por el valor
// de retorno de WSAGetLastError, que syscall ya deja en e).
func wsaError(e syscall.Errno) error {
	if e == 0 {
		return fmt.Errorf("error de Winsock desconocido")
	}
	return e
}
