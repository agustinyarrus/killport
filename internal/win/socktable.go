package win

// socktable.go interpreta las tablas de sockets de GetExtendedTcpTable y
// GetExtendedUdpTable con las clases OWNER_MODULE (las que traen, además del
// PID, la hora de creación del socket y los datos para preguntar qué módulo o
// servicio lo abrió). Sin build tag: se prueba con bytes sintéticos.
//
// Layout de una tabla (verificado en esta máquina: tamaño de fila =
// (bytes-8)/filas, y cada campo contra Get-NetTCPConnection y netstat):
//
//	DWORD dwNumEntries
//	4 bytes de relleno   ← las filas tienen campos de 64 bits: arrancan en 8
//	fila[0] fila[1] …    ← TCP4 160 · TCP6 192 · UDP4 160 · UDP6 176 bytes
//
// Trampas de Windows que esquiva:
//   - el puerto es un DWORD cuyos 2 bytes BAJOS guardan el puerto en orden de
//     red, y los 2 altos pueden traer basura: port = b[0]<<8 | b[1], nada más;
//   - la dirección IPv4 está en orden de red tal cual en memoria (un DWORD
//     leído como entero la daría vuelta);
//   - el scope de IPv6 es un DWORD común (orden de la máquina): 12 es "%12".

import (
	"net/netip"
	"strconv"
	"time"
)

// Proto es el protocolo de un socket.
type Proto uint8

const (
	TCP Proto = 1
	UDP Proto = 2
)

func (p Proto) String() string {
	switch p {
	case TCP:
		return "TCP"
	case UDP:
		return "UDP"
	}
	return "proto(" + strconv.Itoa(int(p)) + ")"
}

// Estados de una conexión TCP (MIB_TCP_STATE), tal como los numera la API.
const (
	TCPClosed      = 1
	TCPListen      = 2
	TCPSynSent     = 3
	TCPSynRcvd     = 4
	TCPEstablished = 5
	TCPFinWait1    = 6
	TCPFinWait2    = 7
	TCPCloseWait   = 8
	TCPClosing     = 9
	TCPLastAck     = 10
	TCPTimeWait    = 11
	TCPDeleteTCB   = 12
)

// Socket es una fila de las tablas de sockets del sistema.
type Socket struct {
	Proto  Proto
	Local  netip.AddrPort // IPv6 con zona ("%12") si el scope no es 0
	Remote netip.AddrPort // solo TCP; en UDP es el valor cero (!Remote.IsValid())
	State  uint32         // MIB_TCP_STATE (TCPListen…); 0 en UDP
	PID    uint32         // proceso dueño; 0 para las conexiones que ya no tienen (TIME_WAIT)
	// Created es desde cuándo existe el socket (liCreateTimestamp). Cero si el
	// sistema no lo informa: pasa con TIME_WAIT, que ya no tiene dueño.
	Created time.Time
	// SpecificPortBind (solo UDP): el socket se ató pidiendo ESE puerto, no uno
	// efímero que eligió el sistema (bit 0 de dwFlags).
	SpecificPortBind bool

	// row es la fila cruda tal cual la devolvió el sistema: las funciones
	// GetOwnerModuleFrom*Entry la piden entera (usan el PID, la hora y los
	// OwningModuleInfo opacos). Apunta al búfer de la tabla, alineado a 8.
	row []byte
}

// IPv6 dice si el socket salió de una tabla IPv6.
func (s Socket) IPv6() bool { return s.Local.Addr().Is6() }

// Tamaños de fila (sizeof de las estructuras MIB_*ROW_OWNER_MODULE en C).
const (
	tableHeader = 8 // dwNumEntries + relleno hasta la alineación de 8 de las filas
	tcp4RowSize = 160
	tcp6RowSize = 192
	udp4RowSize = 160
	udp6RowSize = 176
)

// MIB_TCPROW_OWNER_MODULE (160 bytes).
const (
	tcp4State      = 0  // DWORD dwState
	tcp4LocalAddr  = 4  // DWORD dwLocalAddr (orden de red)
	tcp4LocalPort  = 8  // DWORD dwLocalPort (2 bytes bajos, orden de red)
	tcp4RemoteAddr = 12 // DWORD dwRemoteAddr
	tcp4RemotePort = 16 // DWORD dwRemotePort
	tcp4PID        = 20 // DWORD dwOwningPid
	tcp4Created    = 24 // LARGE_INTEGER liCreateTimestamp (ya alineado a 8)
	tcp4Module     = 32 // OwningModuleInfo: 32 + 128 = 160
)

// MIB_TCP6ROW_OWNER_MODULE (192 bytes).
const (
	tcp6LocalAddr   = 0  // UCHAR ucLocalAddr[16]
	tcp6LocalScope  = 16 // DWORD dwLocalScopeId
	tcp6LocalPort   = 20 // DWORD dwLocalPort
	tcp6RemoteAddr  = 24 // UCHAR ucRemoteAddr[16]
	tcp6RemoteScope = 40 // DWORD dwRemoteScopeId
	tcp6RemotePort  = 44 // DWORD dwRemotePort
	tcp6State       = 48 // DWORD dwState
	tcp6PID         = 52 // DWORD dwOwningPid
	tcp6Created     = 56 // LARGE_INTEGER liCreateTimestamp
	tcp6Module      = 64 // OwningModuleInfo: 64 + 128 = 192
)

// MIB_UDPROW_OWNER_MODULE (160 bytes).
const (
	udp4LocalAddr = 0  // DWORD dwLocalAddr
	udp4LocalPort = 4  // DWORD dwLocalPort
	udp4PID       = 8  // DWORD dwOwningPid, + 4 de relleno
	udp4Created   = 16 // LARGE_INTEGER liCreateTimestamp
	udp4Flags     = 24 // int dwFlags (bit 0: SpecificPortBind), + 4 de relleno
	udp4Module    = 32 // OwningModuleInfo: 32 + 128 = 160
)

// MIB_UDP6ROW_OWNER_MODULE (176 bytes).
const (
	udp6LocalAddr  = 0  // UCHAR ucLocalAddr[16]
	udp6LocalScope = 16 // DWORD dwLocalScopeId
	udp6LocalPort  = 20 // DWORD dwLocalPort
	udp6PID        = 24 // DWORD dwOwningPid, + 4 de relleno
	udp6Created    = 32 // LARGE_INTEGER liCreateTimestamp
	udp6Flags      = 40 // int dwFlags, + 4 de relleno
	udp6Module     = 48 // OwningModuleInfo: 48 + 128 = 176
)

// rowLayout une el tamaño de una fila con su intérprete.
type rowLayout struct {
	name  string
	size  int
	parse func(row []byte) Socket
}

var (
	layoutTCP4 = rowLayout{"TCP4", tcp4RowSize, parseTCP4Row}
	layoutTCP6 = rowLayout{"TCP6", tcp6RowSize, parseTCP6Row}
	layoutUDP4 = rowLayout{"UDP4", udp4RowSize, parseUDP4Row}
	layoutUDP6 = rowLayout{"UDP6", udp6RowSize, parseUDP6Row}
)

// parseSocketTable recorre una tabla completa. O(n) en filas. La fila i vive
// en [8 + i·size, 8 + (i+1)·size); se valida de una vez que las n entren (en
// aritmética de 64 bits: n viene del búfer y n·size podría desbordar un int
// de 32). Los bytes de más al final (la holgura del búfer) se ignoran.
func parseSocketTable(b []byte, l rowLayout) ([]Socket, error) {
	if len(b) < 4 {
		return nil, malformed("tabla %s: %d bytes no alcanzan ni para la cantidad de filas", l.name, len(b))
	}
	n := le32(b, 0)
	if n == 0 {
		return nil, nil // tabla vacía: el sistema puede devolver solo el DWORD
	}
	need := uint64(tableHeader) + uint64(n)*uint64(l.size)
	if need > uint64(len(b)) {
		return nil, malformed("tabla %s truncada: dice %d filas de %d bytes (%d en total) y hay %d bytes",
			l.name, n, l.size, need, len(b))
	}
	out := make([]Socket, n)
	for i := range out {
		off := tableHeader + i*l.size
		out[i] = l.parse(b[off : off+l.size : off+l.size])
	}
	return out, nil
}

// port lee el puerto: 2 bytes bajos del DWORD, en orden de red.
func port(row []byte, off int) uint16 { return be16(row, off) }

// addr4 lee una IPv4 guardada en orden de red.
func addr4(row []byte, off int) netip.Addr { return netip.AddrFrom4([4]byte(row[off : off+4])) }

// addr6 lee una IPv6 y le pone la zona numérica si el scope no es 0 (así la
// muestran netstat y Get-NetTCPConnection: fe80::1%12).
func addr6(row []byte, off, scopeOff int) netip.Addr {
	a := netip.AddrFrom16([16]byte(row[off : off+16]))
	if scope := le32(row, scopeOff); scope != 0 {
		a = a.WithZone(strconv.FormatUint(uint64(scope), 10))
	}
	return a
}

func parseTCP4Row(r []byte) Socket {
	return Socket{
		Proto:   TCP,
		Local:   netip.AddrPortFrom(addr4(r, tcp4LocalAddr), port(r, tcp4LocalPort)),
		Remote:  netip.AddrPortFrom(addr4(r, tcp4RemoteAddr), port(r, tcp4RemotePort)),
		State:   le32(r, tcp4State),
		PID:     le32(r, tcp4PID),
		Created: fileTime(int64(le64(r, tcp4Created))),
		row:     r,
	}
}

func parseTCP6Row(r []byte) Socket {
	return Socket{
		Proto:   TCP,
		Local:   netip.AddrPortFrom(addr6(r, tcp6LocalAddr, tcp6LocalScope), port(r, tcp6LocalPort)),
		Remote:  netip.AddrPortFrom(addr6(r, tcp6RemoteAddr, tcp6RemoteScope), port(r, tcp6RemotePort)),
		State:   le32(r, tcp6State),
		PID:     le32(r, tcp6PID),
		Created: fileTime(int64(le64(r, tcp6Created))),
		row:     r,
	}
}

func parseUDP4Row(r []byte) Socket {
	return Socket{
		Proto:            UDP,
		Local:            netip.AddrPortFrom(addr4(r, udp4LocalAddr), port(r, udp4LocalPort)),
		PID:              le32(r, udp4PID),
		Created:          fileTime(int64(le64(r, udp4Created))),
		SpecificPortBind: le32(r, udp4Flags)&1 != 0,
		row:              r,
	}
}

func parseUDP6Row(r []byte) Socket {
	return Socket{
		Proto:            UDP,
		Local:            netip.AddrPortFrom(addr6(r, udp6LocalAddr, udp6LocalScope), port(r, udp6LocalPort)),
		PID:              le32(r, udp6PID),
		Created:          fileTime(int64(le64(r, udp6Created))),
		SpecificPortBind: le32(r, udp6Flags)&1 != 0,
		row:              r,
	}
}

// OwnerModule es el dueño que informa GetOwnerModuleFrom*Entry: para un
// programa, el ejecutable; para un servicio alojado en svchost, el NOMBRE DEL
// SERVICIO (así sabe netstat -b cuál de los servicios compartidos abrió el
// puerto).
type OwnerModule struct {
	Name string // pModuleName
	Path string // pModulePath (para un servicio, suele repetir el nombre)
}

// ownerModuleBasicSize es sizeof(TCPIP_OWNER_MODULE_BASIC_INFO) en 64 bits:
// dos punteros, a cadenas que el sistema escribe a continuación en el MISMO
// búfer.
const ownerModuleBasicSize = 16

// parseOwnerModule interpreta TCPIP_OWNER_MODULE_BASIC_INFO. base es la
// dirección de b[0] cuando el sistema lo llenó.
func parseOwnerModule(b []byte, base uintptr) (OwnerModule, error) {
	h, err := window(b, 0, ownerModuleBasicSize, "módulo dueño")
	if err != nil {
		return OwnerModule{}, err
	}
	name, err := cString16(b, base, le64(h, 0), "módulo dueño: nombre")
	if err != nil {
		return OwnerModule{}, err
	}
	path, err := cString16(b, base, le64(h, 8), "módulo dueño: ruta")
	if err != nil {
		return OwnerModule{}, err
	}
	return OwnerModule{Name: name, Path: path}, nil
}
