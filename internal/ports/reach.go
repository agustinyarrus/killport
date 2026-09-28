package ports

import (
	"net/netip"
	"sort"
)

// Quién contesta, y hasta dónde llega.
//
// Windows deja que varios sockets escuchen el MISMO puerto en direcciones
// distintas —uno en 127.0.0.1, otro en 0.0.0.0— sin que ninguno dé error, y
// reparte cada conexión según la dirección a la que llega. Las reglas, medidas
// en esta máquina (Windows 11 28120) y no supuestas:
//
//  1. Gana el socket atado EXACTAMENTE a esa dirección.
//  2. Si no hay, el comodín de su familia (0.0.0.0 para IPv4, [::] para
//     IPv6). Un comodín IPv4 no atiende ::1 y uno IPv6 solo no atiende
//     127.0.0.1.
//  3. Un socket de DOBLE PILA (un [::] que también acepta IPv4: lo que hacen
//     node y Go cuando escuchan sin dirección) figura en las tablas como DOS
//     filas, 0.0.0.0 y [::], del mismo PID y con la misma hora de creación al
//     tick. En IPv4 cede ante un 0.0.0.0 nativo, se hayan atado en el orden
//     que sea (18 de 18).
//  4. Dos sockets en la misma dirección (con SO_REUSEADDR): Windows no
//     promete a cuál llega; medido, al que se ató primero. Se devuelven los
//     dos, el más viejo primero, y quien llama dice "reparto indefinido".
//
// Todo es sobre la foto de las tablas: no se abre ni una conexión.

// Reach es hasta dónde llega un socket que escucha.
type Reach uint8

const (
	ReachNone     Reach = iota
	ReachLoopback       // solo esta PC (127.0.0.0/8, ::1)
	ReachIface          // una dirección de una interfaz: quien llegue por esa red
	ReachAll            // todas las interfaces: toda la red (0.0.0.0, [::])
)

// ReachOf clasifica la dirección local de un socket.
func ReachOf(a netip.Addr) Reach {
	a = a.Unmap()
	switch {
	case !a.IsValid():
		return ReachNone
	case a.IsLoopback():
		return ReachLoopback
	case a.IsUnspecified():
		return ReachAll
	}
	return ReachIface
}

// listens dice si el socket escucha en (proto, port): TCP en LISTEN o UDP
// atado. Solo esos reciben conexiones (o datagramas) nuevos.
func listens(s Socket, proto Proto, port uint16) bool {
	return s.Proto == proto && s.Port() == port && (proto == UDP || s.State == Listen)
}

// twinKey identifica un socket por su dueño y su hora de creación: las dos
// filas de un socket de doble pila comparten las dos cosas.
type twinKey struct {
	pid   uint32
	since int64
}

// dualStack marca las filas comodín que son la mitad de un socket de doble
// pila: mismo PID, misma hora de creación (distinta de cero), una en 0.0.0.0
// y la otra en [::]. O(n).
func dualStack(ls []Socket) map[twinKey]bool {
	type halves struct{ v4, v6 bool }
	seen := map[twinKey]*halves{}
	for _, s := range ls {
		if s.Since.IsZero() || !s.Local.Addr().IsUnspecified() {
			continue
		}
		k := twinKey{s.PID, s.Since.UnixNano()}
		h := seen[k]
		if h == nil {
			h = &halves{}
			seen[k] = h
		}
		if s.Local.Addr().Is4() {
			h.v4 = true
		} else {
			h.v6 = true
		}
	}
	out := map[twinKey]bool{}
	for k, h := range seen {
		if h.v4 && h.v6 {
			out[k] = true
		}
	}
	return out
}

// Answer dice qué socket contesta a un cliente que se conecta a addr en
// (proto, port), según las reglas de arriba: los del primer escalón que
// tenga a alguien, el más viejo primero. Vacío si no contesta nadie (el
// cliente recibe "conexión rechazada"). O(n log n) con n los sockets.
func Answer(addr netip.Addr, proto Proto, port uint16, socks []Socket) []Socket {
	addr = addr.Unmap()
	var ls []Socket
	for _, s := range socks {
		if listens(s, proto, port) {
			ls = append(ls, s)
		}
	}
	dual := dualStack(ls)
	isDual := func(s Socket) bool { return dual[twinKey{s.PID, s.Since.UnixNano()}] }
	var tiers [3][]Socket
	for _, s := range ls {
		local := s.Local.Addr()
		switch {
		case sameAddr(local.Unmap(), addr):
			tiers[0] = append(tiers[0], s)
		case !local.IsUnspecified():
		case addr.Is4() && local.Is4() && !isDual(s):
			tiers[1] = append(tiers[1], s)
		case addr.Is4() && local.Is4():
			tiers[2] = append(tiers[2], s)
		case addr.Is6() && local.Is6():
			tiers[1] = append(tiers[1], s)
		}
	}
	for _, t := range tiers {
		if len(t) > 0 {
			return oldestFirst(t)
		}
	}
	return nil
}

// AnswerDest es Answer para el destino de un pedido. at es la dirección que
// terminó contestando: con localhost, ::1 si ahí hubo alguien y si no
// 127.0.0.1 (el navegador prueba en ese orden). Un DestBound elige a lo que
// está atado exactamente en ese comodín (las dos mitades de un doble pila
// incluidas, cada una en su familia).
func AnswerDest(d Dest, proto Proto, port uint16, socks []Socket) (who []Socket, at netip.Addr) {
	switch d.Kind {
	case DestLocalhost:
		v6 := netip.IPv6Loopback()
		if who = Answer(v6, proto, port, socks); len(who) > 0 {
			return who, v6
		}
		v4 := netip.AddrFrom4([4]byte{127, 0, 0, 1})
		return Answer(v4, proto, port, socks), v4
	case DestBound:
		for _, s := range socks {
			if listens(s, proto, port) && s.Local.Addr() == d.Addr {
				who = append(who, s)
			}
		}
		return oldestFirst(who), d.Addr
	case DestAddr:
		return Answer(d.Addr, proto, port, socks), d.Addr
	}
	return nil, netip.Addr{}
}

// AnswerNet es quién contesta a un cliente que llega DESDE LA RED por una
// familia: el que atiende una dirección de la máquina que nadie ató a mano,
// o sea el comodín (con las mismas reglas). Una interfaz atada a mano la
// cuenta ReachIface.
func AnswerNet(fam Family, proto Proto, port uint16, socks []Socket) []Socket {
	// Una dirección de documentación (RFC 5737 / 3849): nadie la ata, así que
	// solo pueden contestarla los comodines.
	probe := netip.AddrFrom4([4]byte{192, 0, 2, 1})
	if fam == IPv6 {
		probe = netip.MustParseAddr("2001:db8::1")
	}
	return Answer(probe, proto, port, socks)
}

// sameAddr compara dos direcciones; la zona de IPv6 (el índice de la
// interfaz de una fe80::) solo cuenta si las dos la traen.
func sameAddr(a, b netip.Addr) bool {
	if a.WithZone("") != b.WithZone("") {
		return false
	}
	return a.Zone() == "" || b.Zone() == "" || a.Zone() == b.Zone()
}

// oldestFirst ordena por hora de creación (la desconocida al final),
// conservando el orden de la tabla ante empates.
func oldestFirst(ss []Socket) []Socket {
	sort.SliceStable(ss, func(i, j int) bool {
		a, b := ss[i].Since, ss[j].Since
		switch {
		case a.IsZero():
			return false
		case b.IsZero():
			return true
		}
		return a.Before(b)
	})
	return ss
}
