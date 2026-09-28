package reaper

import (
	"net/netip"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Lo que no se pidió.
//
// PlanKill planifica sobre TODO lo que ocupa los puertos pedidos. Pero el
// usuario puede haber pedido menos que eso:
//
//   - una dirección: "localhost:5173" es el que contesta localhost, no el
//     python que escucha el mismo puerto en 0.0.0.0 (ver ports/dest.go);
//   - un selector: "-n node" son los node que escuchan, no el chrome que
//     comparte con ellos el 5353; "--orphans", los que quedaron sueltos;
//     "--project", los de una carpeta.
//
// Focus deja en el plan solo eso. Lo demás no es una negativa —nadie dijo que
// no se toca: no se pidió— y tampoco desaparece: pasa a Bystanders, para que
// la herramienta diga que sigue ahí y para que la verificación no lo confunda
// con alguien que retomó el puerto. Se decide socket por socket y se aplica
// por dueño: un objetivo entra si ALGUNO de sus sockets se pidió (un proceso no
// se cierra a medias: si se lo cierra por el 3000, suelta también el 5173).
//
// Va DESPUÉS de desenmascarar (Unmask): Docker Desktop publica cada puerto con
// dos intermediarios, com.docker.backend en 0.0.0.0 y [::] y wslrelay en
// [::1], y "localhost:8080" llega por wslrelay al mismo contenedor que
// "127.0.0.1:8080" por el motor. Mirado antes, el socket de wslrelay quedaría
// solo, sin el motor al lado que lo explica.
//
// Con --tree, el antichain de la planificación ya juntó cada objetivo con los
// que descienden de él. Focus lo DESHACE (Target.units), reparte, y lo rehace
// solo entre lo pedido: si se pidió al hijo (el que contesta localhost) y no al
// padre, el padre no cae por ser su ancestro. Y al revés: lo que quedó al
// margen pero cuelga de un objetivo cae con él (eso pide --tree), así que no
// se lo nombra "al margen".

// Bystander es un dueño que ocupaba un puerto pedido y que el pedido dejó
// afuera. Sus sockets son los que tenía al planificar: la verificación los
// reconoce por su identidad de socket (dueño, dirección, hora de creación),
// así que un socket NUEVO en ese puerto, aunque sea del mismo proceso, cuenta
// como alguien que llegó después.
type Bystander struct {
	PID   uint32 // el que figura en la tabla (un intermediario, si el dueño real está detrás)
	Owner ID     // su identidad; cero si no la tiene (el kernel, un dueño que ya murió)
	Name  string // cómo se lo nombra: la imagen, o el externo ("el contenedor db-1")
	// External: no es un proceso de Windows sino lo que está detrás de un
	// intermediario (un contenedor, un proceso Linux, una regla); PID y
	// Owner son los del intermediario.
	External bool
	Ports    ports.Set
	Sockets  []ports.Socket
}

// Focus reduce el plan a lo pedido. req dice qué direcciones pidió el
// usuario en cada puerto (el valor de Whole: el puerto entero). picked, si no
// es nil, es la otra mitad del pedido, los selectores: solo cuentan los
// sockets que acepta. Un socket entra si cumple las dos. Lo que queda afuera
// pasa a Bystanders. El plan recuerda req: Verify lo necesita para saber
// quién contesta después.
//
// Pura. O(s · d) con s los sockets del plan y d las direcciones pedidas, más
// el orden de Answer por cada (puerto, protocolo, dirección) distinto, que se
// calcula una sola vez.
func Focus(plan Plan, req ports.Request, picked func(ports.Socket) bool) Plan {
	plan.req = req
	if picked == nil && !req.Aimed() {
		return plan
	}
	// Todo lo que escuchaba en los puertos del plan: de acá sale quién
	// contesta cada dirección.
	var all []ports.Socket
	for _, t := range plan.Targets {
		all = append(all, t.Sockets...)
	}
	for _, r := range plan.Refusals {
		all = append(all, r.Sockets...)
	}
	type q struct {
		proto ports.Proto
		port  uint16
		dest  ports.Dest
	}
	answers := map[q]map[sockKey]bool{}
	answered := func(s ports.Socket, d ports.Dest) bool {
		k := q{s.Proto, s.Port(), d}
		m, ok := answers[k]
		if !ok {
			m = map[sockKey]bool{}
			who, _ := ports.AnswerDest(d, s.Proto, s.Port(), all)
			for _, w := range who {
				m[keyOf(w)] = true
			}
			answers[k] = m
		}
		return m[keyOf(s)]
	}
	in := func(s ports.Socket) bool {
		if picked != nil && !picked(s) {
			return false
		}
		ds := req.Dests(s.Port())
		if len(ds) == 0 {
			return true
		}
		for _, d := range ds {
			if answered(s, d) {
				return true
			}
		}
		return false
	}
	anyIn := func(socks []ports.Socket) bool {
		for _, s := range socks {
			if in(s) {
				return true
			}
		}
		return false
	}

	units := plan.Targets
	tree := plan.w != nil && plan.w.cfg.Tree
	if tree {
		units = nil
		for _, t := range plan.Targets {
			units = append(units, t.units()...)
		}
	}
	var targets []Target
	for _, t := range units {
		if anyIn(t.Sockets) {
			if tree && t.process() {
				t.Victims = nil // se vuelven a asignar entre lo pedido
			}
			targets = append(targets, t)
			continue
		}
		b := Bystander{PID: t.Root.PID, Owner: t.Root, Name: t.Name, Ports: socketPorts(t.Sockets), Sockets: t.Sockets}
		if t.Kind == StopExternal {
			b.PID, b.Owner, b.Name, b.External = t.Via.PID, t.Via, t.External.Label(), true
		}
		plan.Bystanders = append(plan.Bystanders, b)
	}
	var refusals []Refusal
	for _, r := range plan.Refusals {
		if anyIn(r.Sockets) {
			refusals = append(refusals, r)
			continue
		}
		plan.Bystanders = append(plan.Bystanders, Bystander{PID: r.PID, Owner: r.Owner, Name: r.Name, Ports: r.Ports, Sockets: r.Sockets})
	}
	plan.Targets, plan.Refusals = targets, refusals
	if tree {
		plan.assignVictims()
	}
	// Lo que cae con un objetivo (su subárbol, con --tree) no está al margen.
	victim := map[ID]bool{}
	for _, t := range plan.Targets {
		for _, v := range t.Victims {
			victim[v.Canon()] = true
		}
	}
	kept := plan.Bystanders[:0]
	for _, b := range plan.Bystanders {
		if b.Owner.Zero() || !victim[b.Owner.Canon()] {
			kept = append(kept, b)
		}
	}
	plan.Bystanders = kept
	sortPlan(&plan)
	return plan
}

// Request es el pedido con el que se enfocó el plan (el valor cero si no se
// enfocó: cada puerto entero).
func (p Plan) Request() ports.Request { return p.req }

// sockKey identifica un socket de la foto: el mismo dueño, protocolo,
// dirección y hora de creación son el mismo objeto del kernel. Un socket
// nuevo en el mismo lugar tiene otra hora.
type sockKey struct {
	pid   uint32
	proto ports.Proto
	local netip.AddrPort
	since int64
}

func keyOf(s ports.Socket) sockKey {
	k := sockKey{pid: s.PID, proto: s.Proto, local: s.Local}
	if !s.Since.IsZero() {
		k.since = s.Since.UnixNano()
	}
	return k
}

// verifyScope es lo que la verificación sabe de lo pedido: en qué puertos se
// pidió una dirección y qué sockets son de alguien que no se pidió. Esos no
// cuentan para decir si un puerto quedó libre —el python que el usuario no
// pidió cerrar no es un respawn ni un "sigue ocupado"—, pero se nombran.
type verifyScope struct {
	req    ports.Request
	filter ports.Filter
	aside  map[sockKey]*Bystander // socket de alguien que no se pidió → quién es
	now    []ports.Socket         // la última lectura de las tablas (para nombrar)
}

func newVerifyScope(plan Plan, filter ports.Filter) *verifyScope {
	sc := &verifyScope{req: plan.req, filter: filter, aside: map[sockKey]*Bystander{}}
	for i := range plan.Bystanders {
		b := &plan.Bystanders[i]
		for _, s := range b.Sockets {
			sc.aside[keyOf(s)] = b
		}
	}
	return sc
}

// holders dice quién ocupa cada puerto pedido en esta lectura: el primer
// socket de la tabla que no es de alguien que no se pidió, o —en un puerto
// pedido con dirección— el que contesta esa dirección, si no es de alguien
// que no se pidió. Un socket que no contesta la dirección pedida no la ocupa
// aunque escuche el puerto: está a la sombra del que sí. O(s) más Answer por
// puerto con dirección.
func (sc *verifyScope) holders(socks []ports.Socket) map[uint16]ports.Socket {
	byPort := map[uint16][]ports.Socket{}
	var order []uint16
	for _, s := range socks {
		if sc.filter.Relate(s) != ports.Holds {
			continue
		}
		p := s.Port()
		if _, ok := byPort[p]; !ok {
			order = append(order, p)
		}
		byPort[p] = append(byPort[p], s)
	}
	out := make(map[uint16]ports.Socket, len(order))
	for _, p := range order {
		cands := byPort[p]
		if ds := sc.req.Dests(p); len(ds) > 0 {
			cands = sc.answering(ds, p, cands)
		}
		for _, s := range cands {
			if sc.aside[keyOf(s)] == nil {
				out[p] = s
				break
			}
		}
	}
	return out
}

// answering son los sockets que contestan alguna de las direcciones pedidas
// en p, por protocolo y dirección en el orden del pedido, sin repetir.
func (sc *verifyScope) answering(ds []ports.Dest, p uint16, cands []ports.Socket) []ports.Socket {
	var out []ports.Socket
	seen := map[sockKey]bool{}
	for _, proto := range wantedProtos(sc.filter) {
		for _, d := range ds {
			who, _ := ports.AnswerDest(d, proto, p, cands)
			for _, w := range who {
				if k := keyOf(w); !seen[k] {
					seen[k] = true
					out = append(out, w)
				}
			}
		}
	}
	return out
}

// stillThere dice si en un puerto que quedó libre PARA LO PEDIDO sigue
// alguien que no se pidió, y lo cuenta: con una dirección pedida, si ahora es
// él quien la contesta ("ahora localhost llega a python.exe…": answers) o si
// solo escucha en otra dirección; sin dirección, que sigue escuchando.
//
// Devuelve quién, la nota, si contesta la dirección pedida, y si hay alguien.
func (sc *verifyScope) stillThere(p uint16) (Bystander, string, bool, bool) {
	if len(sc.aside) == 0 {
		return Bystander{}, "", false, false
	}
	var here []ports.Socket
	for _, s := range sc.now {
		if sc.filter.Relate(s) == ports.Holds && s.Port() == p && sc.aside[keyOf(s)] != nil {
			here = append(here, s)
		}
	}
	if len(here) == 0 {
		return Bystander{}, "", false, false
	}
	ds := sc.req.Dests(p)
	if len(ds) > 0 {
		for _, proto := range wantedProtos(sc.filter) {
			for _, d := range ds {
				if who, at := ports.AnswerDest(d, proto, p, sc.now); len(who) > 0 {
					b := sc.aside[keyOf(who[0])]
					if b == nil {
						continue // (no pasa: holders ya lo habría tomado)
					}
					how := d.String()
					if d.Kind == ports.DestLocalhost {
						how += " (por " + addrText(at) + ")"
					}
					return *b, "ahora " + how + " llega a " + sc.who(*b) + ", que ya escuchaba en " + addrText(who[0].Local.Addr()) + " y no se pidió", true, true
				}
			}
		}
		b := sc.aside[keyOf(here[0])]
		return *b, "nadie contesta " + ds[0].String() + "; " + sc.who(*b) + " escucha el " + itoa(uint32(p)) + " en " + addrText(here[0].Local.Addr()) + " y no se pidió", false, true
	}
	x := sc.aside[keyOf(here[0])]
	return *x, "sigue escuchando " + sc.who(*x) + " en " + addrText(here[0].Local.Addr()) + ", que no se pidió", false, true
}

// who nombra a alguien que no se pidió: "python.exe (PID 990)", o el externo
// ("el contenedor db-1"), que no tiene PID propio.
func (sc *verifyScope) who(b Bystander) string {
	if b.External {
		return b.Name
	}
	return NameWithPID(b.Name, b.PID)
}

// addrText escribe una dirección como la escribe netstat: la IPv6 entre
// corchetes.
func addrText(a netip.Addr) string {
	if a.Is6() && !a.Is4In6() {
		return "[" + a.String() + "]"
	}
	return a.Unmap().String()
}
