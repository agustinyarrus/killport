package reaper

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Config son los datos de contexto que la planificación necesita del entorno.
type Config struct {
	Self       ID              // identidad de killport: jamás se toca a sí mismo…
	OwnConsole map[uint32]bool // …ni le manda Ctrl+C a su propia consola
	SystemRoot string          // p. ej. "C:\\Windows": una imagen ahí abajo es "de Windows"
	Tree       bool            // --tree: también el subárbol de cada objetivo
	Force      bool            // --force: sin cierre prolijo, directo a terminar
}

// Kind es qué clase de objetivo es una víctima raíz, lo que decide la acción.
type Kind uint8

const (
	KillProcess  Kind = iota // proceso común: la escalera de cierre
	KillPhantom              // el dueño murió; se cierra al hijo que heredó el socket
	StopThirdSvc             // servicio de terceros: se detiene el servicio, no se mata
	StopExternal             // el objetivo real detrás de un intermediario (contenedor, proceso Linux, regla de reenvío)
)

// Target es un objetivo del plan: a quién cerrar y por qué.
type Target struct {
	Root    ID     // la raíz a atacar (el dueño, o el hijo heredero de un fantasma); cero en StopExternal
	Name    string // nombre de la imagen de la raíz (o del externo)
	Kind    Kind
	Service string    // StopThirdSvc: nombre del servicio a detener
	Ports   ports.Set // qué puertos pedidos ocupa este objetivo
	Sockets []ports.Socket
	Victims []ID   // a quién terminar, en orden: raíz primero, después BFS del subárbol (vacío en StopExternal)
	Reason  string // explicación (fantasma, servicio…) en español; "" si es un proceso común obvio
	// Caution es lo que hay que saber antes de confirmar (en español): "cerrarlo
	// corta todos los reenvíos de esa VM", "vuelve cuando reinicie Docker".
	Caution string
	// Impostor: la raíz lleva el nombre de un proceso de Windows y no lo es
	// (ver StandingOf). Es el motivo, para mostrar tal cual; "" si no aplica.
	Impostor string

	// External (StopExternal): el objetivo real, que no es un proceso de Windows.
	External External
	// Via es el intermediario que sostiene los sockets en nombre del objetivo:
	// sigue intocable, y la verificación espera a que suelte el puerto. Lo
	// tienen los externos y también los procesos de Windows que están detrás
	// de http.sys (el socket es del kernel; el que se cierra es el proceso).
	Via      ID
	ViaName  string
	ViaClass Relay // la clase del intermediario (también en un relay zombi que es él mismo el objetivo)

	// Con --tree, el antichain absorbe un objetivo en su ancestro (ver
	// assignVictims). Se guarda lo absorbido, tal como era, y cómo era este
	// objetivo antes de absorber nada: Focus lo necesita para deshacerlo, porque
	// lo pedido puede ser el descendiente y no el ancestro (localhost:5173 es
	// el hijo en [::1], no el padre en 0.0.0.0).
	solo     *Target  // este objetivo antes de su primera absorción; nil si no absorbió nada
	absorbed []Target // los objetivos que absorbió
}

// units deshace el antichain: el objetivo tal como era antes de absorber y,
// recursivamente, cada uno de los que absorbió. Sin absorciones, él mismo.
func (t Target) units() []Target {
	if t.solo == nil {
		return []Target{t}
	}
	own := *t.solo
	out := []Target{own}
	for _, a := range t.absorbed {
		out = append(out, a.units()...)
	}
	return out
}

// Refusal es un dueño que NO se toca, con el motivo y el consejo (en español,
// para mostrar tal cual).
type Refusal struct {
	PID     uint32
	Owner   ID // Zero si el dueño es un PID sin identidad (kernel, fantasma sin heredero)
	Name    string
	Ports   ports.Set
	Sockets []ports.Socket
	Reason  string
	Advice  string
	// Relay: el dueño es un intermediario (contenedores, WSL, VS Code,
	// http.sys, portproxy). La herramienta puede desenmascararlo (Plan.Masked
	// + Unmask).
	Relay Relay
	// Via: la negativa es de un proceso que está DETRÁS de un intermediario
	// (el svchost que registró una URL en http.sys): los sockets figuran a
	// nombre del intermediario, no del proceso negado.
	Via     ID
	ViaName string
	// Service es el servicio de Windows por el que se niega (dentro de un
	// svchost, o un servicio del sistema con proceso propio); "" si no es eso.
	Service string
	// Impostor: como en Target. Un impostor puede quedar negado igual, por
	// otro motivo (es la terminal de killport, es un intermediario).
	Impostor string
}

// Plan es el resultado de planificar: qué atacar, qué no, y qué puertos
// pedidos no tienen dueño (se diagnostican en la verificación).
type Plan struct {
	Targets   []Target
	Refusals  []Refusal
	Ownerless ports.Set // puertos pedidos que ninguna tabla atribuye a nadie
	// Bystanders ocupan un puerto pedido y el pedido los dejó afuera (una
	// dirección a la que no contestan, un selector que no los incluye): no se
	// tocan, no son negativas, y siguen ahí. Los arma Focus.
	Bystanders []Bystander

	// req es el pedido con el que Focus enfocó el plan: la verificación lo
	// usa para saber a quién preguntarle en un puerto pedido con dirección.
	// El valor cero es cada puerto entero.
	req ports.Request

	// w es el mundo con el que se planificó. Lo recuerda el plan para que
	// Unmask clasifique a los procesos que aparecen detrás de un intermediario
	// con EXACTAMENTE las mismas reglas —y la misma instantánea— que usó
	// PlanKill. nil en un plan armado a mano: ahí Unmask no puede volver
	// objetivo a un proceso, y lo deja negado.
	w *world
}

// Empty dice si no hay nada que hacer (ni objetivos ni negativas): todos los
// puertos pedidos ya estaban libres.
func (p Plan) Empty() bool { return len(p.Targets) == 0 && len(p.Refusals) == 0 }

// Victims aplana todas las víctimas del plan, en orden de objetivo y, dentro de
// cada uno, raíz primero. Es el orden en que el motor abre y termina.
func (p Plan) Victims() []ID {
	var out []ID
	for _, t := range p.Targets {
		out = append(out, t.Victims...)
	}
	return out
}

// world es todo lo que la planificación sabe del sistema: la instantánea, los
// servicios, la configuración y la cadena de ancestros de killport. Es
// inmutable una vez armado.
type world struct {
	f         *Forest
	svc       map[uint32][]Service
	cfg       Config
	selfChain map[ID]bool // killport y sus ancestros: la terminal desde donde corrés
}

func newWorld(f *Forest, svc map[uint32][]Service, cfg Config) *world {
	if f == nil {
		f = NewForest(nil)
	}
	cfg.Self = cfg.Self.Canon() // se compara con == contra identidades del bosque
	w := &world{f: f, svc: svc, cfg: cfg, selfChain: map[ID]bool{cfg.Self: true}}
	for _, a := range f.Ancestors(cfg.Self) {
		w.selfChain[a.ID] = true
	}
	return w
}

// kernelHTTPPorts son los que suele escuchar http.sys (PID 4/System) EN NOMBRE
// de otro proceso: WinRM, WSDAPI, los de un servidor web. Solo afinan el texto
// de la negativa cuando nadie pudo mirar las colas de http.sys.
var kernelHTTPPorts = map[uint16]bool{80: true, 443: true, 5985: true, 5986: true, 5357: true, 5358: true, 47001: true}

// kernelService es un puerto que el kernel escucha por sí mismo: ahí no hay
// http.sys ni proceso detrás que buscar.
type kernelService struct{ reason, advice string }

// kernelServiceOf: lo que System escucha por cuenta propia. El 445 y el 139
// son el uso compartido de archivos (SMB directo, y SMB sobre NetBIOS en cada
// interfaz); 137 y 138 UDP, los nombres y datagramas de NetBIOS.
func kernelServiceOf(proto ports.Proto, port uint16) (kernelService, bool) {
	smb := "se libera apagando el uso compartido (servicio Servidor, LanmanServer) y reiniciando: si no es eso lo que buscás, usá otro puerto"
	netbios := "es de cada adaptador de red (NetBIOS sobre TCP/IP): se apaga en las propiedades de IPv4 del adaptador, o usá otro puerto"
	switch {
	case proto == ports.TCP && port == 445:
		return kernelService{"es compartir archivos e impresoras de Windows (SMB): lo atiende el kernel, no http.sys", smb}, true
	case proto == ports.TCP && port == 139:
		return kernelService{"es compartir archivos e impresoras de Windows (SMB sobre NetBIOS): lo atiende el kernel, no http.sys", netbios}, true
	case proto == ports.UDP && (port == 137 || port == 138):
		return kernelService{"es NetBIOS (nombres y datagramas de la red de Windows): lo atiende el kernel", netbios}, true
	}
	return kernelService{}, false
}

// kernelRelay dice qué clase de intermediario es System (PID 4) en ese puerto:
// http.sys si es un puerto TCP que el kernel no escucha por cuenta propia.
func kernelRelay(proto ports.Proto, port uint16) Relay {
	if _, own := kernelServiceOf(proto, port); own || proto != ports.TCP {
		return RelayNone
	}
	return RelayHTTP
}

// ipHelper es el servicio IP Helper: el que escucha por las reglas de
// `netsh interface portproxy` (y arma los túneles de transición a IPv6).
const ipHelper = "iphlpsvc"

// svchostRelay dice qué clase de intermediario es un host de servicios para
// ese socket: el reenvío de portproxy si el que lo abrió es el IP Helper y es
// TCP. hosted son los servicios que corren en ese host.
func svchostRelay(s ports.Socket, hosted []Service) Relay {
	if s.Proto == ports.TCP && strings.EqualFold(exactService(s, hosted), ipHelper) {
		return RelayPortProxy
	}
	return RelayNone
}

// PlanKill arma el plan a partir de los sockets que OCUPAN los puertos pedidos
// (los que Filter clasificó como Holds), el bosque de la instantánea, el mapa
// de servicios y la configuración. `requested` es todo lo que pidió el usuario,
// para calcular los puertos sin dueño. Complejidad O(s + n) más el orden final.
func PlanKill(requested ports.Set, holding []ports.Socket, f *Forest, svcByPID map[uint32][]Service, cfg Config) Plan {
	w := newWorld(f, svcByPID, cfg)
	plan := Plan{w: w}
	occupied := ports.Of()

	// 1. Agrupar los sockets por PID dueño, conservando el orden de aparición
	//    del primer socket de cada PID (salida determinista).
	type group struct {
		pid     uint32
		sockets []ports.Socket
		since   time.Time // la creación más temprana entre sus sockets
	}
	var order []uint32
	groups := map[uint32]*group{}
	for _, s := range holding {
		occupied = occupied.Union(ports.Of(s.Port()))
		g := groups[s.PID]
		if g == nil {
			g = &group{pid: s.PID}
			groups[s.PID] = g
			order = append(order, s.PID)
		}
		g.sockets = append(g.sockets, s)
		if !s.Since.IsZero() && (g.since.IsZero() || s.Since.Before(g.since)) {
			g.since = s.Since
		}
	}
	plan.Ownerless = requested.Minus(occupied)

	// 2..5. Clasificar cada grupo.
	for _, pid := range order {
		g := groups[pid]
		pset := socketPorts(g.sockets)

		// Dueño según el bosque, validado por la hora del socket: el dueño real
		// no puede haber nacido DESPUÉS de que el socket ya existía.
		hold := w.f.Hold(pid, g.since)
		if !hold.Live {
			// Dueño fantasma: el PID que figura murió (o se recicló en otro
			// proceso más nuevo). El socket lo sostiene un hijo que lo heredó.
			if len(hold.Heirs) == 0 {
				plan.Refusals = append(plan.Refusals, Refusal{
					PID: pid, Ports: pset, Sockets: g.sockets,
					Reason: fmt.Sprintf("el dueño (PID %d) ya no existe y ningún proceso heredó el socket", pid),
					Advice: "debería liberarse solo en unos segundos; si no, volvé a intentar",
				})
				continue
			}
			for _, h := range hold.Heirs {
				plan.phantomHeir(w, pid, h, pset, g.sockets)
			}
			continue
		}

		// A partir de acá el dueño está vivo y es el real.
		refs, tg := w.classify(hold.Owner, pset, g.sockets)
		plan.Refusals = append(plan.Refusals, refs...)
		if tg != nil {
			plan.addTarget(*tg)
		}
	}

	// 6. Víctimas por objetivo, y antichain con --tree.
	plan.assignVictims()

	sortPlan(&plan)
	return plan
}

// PlanProcess planifica cerrar UN proceso por su identidad, sin puertos
// propios: el supervisor que relanza a un servidor (nodemon, pm2, un script),
// que no escucha nada y por eso ningún selector lo alcanza. Pasa por la MISMA
// clasificación que un dueño de socket (killport, su terminal, lo crítico, un
// servicio: negados o detenidos como siempre), y con cfg.Tree sus víctimas son
// él y su subárbol. requested son los puertos que tienen que quedar libres
// después: van como los puertos del objetivo, así la verificación los vigila.
// Vacío si ya no está.
func PlanProcess(id ID, requested ports.Set, f *Forest, svcByPID map[uint32][]Service, cfg Config) Plan {
	w := newWorld(f, svcByPID, cfg)
	plan := Plan{w: w}
	p, ok := f.Lookup(id)
	if !ok {
		return plan
	}
	refs, tg := w.classify(p, requested, nil)
	plan.Refusals = refs
	if tg != nil {
		plan.addTarget(*tg)
	}
	plan.assignVictims()
	sortPlan(&plan)
	return plan
}

// phantomHeir juzga a un heredero de un socket cuyo dueño (dead) murió. Pasa
// por la MISMA clasificación que un dueño vivo: el bosque solo dice que pudo
// heredar el socket (Forest.Heirs), no que se lo pueda cerrar. Si el PID
// muerto lo reusó un proceso que también murió dejando hijos, esos hijos se
// ven igual que un heredero, y entre ellos pueden estar killport mismo, su
// terminal, un intermediario o un csrss. Lo que la clasificación deja pasar
// se cierra como fantasma (la raíz es el heredero, los sockets son del
// muerto); lo que niega queda negado, con la historia del socket al lado.
func (p *Plan) phantomHeir(w *world, dead uint32, h Proc, pset ports.Set, socks []ports.Socket) {
	refs, tg := w.classify(h, pset, socks)
	for _, r := range refs {
		// Negativa firme: el heredero no se desenmascara (el socket no es del
		// intermediario, es de un muerto).
		r.Relay = RelayNone
		r.Reason += fmt.Sprintf(" (tiene el socket que heredó del PID %d, que ya murió)", dead)
		p.Refusals = append(p.Refusals, r)
	}
	if tg == nil {
		return
	}
	phantom := fmt.Sprintf("el dueño original (PID %d) ya murió; %s heredó el socket", dead, procLabel(h))
	if tg.Kind == KillProcess {
		tg.Kind, tg.Reason = KillPhantom, phantom
	} else {
		tg.Reason = phantom + "; " + tg.Reason
	}
	p.addTarget(*tg)
}

// addTarget suma un objetivo, o lo funde con el que ya tiene esa raíz: no hay
// dos objetivos sobre el mismo proceso.
func (p *Plan) addTarget(t Target) {
	if t.Kind != StopExternal {
		for i := range p.Targets {
			if ex := &p.Targets[i]; ex.Kind != StopExternal && ex.Root == t.Root {
				ex.Ports = ex.Ports.Union(t.Ports)
				ex.Sockets = append(ex.Sockets, t.Sockets...)
				if ex.Via.Zero() && ex.ViaClass == RelayNone {
					ex.Via, ex.ViaName, ex.ViaClass = t.Via, t.ViaName, t.ViaClass
				}
				if ex.solo != nil {
					// Es el mismo proceso: lo que se le suma es suyo, también en
					// la foto de antes de absorber (una copia: la foto la
					// comparte con el plan del que salió este).
					s := *ex.solo
					s.Ports = s.Ports.Union(t.Ports)
					s.Sockets = append(append([]ports.Socket(nil), s.Sockets...), t.Sockets...)
					ex.solo = &s
				}
				return
			}
		}
	}
	p.Targets = append(p.Targets, t)
}

// classify decide qué hacer con un dueño VIVO: negarlo (una negativa por cada
// historia distinta que haya que contar) o volverlo objetivo. Es la ÚNICA
// puerta por la que un proceso de Windows entra al plan, venga de la tabla de
// sockets (PlanKill) o de detrás de un intermediario (Unmask).
func (w *world) classify(owner Proc, pset ports.Set, socks []ports.Socket) ([]Refusal, *Target) {
	if refs, refuse := w.refuseOwner(owner, pset, socks); refuse {
		return refs, nil
	}
	tg := Target{Root: owner.ID, Name: owner.Name, Kind: KillProcess, Ports: pset, Sockets: socks, Impostor: w.impostor(owner)}

	// Servicio de terceros: se detiene el servicio (matarlo lo reinicia el
	// administrador de servicios por su política de recuperación).
	if svcs, ok := w.svc[owner.PID]; ok && w.thirdParty(owner) {
		names := serviceNames(svcs)
		name := names[0]
		if len(names) > 1 {
			// Un .exe que aloja varios servicios: se detiene el que tiene el
			// puerto, no el primero por orden alfabético (el proceso seguiría
			// vivo con el otro y el puerto, ocupado). Si no se sabe cuál es,
			// no se detiene ninguno.
			exact, known := socketsService(socks, svcs)
			if !known {
				base := Refusal{PID: owner.PID, Owner: owner.ID, Name: owner.Name, Ports: pset, Sockets: socks, Impostor: w.impostor(owner),
					Reason: "el proceso aloja varios servicios (" + strings.Join(names, ", ") + ") y no sé cuál de ellos tiene el puerto",
					Advice: "detené el que corresponda con `net stop NOMBRE` como administrador; terminar el proceso los bajaría a todos"}
				return []Refusal{base}, nil
			}
			name = exact
		}
		tg.Kind, tg.Service = StopThirdSvc, name
		tg.Reason = fmt.Sprintf("es el servicio %s; si lo mato, el administrador de servicios lo reinicia, así que detengo el servicio (hace falta admin)", name)
		if len(names) > 1 {
			tg.Reason = fmt.Sprintf("es el servicio %s (el proceso aloja también %s); si lo mato, el administrador de servicios lo reinicia, así que detengo ese servicio (hace falta admin)",
				name, strings.Join(slices.DeleteFunc(slices.Clone(names), func(n string) bool { return strings.EqualFold(n, name) }), ", "))
		}
	}
	return nil, &tg
}

// socketsService dice qué servicio de un proceso que aloja varios abrió
// TODOS esos sockets (el dueño que informa la pila por socket, validado
// contra lo que corre en el proceso). known=false si alguno no lo trae o si
// son de servicios distintos.
func socketsService(socks []ports.Socket, hosted []Service) (name string, known bool) {
	for _, s := range socks {
		e := exactService(s, hosted)
		if e == "" || (name != "" && !strings.EqualFold(name, e)) {
			return "", false
		}
		name = e
	}
	return name, name != ""
}

// refuseOwner decide si un dueño VIVO es intocable y arma la negativa. El orden
// importa: primero el kernel, después killport y su terminal, lo crítico, los
// intermediarios, y por último svchost y los servicios del sistema.
func (w *world) refuseOwner(owner Proc, pset ports.Set, socks []ports.Socket) ([]Refusal, bool) {
	base := Refusal{PID: owner.PID, Owner: owner.ID, Name: owner.Name, Ports: pset, Sockets: socks, Impostor: w.impostor(owner)}
	one := func(r Refusal) ([]Refusal, bool) { return []Refusal{r}, true }

	switch {
	case owner.PID <= kernelPID:
		return w.refuseKernel(base), true

	case w.cfg.Self != (ID{}) && owner.ID == w.cfg.Self:
		base.Reason = "es killport mismo"
		return one(base)

	case w.selfChain[owner.ID]:
		base.Reason = "es la terminal desde donde corrés killport"
		base.Advice = "cerrala vos si querés; killport no se lleva puesta su propia shell"
		return one(base)

	case w.critical(owner):
		base.Reason = "es un proceso crítico del sistema (" + displayName(owner) + ")"
		base.Advice = "terminarlo colgaría o reiniciaría Windows"
		return one(base)

	case RelayOf(owner) != RelayNone:
		// Un intermediario (Docker Desktop, Podman, WSL, VS Code) NUNCA se
		// mata: sostiene el puerto en nombre de otro. Va antes que la regla
		// del servicio de terceros: com.docker.backend no es un servicio y,
		// sin esta negativa, caería en "proceso común" y se terminaría.
		return one(relayRefusal(base, RelayOf(owner)))

	case w.svchost(owner):
		return w.refuseSvchost(base), true
	}

	// Servicio del sistema (está en el mapa de servicios y su imagen es de
	// Windows, o no se pudo leer): se detiene, no se mata.
	if svcs, ok := w.svc[owner.PID]; ok && !w.thirdParty(owner) {
		names := serviceNames(svcs)
		base.Service = names[0]
		base.Reason = "es el servicio de Windows " + strings.Join(names, ", ")
		base.Advice = "pará el servicio con `net stop " + names[0] + "` como administrador"
		return one(base)
	}
	return nil, false
}

// refuseKernel arma las negativas del kernel (System, PID 4), separando lo
// que escucha por sí mismo —compartir archivos— de lo que PUEDE ser http.sys
// atendiendo en nombre de otro proceso. Esto último queda marcado RelayHTTP:
// la herramienta mira las colas de http.sys y Unmask pone en su lugar al
// proceso que registró la URL. Nunca se aconseja detener ni deshabilitar el
// controlador HTTP: de él dependen la cola de impresión, WinRM y media
// docena de servicios más.
func (w *world) refuseKernel(base Refusal) []Refusal {
	if base.PID != kernelPID {
		base.Reason = "es el kernel de Windows (PID " + itoa(base.PID) + ")"
		base.Advice = "no hay un proceso de usuario que cerrar"
		return []Refusal{base}
	}
	var out []Refusal
	keep := func(reason, advice string, relay Relay, socks []ports.Socket) {
		for i := range out {
			if out[i].Reason == reason && out[i].Advice == advice {
				out[i].Sockets = append(out[i].Sockets, socks...)
				out[i].Ports = socketPorts(out[i].Sockets)
				return
			}
		}
		r := base
		r.Reason, r.Advice, r.Relay, r.Sockets, r.Ports = reason, advice, relay, socks, socketPorts(socks)
		out = append(out, r)
	}
	for _, g := range groupByPortProto(base.Sockets) {
		switch ks, own := kernelServiceOf(g.proto, g.port); {
		case own:
			keep(ks.reason, ks.advice, RelayNone, g.socks)
		case kernelRelay(g.proto, g.port) == RelayNone:
			keep("es el kernel de Windows (PID 4)", "no hay un proceso de usuario que cerrar", RelayNone, g.socks)
		case kernelHTTPPorts[g.port]:
			keep("lo escucha http.sys (el servidor HTTP de Windows) en nombre de otro proceso",
				"`netsh http show servicestate view=requestq` dice qué proceso registró la URL; no se cierra matando al kernel", RelayHTTP, g.socks)
		default:
			keep("lo escucha el kernel de Windows (System, PID 4): si es http.sys, atiende en nombre de otro proceso",
				"`netsh http show servicestate view=requestq` dice qué proceso registró la URL; no se cierra matando al kernel", RelayHTTP, g.socks)
		}
	}
	return out
}

// refuseSvchost arma las negativas de un host de servicios, una por cada
// servicio que tenga alguno de los puertos: dentro de un mismo svchost, "el
// 135 es del RPC" y "el 3000 es una regla de portproxy" son dos historias.
//
// El IP Helper va aparte. Escucha TCP solo por las reglas de `netsh interface
// portproxy`, así que su negativa queda marcada RelayPortProxy: la
// herramienta busca la regla y Unmask la pone de objetivo. Y jamás se aconseja
// detenerlo (el `net stop iphlpsvc` que recomiendan los foros): de él cuelgan
// el reenvío de localhost de WSL y los túneles de IPv6.
func (w *world) refuseSvchost(base Refusal) []Refusal {
	hosted := w.svc[base.PID]
	type bucket struct {
		service string // el servicio exacto; "" = no se supo cuál del host
		socks   []ports.Socket
	}
	var buckets []*bucket
	for _, s := range base.Sockets {
		name := exactService(s, hosted)
		i := slices.IndexFunc(buckets, func(b *bucket) bool { return strings.EqualFold(b.service, name) })
		if i < 0 {
			buckets = append(buckets, &bucket{service: name})
			i = len(buckets) - 1
		}
		buckets[i].socks = append(buckets[i].socks, s)
	}
	slices.SortStableFunc(buckets, func(a, b *bucket) int { return strings.Compare(strings.ToLower(a.service), strings.ToLower(b.service)) })

	var out []Refusal
	add := func(r Refusal, socks []ports.Socket) {
		r.Sockets, r.Ports = socks, socketPorts(socks)
		out = append(out, r)
	}
	for _, b := range buckets {
		r := base
		switch {
		case strings.EqualFold(b.service, ipHelper):
			r.Service = b.service
			var tcp, other []ports.Socket
			for _, s := range b.socks {
				if svchostRelay(s, hosted) == RelayPortProxy {
					tcp = append(tcp, s)
				} else {
					other = append(other, s)
				}
			}
			if len(tcp) > 0 {
				p := r
				p.Relay = RelayPortProxy
				p.Reason = "lo escucha el servicio IP Helper (" + b.service + ", dentro de svchost): es una regla de reenvío de puertos (portproxy)"
				p.Advice = "`netsh interface portproxy show all` muestra la regla y `netsh interface portproxy delete` la borra (como administrador); no detengas " + b.service + ": de él dependen WSL e IPv6"
				add(p, tcp)
			}
			if len(other) > 0 {
				r.Reason = "lo usa el servicio IP Helper (" + b.service + ", dentro de svchost) para los túneles de IPv6"
				r.Advice = "no se mata el svchost ni conviene detener " + b.service + ": de él dependen WSL e IPv6"
				add(r, other)
			}
		case b.service != "":
			r.Service = b.service
			r.Reason = "lo escucha el servicio de Windows " + b.service + " (dentro de svchost)"
			r.Advice = "pará el servicio con `net stop " + b.service + "` como administrador; no se mata el svchost"
			add(r, b.socks)
		default:
			r.Reason = "es un host de servicios de Windows (svchost)"
			if all := serviceNames(hosted); len(all) > 0 {
				r.Service = all[0]
				r.Reason = "lo escucha el servicio de Windows " + strings.Join(all, ", ") + " (dentro de svchost)"
				r.Advice = "pará el servicio con `net stop " + all[0] + "` como administrador; no se mata el svchost"
			} else {
				r.Advice = "no se mata el svchost; identificá el servicio con `tasklist /svc /fi \"PID eq " + itoa(base.PID) + "\"`"
			}
			add(r, b.socks)
		}
	}
	return out
}

// exactService nombra el servicio EXACTO que abrió ese socket dentro de un
// svchost. El mapa PID → servicios dice todos los que corren en ese host (en
// los Windows con poca memoria, un svchost aloja decenas); el dueño que la
// pila TCP/IP informa por socket (Socket.Service, lo que muestra netstat -b)
// dice cuál de ellos abrió ESTE socket. Se lo valida contra el mapa cuando se
// puede; "" si la tabla no lo trajo, trajo el nombre del ejecutable, o nombra
// a un servicio que no corre en ese host (dato viejo o ajeno).
func exactService(s ports.Socket, hosted []Service) string {
	name := strings.TrimSpace(s.Service)
	if name == "" || strings.HasSuffix(strings.ToLower(name), ".exe") {
		return ""
	}
	if len(hosted) == 0 {
		return name
	}
	for _, h := range hosted {
		if strings.EqualFold(h.Name, name) {
			return h.Name
		}
	}
	return ""
}

// assignVictims completa Target.Victims de los objetivos que todavía no las
// tienen. Sin --tree, la víctima es la sola raíz. Con --tree, la raíz y su
// subárbol (raíz primero, BFS), y se aplica el ANTICHAIN: si una raíz es
// descendiente de otra, se absorbe en el ancestro (no se ataca dos veces al
// mismo árbol). Del subárbol se PODAN los intocables (killport y su terminal,
// críticos, svchost, intermediarios) con todo lo que cuelga de ellos: lo que
// la planificación negó como dueño no puede volver a entrar arrastrado por
// --tree.
//
// Se llama al planificar y otra vez al desenmascarar (Unmask suma los
// procesos que encontró detrás de http.sys): los objetivos que ya traen sus
// víctimas —un relay colgado, cuya víctima es él solo— no se tocan ni
// participan del antichain.
func (p *Plan) assignVictims() {
	w := p.w
	process := Target.process
	if w == nil || !w.cfg.Tree {
		for i := range p.Targets {
			if t := &p.Targets[i]; process(*t) && len(t.Victims) == 0 {
				t.Victims = []ID{t.Root.Canon()}
			}
		}
		return
	}

	// Antichain: descartar toda raíz que sea descendiente de otra raíz, y
	// pasarle sus puertos al ancestro que la absorbe. O(t²) sobre la cantidad
	// de OBJETIVOS (un puñado), no de procesos.
	absorbed := make([]bool, len(p.Targets))
	for i := range p.Targets {
		for j := range p.Targets {
			if i == j || absorbed[i] || absorbed[j] || !process(p.Targets[i]) || !process(p.Targets[j]) {
				continue
			}
			if w.f.IsAncestor(p.Targets[j].Root, p.Targets[i].Root) {
				// j es ancestro de i: i se absorbe en j, y queda anotado cómo
				// eran los dos (ver Target.units).
				if tj := &p.Targets[j]; tj.solo == nil {
					s := *tj
					tj.solo = &s
				}
				p.Targets[j].absorbed = append(p.Targets[j].absorbed, p.Targets[i])
				p.Targets[j].Ports = p.Targets[j].Ports.Union(p.Targets[i].Ports)
				p.Targets[j].Sockets = append(p.Targets[j].Sockets, p.Targets[i].Sockets...)
				if p.Targets[j].Via.Zero() && p.Targets[j].ViaClass == RelayNone {
					p.Targets[j].Via, p.Targets[j].ViaName, p.Targets[j].ViaClass = p.Targets[i].Via, p.Targets[i].ViaName, p.Targets[i].ViaClass
				}
				absorbed[i] = true
			}
		}
	}
	kept := p.Targets[:0]
	for i, t := range p.Targets {
		if absorbed[i] {
			continue
		}
		if process(t) {
			t.Victims = w.prunedSubtree(t.Root)
		}
		kept = append(kept, t)
	}
	p.Targets = kept
}

// process: el objetivo es un proceso de Windows con víctimas propias (no un
// externo, ni un relay colgado, que trae la suya de fábrica).
func (t Target) process() bool { return t.Kind != StopExternal && !colgado(t) }

// colgado: el objetivo es un intermediario que su dominio declaró colgado y
// seguro de cerrar (Unmask con KillVia). Trae su víctima —él solo— de fábrica.
func colgado(t Target) bool {
	return t.Kind == KillProcess && t.ViaClass != RelayNone && t.Via.Zero()
}

// prunedSubtree es el subárbol de root en orden BFS (raíz primero) sin los
// intocables ni sus descendientes. La raíz se conserva siempre: ya la eligió
// la planificación. O(nodos del subárbol).
func (w *world) prunedSubtree(root ID) []ID {
	out := []ID{root.Canon()}
	queue := w.f.Children(root)
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if w.untouchable(p) || consoleHost(p, w.cfg.SystemRoot) {
			continue // y sus hijos tampoco: son parte de la maquinaria intocable
		}
		out = append(out, p.ID)
		queue = append(queue, w.f.Children(p.ID)...)
	}
	return out
}

// consoleHost: el anfitrión de una consola (conhost.exe de System32, o el
// OpenConsole.exe de Windows Terminal). Figura como hijo del primer proceso de
// esa consola, pero no es un hijo del programa: es la ventana. Se va solo
// cuando se va el último proceso de la consola; terminarlo en seco no agrega
// nada (y cortaría la consola a los que todavía la usan). Con --tree no es
// víctima, y no se lo cuenta ni se lo narra.
func consoleHost(p Proc, systemRoot string) bool {
	switch strings.ToLower(p.Name) {
	case "openconsole.exe":
		return true
	case "conhost.exe":
		return atSystem32(p.Path, systemRoot, "conhost.exe", false)
	}
	return false
}

// sortPlan ordena objetivos, negativas y sus sockets por el puerto más bajo,
// para que la salida sea estable y lea de menor a mayor.
func sortPlan(plan *Plan) {
	sort.SliceStable(plan.Targets, func(i, j int) bool { return minPort(plan.Targets[i].Ports) < minPort(plan.Targets[j].Ports) })
	sort.SliceStable(plan.Refusals, func(i, j int) bool { return minPort(plan.Refusals[i].Ports) < minPort(plan.Refusals[j].Ports) })
	for i := range plan.Targets {
		sortSockets(plan.Targets[i].Sockets)
	}
	for i := range plan.Refusals {
		sortSockets(plan.Refusals[i].Sockets)
	}
}

// --- Clasificación auxiliar ------------------------------------------------

// standing juzga si p es el proceso de Windows cuyo nombre lleva.
func (w *world) standing(p Proc) (Standing, string) {
	return StandingOf(p, w.f, w.cfg.SystemRoot)
}

// impostor es el motivo por el que p es un posible impostor, o "".
func (w *world) impostor(p Proc) string {
	if s, why := w.standing(p); s == StandingImpostor {
		return why
	}
	return ""
}

// critical: terminarlo cuelga o reinicia Windows. O está marcado así
// (ProcessBreakOnTermination, que no depende del nombre), o es de verdad uno
// de los procesos críticos de Windows. Un impostor que se llama lsass.exe no
// es crítico: es un programa cualquiera.
func (w *world) critical(p Proc) bool {
	if p.Critical {
		return true
	}
	si, ok := systemImageOf(p)
	if !ok || !si.critical {
		return false
	}
	s, _ := w.standing(p)
	return s.Trusted()
}

// svchost: es de verdad un host de servicios de Windows.
func (w *world) svchost(p Proc) bool {
	if !namedSvchost(p) {
		return false
	}
	s, _ := w.standing(p)
	return s.Trusted()
}

func namedSvchost(p Proc) bool {
	return strings.EqualFold(p.Name, "svchost.exe") || strings.EqualFold(p.Name, "svchost")
}

// thirdParty: es un servicio cuya imagen NO está bajo %SystemRoot% (y no es
// un svchost de verdad). Sin ruta o bajo SystemRoot, se toma como servicio de
// Windows (más conservador: no lo matamos, sugerimos net stop).
func (w *world) thirdParty(p Proc) bool {
	if w.svchost(p) || p.Path == "" || w.cfg.SystemRoot == "" {
		return false
	}
	return !UnderDir(p.Path, w.cfg.SystemRoot)
}

// UnderDir dice si path está dentro de la carpeta dir. Compara sin distinguir
// mayúsculas (NTFS es insensible), con las barras normalizadas y sin el
// prefijo de ruta larga (\\?\C:\… es C:\…, \\?\UNC\srv\… es \\srv\…, como
// win.TrimLongPrefix). Es la única regla de "está en esa carpeta" de killport:
// la usan la protección de lo de Windows y lo que se cuenta de un servicio.
func UnderDir(path, dir string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "/", `\`))
		if rest, ok := strings.CutPrefix(s, `\\?\unc\`); ok {
			return `\\` + rest
		}
		return strings.TrimPrefix(s, `\\?\`)
	}
	d := strings.TrimRight(norm(dir), `\`) + `\`
	return strings.HasPrefix(norm(path), d)
}

// userWritableDirs son las carpetas debajo de %SystemRoot% donde cualquier
// usuario puede escribir y ejecutar (las conocidas por los que se saltean
// AppLocker; comprobado a mano en esta máquina con Temp, Tasks, tracing,
// System32\Tasks y spool\drivers\color). Lo que corre desde ahí vive en la
// carpeta de Windows pero lo puso cualquiera.
var userWritableDirs = []string{
	`Temp`, `Tasks`, `tracing`, `debug`, `PLA`, `Registration\CRMLog`,
	`System32\Tasks`, `System32\Tasks_Migrated`, `System32\spool`, `System32\FxsTmp`,
	`System32\com\dmp`, `System32\Microsoft\Crypto\RSA\MachineKeys`,
	`SysWOW64\Tasks`, `SysWOW64\FxsTmp`, `SysWOW64\com\dmp`,
}

// scriptHosts son programas de Windows que corren código de otro: un
// servidor escrito en PowerShell, un .hta, una DLL por rundll32. La imagen es
// de Windows; lo que escucha es del usuario.
var scriptHosts = map[string]bool{
	"powershell.exe": true, "powershell_ise.exe": true, "cmd.exe": true,
	"cscript.exe": true, "wscript.exe": true, "mshta.exe": true,
	"rundll32.exe": true, "regsvr32.exe": true,
	"msbuild.exe": true, "installutil.exe": true, "regasm.exe": true, "regsvcs.exe": true,
}

// WindowsImage dice si p corre una imagen de Windows: debajo de %SystemRoot%
// (root), fuera de las carpetas donde escribe cualquiera y sin ser un host de
// scripts. Solo mira la ruta y el nombre (sin bosque ni servicios): sirve
// para decidir, antes de tocarlo, que un proceso es cosa del sistema.
func WindowsImage(p Proc, root string) bool {
	return p.Path != "" && root != "" && UnderDir(p.Path, root) && windowsImage(p, root)
}

// windowsImage dice si una imagen que vive debajo de %SystemRoot% es de
// Windows para esconderla: no lo es si está en una carpeta donde escribe
// cualquiera, ni si es un intérprete o un host de scripts.
func windowsImage(p Proc, root string) bool {
	if scriptHosts[strings.ToLower(p.Name)] {
		return false
	}
	nr := strings.TrimRight(strings.ReplaceAll(root, "/", `\`), `\`)
	for _, d := range userWritableDirs {
		if UnderDir(p.Path, nr+`\`+d) {
			return false
		}
	}
	return true
}

// IsWindowsProc clasifica un proceso como "de Windows" para OCULTARLO en el
// listado general (salvo --system). Es la contracara de lo anterior: kernel,
// críticos, svchost, imagen de Windows (debajo de %SystemRoot%, fuera de las
// carpetas donde escribe cualquiera, que no sea un host de scripts) en la
// sesión 0, o sin imagen conocida en sesión 0 (servicios del sistema) que no
// sea un servicio de terceros. f es el bosque de donde salió p.
//
// La ruta sola esconde SOLO en la sesión 0 (la de los servicios). En la
// sesión del usuario, lo que escucha desde la carpeta de Windows y no es un
// proceso de Windows con nombre y padre verificados (StandingOf) es casi
// siempre del usuario —un script de Windows PowerShell 5.1, un HttpListener—
// o alguien que se esconde ahí: se muestra.
//
// Un proceso que LLEVA el nombre de uno de Windows y no se pudo confirmar que
// lo sea (posible impostor, o ruta ilegible) no se oculta nunca, por ninguna
// de las otras reglas: esconder justo eso sería hacerle el favor.
func IsWindowsProc(p Proc, f *Forest, svcByPID map[uint32][]Service, cfg Config) bool {
	w := &world{f: f, svc: svcByPID, cfg: cfg}
	if w.f == nil {
		w.f = NewForest(nil)
	}
	switch s, _ := w.standing(p); {
	case RelayOf(p) != RelayNone:
		return false // un relay tiene los servidores del usuario (contenedores, WSL): se muestra
	case p.PID <= kernelPID:
		return true
	case s == StandingGenuine:
		return true
	case s != StandingOrdinary:
		return false
	case p.Critical:
		return true
	case p.Path != "" && UnderDir(p.Path, cfg.SystemRoot):
		// La ruta sola no alcanza para esconder: ver windowsImage.
		return p.SessionID == 0 && windowsImage(p, cfg.SystemRoot)
	case w.thirdParty(p):
		return false // un postgres/mysql en sesión 0 NO se oculta
	case p.Path == "" && p.SessionID == 0:
		return true // imagen desconocida en la sesión de servicios: cosa del sistema
	}
	_, isSvc := svcByPID[p.PID]
	return isSvc && !w.thirdParty(p)
}

// --- Helpers de presentación y orden ---------------------------------------

func socketPorts(socks []ports.Socket) ports.Set {
	ps := make([]uint16, len(socks))
	for i, s := range socks {
		ps[i] = s.Port()
	}
	return ports.Of(ps...)
}

func serviceNames(svcs []Service) []string {
	if len(svcs) == 0 {
		return nil
	}
	out := make([]string, len(svcs))
	for i, s := range svcs {
		out[i] = s.Name
	}
	slices.Sort(out)
	return out
}

// NameOrPID es el nombre de un proceso, o "PID N" si no se supo. Con
// NameWithPID, es la única forma de nombrar a un proceso en un texto (el
// núcleo y la herramienta usan estas dos).
func NameOrPID(name string, pid uint32) string {
	if name != "" {
		return name
	}
	return "PID " + itoa(pid)
}

// NameWithPID nombra a un proceso con su PID: "node.exe (PID 812)", o "PID
// 812" si no se supo el nombre (sin repetirlo: nunca "PID 812 (PID 812)").
func NameWithPID(name string, pid uint32) string {
	if name == "" {
		return "PID " + itoa(pid)
	}
	return name + " (PID " + itoa(pid) + ")"
}

func displayName(p Proc) string { return NameOrPID(p.Name, p.PID) }

func procLabel(p Proc) string { return NameWithPID(p.Name, p.PID) }

func minPort(s ports.Set) int {
	if p, ok := s.Min(); ok {
		return int(p)
	}
	return 1 << 20
}

func sortSockets(socks []ports.Socket) {
	sort.SliceStable(socks, func(i, j int) bool {
		if socks[i].Port() != socks[j].Port() {
			return socks[i].Port() < socks[j].Port()
		}
		return socks[i].Proto < socks[j].Proto
	})
}

func itoa(n uint32) string { return fmt.Sprintf("%d", n) }
