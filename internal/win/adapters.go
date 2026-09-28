package win

// adapters.go interpreta el búfer de GetAdaptersAddresses: las interfaces de
// red con su nombre visible ("Wi-Fi", "vEthernet (WSL)") y sus direcciones.
// Sirve para decir el ALCANCE de un socket atado a una dirección concreta: no
// "192.168.1.5" sino "Wi-Fi". Sin build tag: se prueba con bytes sintéticos.
//
// El búfer es una lista enlazada por PUNTEROS (no por desplazamientos, como
// la de procesos) que apuntan dentro del mismo búfer:
//
//	IP_ADAPTER_ADDRESSES ──Next──▶ IP_ADAPTER_ADDRESSES ──▶ … ──▶ NULL
//	   └─FirstUnicastAddress──▶ IP_ADAPTER_UNICAST_ADDRESS ──Next──▶ … ──▶ NULL
//	                               └─Address.lpSockaddr──▶ SOCKADDR_IN / SOCKADDR_IN6
//
// Cada puntero se resuelve contra la base del búfer y se valida como una
// ventana más (rawbuf.go). Una lista que se muerde la cola no se sigue: un
// nodo ya visitado es un error, y hay un tope de nodos, así que el recorrido
// termina siempre, en O(nodos).

import (
	"net/netip"
	"strconv"
)

// Interface es una interfaz de red y sus direcciones unicast.
type Interface struct {
	Index uint32 // IfIndex (IPv4) o, si es 0, Ipv6IfIndex
	Name  string // el nombre visible (FriendlyName): "Wi-Fi", "Ethernet 2"
	Addrs []netip.Addr
}

// Desplazamientos de IP_ADAPTER_ADDRESSES_LH e IP_ADAPTER_UNICAST_ADDRESS_LH en
// 64 bits (verificados contra Get-NetIPAddress en la máquina).
const (
	iaaLength       = 0x00 // ULONG
	iaaIfIndex      = 0x04 // DWORD
	iaaNext         = 0x08 // PIP_ADAPTER_ADDRESSES
	iaaFirstUnicast = 0x18 // PIP_ADAPTER_UNICAST_ADDRESS
	iaaFriendlyName = 0x48 // PWCHAR
	iaaIpv6IfIndex  = 0x6C // IF_INDEX
	iaaMinSize      = 0x70 // lo que se lee de cada adaptador

	iuaNext         = 0x08 // PIP_ADAPTER_UNICAST_ADDRESS
	iuaSockaddr     = 0x10 // SOCKET_ADDRESS.lpSockaddr
	iuaSockLen      = 0x18 // SOCKET_ADDRESS.iSockaddrLength (INT)
	iuaMinSize      = 0x20
	afInet          = 2
	afInet6         = 23
	sockaddrIn6Size = 28 // sizeof(SOCKADDR_IN6)
	sockaddrIn4Size = 16 // sizeof(SOCKADDR_IN)
	maxAdapters     = 4096
	maxUnicastsA    = 4096
)

// parseAdapters recorre la lista. base es la dirección de b[0] cuando el
// sistema llenó el búfer.
func parseAdapters(b []byte, base uintptr) ([]Interface, error) {
	var out []Interface
	if len(b) == 0 {
		return nil, nil
	}
	off, seen := 0, map[int]bool{}
	for n := 0; ; n++ {
		if n == maxAdapters {
			return nil, malformed("adaptadores: más de %d", maxAdapters)
		}
		if seen[off] {
			return nil, malformed("adaptadores: la lista vuelve al adaptador en %d", off)
		}
		seen[off] = true
		e, err := window(b, off, iaaMinSize, "adaptador")
		if err != nil {
			return nil, err
		}
		if int(le32(e, iaaLength)) < iaaMinSize {
			return nil, malformed("adaptador en %d: Length %d menor que %d", off, le32(e, iaaLength), iaaMinSize)
		}
		name, err := cString16(b, base, le64(e, iaaFriendlyName), "nombre del adaptador")
		if err != nil {
			return nil, err
		}
		it := Interface{Index: le32(e, iaaIfIndex), Name: name}
		if it.Index == 0 {
			it.Index = le32(e, iaaIpv6IfIndex)
		}
		if it.Addrs, err = parseUnicasts(b, base, le64(e, iaaFirstUnicast)); err != nil {
			return nil, err
		}
		out = append(out, it)
		next := le64(e, iaaNext)
		if next == 0 {
			return out, nil
		}
		if next < uint64(base) || next-uint64(base) > uint64(len(b)) {
			return nil, malformed("adaptador en %d: Next 0x%x fuera del búfer", off, next)
		}
		off = int(next - uint64(base))
	}
}

// parseUnicasts recorre la lista de direcciones de un adaptador.
func parseUnicasts(b []byte, base uintptr, ptr uint64) ([]netip.Addr, error) {
	var out []netip.Addr
	seen := map[uint64]bool{}
	for n := 0; ptr != 0; n++ {
		if n == maxUnicastsA {
			return nil, malformed("direcciones: más de %d", maxUnicastsA)
		}
		if seen[ptr] {
			return nil, malformed("direcciones: la lista vuelve a 0x%x", ptr)
		}
		seen[ptr] = true
		u, err := pointee(b, base, ptr, iuaMinSize, "dirección del adaptador")
		if err != nil {
			return nil, err
		}
		if a, ok, err := parseSockaddr(b, base, le64(u, iuaSockaddr), int(int32(le32(u, iuaSockLen)))); err != nil {
			return nil, err
		} else if ok {
			out = append(out, a)
		}
		ptr = le64(u, iuaNext)
	}
	return out, nil
}

// parseSockaddr lee un SOCKADDR_IN o SOCKADDR_IN6. ok=false para otras
// familias (se ignoran). En IPv6, un scope_id distinto de cero es la zona
// (el índice de la interfaz), como en la tabla de sockets.
func parseSockaddr(b []byte, base uintptr, ptr uint64, size int) (netip.Addr, bool, error) {
	if ptr == 0 || size < 2 {
		return netip.Addr{}, false, nil
	}
	head, err := pointee(b, base, ptr, 2, "sockaddr")
	if err != nil {
		return netip.Addr{}, false, err
	}
	switch le16(head, 0) {
	case afInet:
		if size < sockaddrIn4Size {
			return netip.Addr{}, false, malformed("SOCKADDR_IN de %d bytes", size)
		}
		sa, err := pointee(b, base, ptr, sockaddrIn4Size, "SOCKADDR_IN")
		if err != nil {
			return netip.Addr{}, false, err
		}
		return netip.AddrFrom4([4]byte(sa[4:8])), true, nil
	case afInet6:
		if size < sockaddrIn6Size {
			return netip.Addr{}, false, malformed("SOCKADDR_IN6 de %d bytes", size)
		}
		sa, err := pointee(b, base, ptr, sockaddrIn6Size, "SOCKADDR_IN6")
		if err != nil {
			return netip.Addr{}, false, err
		}
		a := netip.AddrFrom16([16]byte(sa[8:24]))
		if scope := le32(sa, 24); scope != 0 {
			a = a.WithZone(strconv.FormatUint(uint64(scope), 10))
		}
		return a, true, nil
	}
	return netip.Addr{}, false, nil
}
