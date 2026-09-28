//go:build windows

package win

// sockets.go lee las tablas de sockets del sistema (lo que muestra netstat
// -ano) y pregunta el módulo dueño de cada uno (lo que agrega netstat -b).
// El formato de las filas se interpreta en socktable.go, sin build tag.

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	procGetExtendedTcpTable         = modiphlpapi.NewProc("GetExtendedTcpTable")
	procGetExtendedUdpTable         = modiphlpapi.NewProc("GetExtendedUdpTable")
	procGetOwnerModuleFromTcpEntry  = modiphlpapi.NewProc("GetOwnerModuleFromTcpEntry")
	procGetOwnerModuleFromTcp6Entry = modiphlpapi.NewProc("GetOwnerModuleFromTcp6Entry")
	procGetOwnerModuleFromUdpEntry  = modiphlpapi.NewProc("GetOwnerModuleFromUdpEntry")
	procGetOwnerModuleFromUdp6Entry = modiphlpapi.NewProc("GetOwnerModuleFromUdp6Entry")
)

const (
	afINET  = 2
	afINET6 = 23

	tcpTableOwnerModuleListener = 6 // TCP_TABLE_OWNER_MODULE_LISTENER: solo lo que escucha
	tcpTableOwnerModuleAll      = 8 // TCP_TABLE_OWNER_MODULE_ALL: escuchando y conectadas
	udpTableOwnerModule         = 2 // UDP_TABLE_OWNER_MODULE

	tcpipOwnerModuleInfoBasic = 0 // TCPIP_OWNER_MODULE_INFO_BASIC

	errInsufficientBuffer = syscall.Errno(122) // ERROR_INSUFFICIENT_BUFFER
	maxTableAttempts      = 16
)

// TCPSockets devuelve la tabla TCP (todas las conexiones, en cualquier estado)
// de IPv4 o de IPv6.
func TCPSockets(ipv6 bool) ([]Socket, error) {
	if ipv6 {
		return readSocketTable(procGetExtendedTcpTable, afINET6, tcpTableOwnerModuleAll, layoutTCP6)
	}
	return readSocketTable(procGetExtendedTcpTable, afINET, tcpTableOwnerModuleAll, layoutTCP4)
}

// UDPSockets devuelve la tabla UDP de IPv4 o de IPv6.
func UDPSockets(ipv6 bool) ([]Socket, error) {
	if ipv6 {
		return readSocketTable(procGetExtendedUdpTable, afINET6, udpTableOwnerModule, layoutUDP6)
	}
	return readSocketTable(procGetExtendedUdpTable, afINET, udpTableOwnerModule, layoutUDP4)
}

// AllSockets lee las cuatro tablas (TCP4, TCP6, UDP4, UDP6), en ese orden.
// No es una foto atómica: son cuatro lecturas seguidas, cada una consistente
// consigo misma.
func AllSockets() ([]Socket, error) {
	var all []Socket
	for _, read := range []func() ([]Socket, error){
		func() ([]Socket, error) { return TCPSockets(false) },
		func() ([]Socket, error) { return TCPSockets(true) },
		func() ([]Socket, error) { return UDPSockets(false) },
		func() ([]Socket, error) { return UDPSockets(true) },
	} {
		s, err := read()
		if err != nil {
			return nil, err
		}
		all = append(all, s...)
	}
	return all, nil
}

// ListeningSockets lee lo que puede recibir conexiones o datagramas nuevos: la
// tabla TCP de los que ESCUCHAN (sin las conexiones, que en una máquina de
// desarrollo son cientos) y las dos UDP. Mismas filas que AllSockets —con la
// hora de creación de cada socket, que es la clase MODULE; la clase PID no la
// trae—, sin preguntar a nadie por el módulo dueño. Es la lectura barata que
// hace falta para releer la tabla muchas veces seguidas (esperar a que un
// puerto escuche o se libere).
func ListeningSockets() ([]Socket, error) {
	var all []Socket
	for _, read := range []func() ([]Socket, error){
		func() ([]Socket, error) {
			return readSocketTable(procGetExtendedTcpTable, afINET, tcpTableOwnerModuleListener, layoutTCP4)
		},
		func() ([]Socket, error) {
			return readSocketTable(procGetExtendedTcpTable, afINET6, tcpTableOwnerModuleListener, layoutTCP6)
		},
		func() ([]Socket, error) { return UDPSockets(false) },
		func() ([]Socket, error) { return UDPSockets(true) },
	} {
		s, err := read()
		if err != nil {
			return nil, err
		}
		all = append(all, s...)
	}
	return all, nil
}

// readSocketTable llama a GetExtended*Table hasta que el búfer alcance. La
// tabla puede crecer entre la llamada que informa el tamaño y la que la llena
// (se abrió una conexión en el medio): por eso es un bucle, y cada reintento
// pide lo informado más un 25 % y 8 filas de holgura. Con 16 intentos, una
// tabla que crece sin parar termina en error, no en un bucle infinito.
func readSocketTable(p *syscall.LazyProc, af, class uintptr, l rowLayout) ([]Socket, error) {
	size := uint32(tableHeader + 64*l.size)
	for attempt := 0; attempt < maxTableAttempts; attempt++ {
		buf := alignedBuffer(int(size))
		size = uint32(len(buf))
		r, _ := call(p, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, af, class, 0)
		switch syscall.Errno(r) {
		case 0:
			return parseSocketTable(buf, l)
		case errInsufficientBuffer:
			size += size/4 + uint32(8*l.size)
		default:
			return nil, fmt.Errorf("no pude leer la tabla %s: %w", l.name, syscall.Errno(r))
		}
	}
	return nil, fmt.Errorf("no pude leer la tabla %s: no dejó de crecer en %d intentos", l.name, maxTableAttempts)
}

// alignedBuffer devuelve al menos n bytes alineados a 8 (respaldados por un
// []uint64): las filas y estructuras tienen campos de 64 bits y el sistema
// asume esa alineación. make con tamaño variable siempre va al heap, y el
// recolector de Go no mueve objetos del heap: la dirección que ve el sistema
// es la misma que después se usa como base para resolver sus punteros.
func alignedBuffer(n int) []byte {
	words := make([]uint64, max(1, (n+7)/8))
	return unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)
}

// Owner pregunta qué módulo abrió el socket (GetOwnerModuleFrom*Entry con
// TCPIP_OWNER_MODULE_INFO_BASIC). Para un svchost devuelve el nombre del
// servicio. Sin permisos de administrador falla con acceso denegado para los
// procesos de otros usuarios (verificado: ver _oracle); para los propios
// funciona. Si el proceso ya terminó, falla con ERROR_NOT_FOUND.
func (s Socket) Owner() (OwnerModule, error) {
	if len(s.row) == 0 {
		return OwnerModule{}, fmt.Errorf("el socket no viene de una tabla del sistema")
	}
	var p *syscall.LazyProc
	switch {
	case s.Proto == TCP && !s.IPv6():
		p = procGetOwnerModuleFromTcpEntry
	case s.Proto == TCP:
		p = procGetOwnerModuleFromTcp6Entry
	case s.Proto == UDP && !s.IPv6():
		p = procGetOwnerModuleFromUdpEntry
	default:
		p = procGetOwnerModuleFromUdp6Entry
	}
	size := uint32(512)
	for attempt := 0; attempt < 4; attempt++ {
		buf := alignedBuffer(int(size))
		size = uint32(len(buf))
		r, _ := call(p, uintptr(unsafe.Pointer(&s.row[0])), tcpipOwnerModuleInfoBasic,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
		switch syscall.Errno(r) {
		case 0:
			return parseOwnerModule(buf, uintptr(unsafe.Pointer(&buf[0])))
		case errInsufficientBuffer:
			continue // size ya trae lo necesario
		default:
			return OwnerModule{}, fmt.Errorf("no pude saber quién abrió %s %s: %w", s.Proto, s.Local, syscall.Errno(r))
		}
	}
	return OwnerModule{}, fmt.Errorf("no pude saber quién abrió %s %s: el tamaño no se estabilizó", s.Proto, s.Local)
}
