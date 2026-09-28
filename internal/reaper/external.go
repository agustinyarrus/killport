package reaper

import (
	"slices"
	"sort"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Dueño enmascarado → objetivo real.
//
// La tabla de sockets solo conoce procesos de Windows. Cuando el dueño que
// informa es un INTERMEDIARIO (Relay: el motor de contenedores, el reenvío de
// WSL, http.sys, el IP Helper), el objetivo verdadero es otro: un contenedor
// que se detiene por la API de su motor, un proceso Linux al que se le manda
// una señal dentro de su distro, una regla de portproxy que se borra… o un
// proceso de Windows común y corriente, el que registró la URL en http.sys,
// que entra al plan por la misma puerta que cualquier otro dueño.
//
// El núcleo no habla con esos dominios —sigue siendo puro—; lo hace la
// herramienta. El protocolo tiene tres pasos:
//
//  1. PlanKill niega a todo intermediario (Refusal.Relay != RelayNone). Esto
//     vale aunque no se haga nada más: es la garantía de que un relay nunca
//     se mata por error.
//  2. La herramienta pide Plan.Masked() —los pares (intermediario, puerto,
//     protocolo) que habría que mirar—, consulta cada dominio y contesta un
//     Unmasking por par: se vio el objetivo real (Found), se miró y no hay
//     nadie (Empty), o no se pudo mirar (Unavailable).
//  3. Unmask(plan, respuestas) reescribe el plan, puro: lo encontrado pasa a
//     ser un Target (StopExternal si vive en otro dominio; el de siempre si es
//     un proceso de Windows) con el intermediario como Via, que sigue
//     intocable; lo que no, queda como negativa con el motivo que se vio. Un
//     intermediario solo pasa a ser víctima si el dominio lo declaró colgado
//     y seguro de cerrar (KillVia: el relay zombi de WSL).
//
// El motor detiene los externos con OS.StopExternal, todos a la vez y con un
// solo plazo (peldaño 1b), y la verificación sabe que el intermediario tarda
// un instante en soltar el puerto después (lo espera como a un muerto reciente).

// ExtKind es el dominio de un objetivo externo.
type ExtKind uint8

const (
	ExtNone      ExtKind = iota
	ExtContainer         // un contenedor de Docker/Podman: se detiene con la API de su motor
	ExtLinux             // un proceso Linux dentro de una distro de WSL: se le manda una señal adentro
	ExtRule              // una regla de reenvío de puertos (netsh interface portproxy): se borra
)

func (k ExtKind) String() string {
	switch k {
	case ExtContainer:
		return "contenedor"
	case ExtLinux:
		return "proceso Linux"
	case ExtRule:
		return "regla de reenvío"
	}
	return ""
}

// home es la clase de intermediario PROPIA del dominio: la del motor para un
// contenedor, la del reenvío de WSL para un proceso Linux. A un mismo externo
// lo pueden servir varios intermediarios (Docker Desktop publica cada puerto
// con com.docker.backend y, además, con wslrelay en [::1]); el que se nombra
// como Via es el de su dominio.
func (k ExtKind) home() Relay {
	switch k {
	case ExtContainer:
		return RelayContainer
	case ExtLinux:
		return RelayWSL
	case ExtRule:
		return RelayPortProxy
	}
	return RelayNone
}

// External es un objetivo que NO es un proceso de Windows: no se abre con
// OpenProcess ni se espera con WaitForMultipleObjects. Se detiene con la
// operación de su dominio, que además hace su propia escalera (docker stop
// manda SIGTERM, espera y recién después SIGKILL; en Linux se manda SIGINT,
// SIGTERM y SIGKILL en ese orden).
//
// Key es su identidad dentro del dominio y cumple el mismo papel que ID para
// un proceso: no puede apuntar a otra cosa por reciclado. El id de un
// contenedor no se reusa; un proceso Linux se identifica por PID y hora de
// arranque, igual que en Windows.
type External struct {
	Kind  ExtKind
	Where string    // dónde vive, para mostrar: "Docker Desktop", "Podman", "Ubuntu"
	Key   string    // identidad estable en su dominio (id del contenedor; "distro/PID@arranque")
	Name  string    // nombre para mostrar: "db-1", "node"
	Ports ports.Set // TODOS los puertos del host que libera detenerlo, pedidos o no
	// Caution es lo que el usuario tiene que saber antes de confirmar ("tiene
	// restart=always: vuelve cuando reinicie Docker"); "" si nada.
	Caution string
	// Ref es lo que el adaptador necesita para actuar (el contenedor con su
	// motor, el proceso Linux con su distro). El núcleo no lo interpreta: lo
	// devuelve tal cual en OS.StopExternal.
	Ref any
}

// Label nombra al externo dentro de una oración: "el contenedor db-1", "node
// en Ubuntu". Es la única forma de nombrarlo, para los motivos que escribe el
// núcleo y para todo lo que muestra la herramienta.
func (x External) Label() string {
	switch x.Kind {
	case ExtContainer:
		return "el contenedor " + x.Name
	case ExtLinux:
		if x.Where != "" {
			return x.Name + " en " + x.Where
		}
	case ExtRule:
		return "la regla de portproxy " + x.Name
	}
	return x.Name
}

// UnmaskVerdict es qué se averiguó al mirar detrás de un intermediario.
type UnmaskVerdict uint8

const (
	UnmaskUnknown     UnmaskVerdict = iota // nadie miró (queda la negativa genérica)
	UnmaskFound                            // se vio el objetivo real
	UnmaskEmpty                            // se miró y no hay nadie detrás (puerto interno, relay zombi)
	UnmaskUnavailable                      // no se pudo mirar (motor apagado, sin permiso, WSL no responde)
)

// Unmasking es la respuesta de la herramienta sobre UN puerto enmascarado.
type Unmasking struct {
	Via     ID // el intermediario que figura como dueño en la tabla
	Port    uint16
	Proto   ports.Proto
	Verdict UnmaskVerdict
	Behind  []External // Found: el objetivo real (varios si comparten el puerto con SO_REUSEPORT)
	// Procs (Found): los PROCESOS DE WINDOWS que están detrás —los que
	// registraron una URL de ese puerto en http.sys—. No se vuelven objetivo
	// sin más: Unmask los pasa por la clasificación de PlanKill, con todas sus
	// negativas (un svchost, la terminal de killport) y sus acciones (detener
	// el servicio de terceros, --tree). Identidades de la instantánea con la
	// que se planificó; las que no estén en ella se ignoran.
	Procs []ID
	// KillVia (con Empty): el intermediario quedó colgado sin nadie detrás y
	// cerrarlo no corta nada vivo, así que él mismo pasa a ser el objetivo.
	KillVia bool
	Reason  string // lo que se vio, en español ("ningún contenedor publica el 8888")
	Advice  string // qué hacer
	// Sockets (opcional): la respuesta vale solo para ESTOS sockets del par;
	// nil = para todos. Un mismo intermediario puede tener en el mismo puerto
	// sockets de dueños distintos: dos contenedores que publican el 8080 en
	// 127.0.0.1 y en 127.0.0.2 (el motor abre uno por dirección), o wslrelay
	// con el [::1] de una publicación de Docker y el 127.0.0.1 de un proceso
	// de Ubuntu. Entonces se contesta una vez por cada parte, y cada externo
	// se lleva solo los sockets que atiende (así Focus puede separarlos). Un
	// socket del par que ninguna respuesta nombra toma la respuesta sin
	// Sockets, si la hay; si no, queda negado como estaba.
	Sockets []ports.Socket
}

// MaskedPort es un par (intermediario, puerto, protocolo) que la herramienta
// tiene que averiguar: lo que devuelve Plan.Masked.
type MaskedPort struct {
	Via     ID
	Name    string // imagen del intermediario
	Relay   Relay
	Port    uint16
	Proto   ports.Proto
	Sockets []ports.Socket
}

// Masked lista, en orden de puerto, lo que las negativas de intermediarios
// tienen enmascarado. Solo las clases que se pueden desenmascarar
// (Relay.Unmaskable: contenedores y WSL; VS Code no expone a quién reenvía).
func (p Plan) Masked() []MaskedPort {
	var out []MaskedPort
	for _, r := range p.Refusals {
		if !r.Relay.Unmaskable() {
			continue
		}
		for _, g := range groupByPortProto(r.Sockets) {
			out = append(out, MaskedPort{Via: r.Owner, Name: r.Name, Relay: r.Relay, Port: g.port, Proto: g.proto, Sockets: g.socks})
		}
	}
	return out
}

// Unmask reescribe el plan con lo que la herramienta vio detrás de los
// intermediarios. Es pura y total: un par sin respuesta queda como estaba.
//
//   - Found con Behind: cada External pasa a ser (o se suma a) un Target
//     StopExternal, identificado por su Key —un contenedor que publica el
//     5432 y el 5433 es UN objetivo con los dos puertos—, con el intermediario
//     como Via. Si lo sirven varios intermediarios, sus sockets se juntan en
//     el mismo objetivo (la verificación espera a que TODOS suelten el puerto)
//     y el Via es el del dominio del externo.
//   - Found con Procs: cada proceso de Windows pasa por la clasificación de
//     PlanKill, con el mundo que el plan recuerda. Sale de ahí negado (con el
//     intermediario anotado en Refusal.Via) o como objetivo de siempre
//     —proceso común, servicio de terceros— con el intermediario como Via.
//     Con --tree se le suma el subárbol y se rehace el antichain.
//   - Empty con KillVia: el intermediario mismo es el objetivo (KillProcess),
//     con el motivo y la advertencia que dio el dominio.
//   - Empty sin KillVia, Unavailable o sin respuesta: el par queda negado, con
//     el motivo y el consejo que se vieron (o el genérico si nadie miró).
//
// Las negativas de un mismo intermediario se parten según lo que se vio de
// cada puerto: "el 5432 lo publica db-1" y "el 8888 es un puerto interno de
// Docker Desktop" son dos historias distintas. O(pares + objetivos).
func Unmask(plan Plan, found []Unmasking) Plan {
	type key struct {
		via   ID
		port  uint16
		proto ports.Proto
	}
	answers := make(map[key][]Unmasking, len(found))
	for _, u := range found {
		k := key{u.Via.Canon(), u.Port, u.Proto}
		answers[k] = append(answers[k], u)
	}

	// El plan que se devuelve no comparte la lista de objetivos con el que
	// entró: quien llama puede seguir usando el suyo.
	plan.Targets = append([]Target(nil), plan.Targets...)
	byKey := map[string]int{} // Key del externo → índice en plan.Targets
	for i, t := range plan.Targets {
		if t.Kind == StopExternal {
			byKey[t.External.Key] = i
		}
	}
	byRelay := map[ID]int{} // relay colgado → índice de su Target KillProcess
	procs := false          // entraron procesos de Windows: falta darles sus víctimas

	var refusals []Refusal
	for _, r := range plan.Refusals {
		if !r.Relay.Unmaskable() {
			// Una negativa que no es de un intermediario desenmascarable (el
			// kernel, tu terminal, VS Code…) no se reescribe aunque llegue una
			// respuesta con su identidad: Unmask no es una puerta para volver
			// objetivo a lo que la planificación prometió no tocar.
			refusals = append(refusals, r)
			continue
		}
		// Lo que sigue negado, agrupado por el texto que lo explica.
		type rest struct {
			reason, advice string
			socks          []ports.Socket
		}
		var kept []*rest
		keepFor := func(reason, advice string) *rest {
			for _, k := range kept {
				if k.reason == reason && k.advice == advice {
					return k
				}
			}
			k := &rest{reason: reason, advice: advice}
			kept = append(kept, k)
			return k
		}
		handle := func(u Unmasking, ok bool, g portGroup) {
			switch {
			case ok && u.Verdict == UnmaskFound && (len(u.Behind) > 0 || len(u.Procs) > 0):
				for _, x := range u.Behind {
					if i, dup := byKey[x.Key]; dup {
						t := &plan.Targets[i]
						t.Ports = t.Ports.Union(ports.Of(g.port))
						t.Sockets = append(t.Sockets, g.socks...)
						if home := t.External.Kind.home(); t.ViaClass != home && r.Relay == home {
							t.Via, t.ViaName, t.ViaClass = r.Owner, r.Name, r.Relay
						}
						continue
					}
					plan.Targets = append(plan.Targets, Target{
						Name: x.Name, Kind: StopExternal, External: x,
						Ports: ports.Of(g.port), Sockets: g.socks,
						Via: r.Owner, ViaName: r.Name, ViaClass: r.Relay,
						Caution: x.Caution,
					})
					byKey[x.Key] = len(plan.Targets) - 1
				}
				if len(u.Procs) == 0 {
					return
				}
				denied, entered := plan.behind(r, g, u.Procs)
				for _, n := range denied {
					refusals = mergeRefusal(refusals, n)
				}
				procs = procs || entered
				if len(denied) == 0 && !entered && len(u.Behind) == 0 {
					// Ninguno de los procesos que se vieron detrás está en la
					// instantánea (salieron entre una lectura y la otra), o el
					// plan no recuerda su mundo: queda negado como estaba.
					k := keepFor(r.Reason, r.Advice)
					k.socks = append(k.socks, g.socks...)
				}
			case ok && u.Verdict == UnmaskEmpty && u.KillVia:
				if i, dup := byRelay[r.Owner]; dup {
					t := &plan.Targets[i]
					t.Ports = t.Ports.Union(ports.Of(g.port))
					t.Sockets = append(t.Sockets, g.socks...)
					return
				}
				plan.Targets = append(plan.Targets, Target{
					Root: r.Owner, Name: r.Name, Kind: KillProcess,
					Ports: ports.Of(g.port), Sockets: g.socks, Victims: []ID{r.Owner},
					Reason: u.Reason, Caution: u.Advice, ViaClass: r.Relay,
				})
				byRelay[r.Owner] = len(plan.Targets) - 1
			case ok && (u.Verdict == UnmaskEmpty || u.Verdict == UnmaskUnavailable) && u.Reason != "":
				k := keepFor(u.Reason, u.Advice)
				k.socks = append(k.socks, g.socks...)
			default:
				k := keepFor(r.Reason, r.Advice)
				k.socks = append(k.socks, g.socks...)
			}
		}
		for _, g := range groupByPortProto(r.Sockets) {
			for _, part := range splitAnswers(g, answers[key{r.Owner.Canon(), g.port, g.proto}]) {
				handle(part.u, part.ok, part.g)
			}
		}
		for _, k := range kept {
			nr := r
			nr.Sockets = k.socks
			nr.Ports = socketPorts(k.socks)
			nr.Reason, nr.Advice = k.reason, k.advice
			refusals = append(refusals, nr)
		}
	}
	plan.Refusals = refusals
	if procs {
		plan.assignVictims()
	}
	sortPlan(&plan)
	return plan
}

// answerPart es una parte de los sockets de un par (puerto, protocolo) con la
// respuesta que le toca; ok=false si ninguna respuesta la nombra.
type answerPart struct {
	u  Unmasking
	ok bool
	g  portGroup
}

// splitAnswers reparte los sockets de un par entre sus respuestas (ver
// Unmasking.Sockets): cada socket va a la primera respuesta que lo nombra,
// o si ninguna, a la primera sin Sockets; si tampoco hay, a ninguna. Las
// partes salen en el orden de las respuestas, y al final lo que no tomó
// nadie. O(s · r).
func splitAnswers(g portGroup, us []Unmasking) []answerPart {
	if len(us) == 0 {
		return []answerPart{{g: g}}
	}
	if len(us) == 1 && us[0].Sockets == nil {
		return []answerPart{{u: us[0], ok: true, g: g}}
	}
	same := func(a, b ports.Socket) bool { return a.PID == b.PID && a.Proto == b.Proto && a.Local == b.Local }
	byAnswer := make([][]ports.Socket, len(us))
	var none []ports.Socket
	for _, s := range g.socks {
		i := slices.IndexFunc(us, func(u Unmasking) bool {
			return slices.ContainsFunc(u.Sockets, func(x ports.Socket) bool { return same(x, s) })
		})
		if i < 0 {
			i = slices.IndexFunc(us, func(u Unmasking) bool { return u.Sockets == nil })
		}
		if i < 0 {
			none = append(none, s)
			continue
		}
		byAnswer[i] = append(byAnswer[i], s)
	}
	var out []answerPart
	for i, socks := range byAnswer {
		if len(socks) > 0 {
			out = append(out, answerPart{u: us[i], ok: true, g: portGroup{port: g.port, proto: g.proto, socks: socks}})
		}
	}
	if len(none) > 0 {
		out = append(out, answerPart{g: portGroup{port: g.port, proto: g.proto, socks: none}})
	}
	return out
}

// behind pasa por la clasificación de siempre a los procesos de Windows que
// se vieron detrás de un intermediario (r) en un puerto (g). Devuelve las
// negativas que salieron y si alguno entró al plan como objetivo. Los sockets
// son los del intermediario: él es quien figura en la tabla, y de él espera la
// verificación que suelte el puerto.
//
// Sin el mundo de la planificación (un plan armado a mano) no clasifica: no
// hay con qué saber si ese proceso es la terminal de killport o un svchost, y
// ante la duda no se toca.
func (p *Plan) behind(r Refusal, g portGroup, ids []ID) (denied []Refusal, entered bool) {
	w := p.w
	if w == nil {
		return nil, false
	}
	seen := map[ID]bool{}
	for _, id := range ids {
		owner, ok := w.f.Lookup(id)
		if !ok || seen[owner.ID] || owner.PID <= kernelPID {
			continue // ya no está, repetido, o el kernel detrás de sí mismo
		}
		seen[owner.ID] = true
		refs, tg := w.classify(owner, ports.Of(g.port), g.socks)
		for _, n := range refs {
			// Una negativa firme: no se vuelve a desenmascarar aunque el
			// proceso de atrás sea, a su vez, un intermediario.
			n.Relay, n.Via, n.ViaName = RelayNone, r.Owner, r.Name
			denied = append(denied, n)
		}
		if tg != nil {
			tg.Via, tg.ViaName, tg.ViaClass = r.Owner, r.Name, r.Relay
			p.addTarget(*tg)
			entered = true
		}
	}
	return denied, entered
}

// mergeRefusal suma una negativa a la lista, o la funde con la que ya cuenta
// la misma historia del mismo dueño (el svchost que está detrás del 80 y del
// 443 es una negativa con los dos puertos).
func mergeRefusal(rs []Refusal, n Refusal) []Refusal {
	for i := range rs {
		if r := &rs[i]; r.Owner == n.Owner && r.PID == n.PID && r.Via == n.Via && r.Reason == n.Reason && r.Advice == n.Advice {
			r.Sockets = append(r.Sockets, n.Sockets...)
			r.Ports = r.Ports.Union(n.Ports)
			return rs
		}
	}
	return append(rs, n)
}

// ExternalStop es cómo terminó el intento de detener un externo.
type ExternalStop struct {
	Outcome Outcome // Closed, Terminated, Gone, Denied, Error
	Method  Method  // con qué cedió: MethodSigInt, MethodSigTerm, MethodEngine, MethodTerminate
	Err     error   // el motivo crudo si no se pudo (Denied/Error)
}

// ExternalResult es cómo le fue a un objetivo externo en la escalada.
type ExternalResult struct {
	External External
	Outcome  Outcome
	Method   Method
	Elapsed  time.Duration
	Err      error
}

// portGroup son los sockets de un mismo (puerto, protocolo): IPv4 e IPv6 del
// mismo servidor son el mismo puerto para quien lo publica.
type portGroup struct {
	port  uint16
	proto ports.Proto
	socks []ports.Socket
}

// groupByPortProto agrupa sockets por (puerto, protocolo), en orden de puerto
// y después de protocolo. O(s log s).
func groupByPortProto(socks []ports.Socket) []portGroup {
	var out []portGroup
	idx := map[[2]int]int{}
	for _, s := range socks {
		k := [2]int{int(s.Port()), int(s.Proto)}
		i, ok := idx[k]
		if !ok {
			out = append(out, portGroup{port: s.Port(), proto: s.Proto})
			i = len(out) - 1
			idx[k] = i
		}
		out[i].socks = append(out[i].socks, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].port != out[j].port {
			return out[i].port < out[j].port
		}
		return out[i].proto < out[j].proto
	})
	return out
}
