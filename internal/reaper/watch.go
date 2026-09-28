package reaper

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Esperar a que un puerto ESCUCHE (el reemplazo nativo de wait-on: `npm run
// dev & killport 5173 --wait 60s && npx playwright test`) o a que se LIBERE
// sin matar nada (--wait-free).
//
// Se mira la TABLA, no se conecta nadie: no hay trampa de localhost (wait-on
// pregunta a 127.0.0.1 y el servidor escucha en ::1, o al revés), no salta el
// aviso del Firewall y no se despierta al servidor con un pedido falso. Un
// puerto pedido con dirección (localhost:5173) espera al que CONTESTA esa
// dirección, con las reglas de ports.Answer.
//
// Dos piezas:
//
//   - Watch, la máquina de estados, PURA: recibe lecturas de la tabla con su
//     instante (Observe) y lleva, por puerto, el TESTIGO (los sockets que
//     cumplen la condición) y desde cuándo se sostiene. No tiene reloj ni
//     sistema: se prueba con horas escritas a mano.
//   - WaitPorts, el bucle: lee la tabla, se la da a Watch y duerme hasta la
//     próxima lectura.
//
// POR QUÉ SE RELEE LA TABLA (una de las dos esperas por sondeo de killport;
// la otra es la de un servicio que se detiene o arranca, en win.waitService). El
// kernel no le avisa a un proceso sin privilegios que apareció o se fue un
// socket: la sesión ETW en tiempo real de Microsoft-Windows-TCPIP y
// FwpmNetEventSubscribe piden administrador, y las notificaciones de IP
// Helper (NotifyIpInterfaceChange y compañía) son de interfaces y rutas, no
// de sockets. Así que se relee, acotado: la tabla liviana (lo que escucha,
// con su hora de creación, sin preguntar el módulo dueño; 0,3 ms medidos en
// esta máquina contra 7 ms de la completa con dueños), con backoff de 25 a
// 200 ms, en primer plano, con un plazo obligatorio y cancelable al instante.
// Termina sola: no queda nada residente. Lo que SÍ avisa el kernel se usa:
// al esperar que se libere, la espera entre lecturas es un Wait sobre los
// procesos dueños, así que una salida despierta al instante (y la lectura
// siguiente llega en 25 ms, lo que tarda el kernel en soltar el socket).

// WatchFor es qué se espera.
type WatchFor uint8

const (
	WatchListen WatchFor = iota + 1 // que alguien escuche (--wait)
	WatchFree                       // que nadie lo ocupe (--wait-free)
)

// WatchSpec describe la espera.
type WatchSpec struct {
	For WatchFor
	// Request son los puertos y, de los que vinieron con dirección, a cuál
	// apuntan: "localhost:5173" espera al que contesta localhost.
	Request ports.Request
	// Filter: los protocolos. En WatchFree, AllStates suma las conexiones
	// (salvo los restos del PID 0, como siempre).
	Filter ports.Filter
	Any    bool // alcanza con un puerto (si no, todos a la vez, en la misma lectura)
	// Stable: el testigo tiene que sostenerse al menos esto. Un servidor bajo
	// nodemon escucha, se reinicia y vuelve: cada reinicio es otro socket (otra
	// hora de creación, casi siempre otro PID), y la cuenta vuelve a cero.
	Stable time.Duration
	// NewSince (solo WatchListen): cuentan únicamente los sockets creados
	// DESPUÉS de este instante (--new: el arranque de killport), para no darse
	// por satisfecho con el servidor viejo que todavía escucha. Un socket sin
	// hora conocida no cuenta como nuevo. Cero = cualquiera.
	NewSince time.Time
	// Blocked (solo WatchListen): los puertos que no van a poder escuchar
	// (Diagnoser.Blocked), con su diagnóstico.
	Blocked map[uint16]PortResult
}

// WatchPortState es cómo está un puerto en la espera.
type WatchPortState uint8

const (
	WatchPending  WatchPortState = iota // no se cumple
	WatchSettling                       // se cumple, y todavía no se sostuvo lo pedido (--stable)
	WatchMet                            // se cumple (y se sostuvo)
	WatchBlocked                        // no se va a cumplir: lo tapa Windows
)

func (s WatchPortState) String() string {
	switch s {
	case WatchPending:
		return "pendiente"
	case WatchSettling:
		return "sosteniéndose"
	case WatchMet:
		return "cumplido"
	case WatchBlocked:
		return "bloqueado"
	}
	return ""
}

// WatchPort es un puerto de la espera.
type WatchPort struct {
	Port  uint16
	State WatchPortState
	// Witness: en WatchListen, los sockets que lo cumplen (los que escuchan o
	// contestan la dirección pedida). En WatchFree, los que lo OCUPAN todavía
	// (vacío cuando se cumple).
	Witness []ports.Socket
	Since   time.Time // desde cuándo se cumple con este testigo; cero si no se cumple
	MetAt   time.Time // desde cuándo está cumplido sin cortes; cero si no lo está
	// Changes cuenta las veces que, cumpliéndose, dejó de cumplirse o cambió
	// de testigo: los reinicios de un supervisor.
	Changes int
	// Already: ya estaba CUMPLIDO en la primera lectura (sostenido lo pedido,
	// si hay --stable) y no se cortó ni cambió de testigo desde entonces: "ya
	// escuchaba", "ya estaba libre". Un testigo que en la primera lectura
	// todavía se estaba sosteniendo no cuenta: se cumplió después.
	Already bool
	Blocked PortResult // WatchBlocked: por qué

	key string // el testigo como texto canónico (para saber si cambió)
}

// WatchEvent es un cambio en un puerto: lo que la interfaz cuenta en vivo.
type WatchEvent struct {
	Time    time.Time
	Port    WatchPort      // cómo quedó
	Prev    WatchPortState // cómo estaba
	Initial bool           // es la primera lectura: "ya escuchaba", "ya estaba libre"
	Restart bool           // se cumplía y cambió de testigo (otro socket: un reinicio)
}

// Watch es la máquina de estados de la espera. Pura: no lee la hora ni el
// sistema; cada Observe trae una lectura y su instante.
type Watch struct {
	spec  WatchSpec
	ports []WatchPort
	first map[sockKey]time.Time // primera vez que se vio cada socket (los que no traen hora)
	seen  bool
}

// NewWatch prepara la espera de los puertos pedidos, en orden.
func NewWatch(spec WatchSpec) *Watch {
	w := &Watch{spec: spec, first: map[sockKey]time.Time{}}
	for p := range spec.Request.Ports.All() {
		wp := WatchPort{Port: p}
		if b, ok := spec.Blocked[p]; ok && spec.For == WatchListen {
			wp.State, wp.Blocked = WatchBlocked, b
		}
		w.ports = append(w.ports, wp)
	}
	return w
}

// Ports es el estado de cada puerto, en orden.
func (w *Watch) Ports() []WatchPort { return slices.Clone(w.ports) }

// Observe incorpora una lectura de la tabla tomada en now y devuelve lo que
// cambió. O(s + p·a) con s los sockets, p los puertos y a lo que cuesta
// Answer en un puerto con dirección.
func (w *Watch) Observe(now time.Time, socks []ports.Socket) []WatchEvent {
	initial := !w.seen
	w.seen = true
	byPort := map[uint16][]ports.Socket{}
	for _, s := range socks {
		if w.spec.Request.Ports.Contains(s.Port()) && w.counts(s) {
			byPort[s.Port()] = append(byPort[s.Port()], s)
			if k := keyOf(s); w.first[k].IsZero() {
				w.first[k] = now
			}
		}
	}
	var evs []WatchEvent
	for i := range w.ports {
		wp := &w.ports[i]
		if wp.State == WatchBlocked {
			if initial {
				evs = append(evs, WatchEvent{Time: now, Port: *wp, Prev: WatchPending, Initial: true})
			}
			continue
		}
		prev, prevKey := wp.State, wp.key
		witness := w.witness(wp.Port, byPort[wp.Port])
		met := len(witness) > 0
		if w.spec.For == WatchFree {
			met = !met
		}
		wp.Witness = witness
		if !met {
			if prev != WatchPending {
				wp.Changes++
			}
			wp.State, wp.Since, wp.MetAt, wp.key, wp.Already = WatchPending, time.Time{}, time.Time{}, "", false
		} else {
			key := witnessKey(witness)
			restart := prev != WatchPending && key != prevKey
			if restart {
				wp.Changes++
			}
			since := w.since(now, witness)
			if w.spec.For == WatchFree {
				since = wp.Since // libre: desde la primera lectura libre
				if prev == WatchPending || since.IsZero() {
					since = now
				}
			}
			wp.Since, wp.key = since, key
			if now.Sub(since) >= w.spec.Stable {
				if wp.State != WatchMet || restart {
					wp.MetAt = now
				}
				wp.State = WatchMet
			} else {
				wp.State, wp.MetAt = WatchSettling, time.Time{}
			}
			wp.Already = (initial && wp.State == WatchMet) || (wp.Already && !restart)
			if restart {
				evs = append(evs, WatchEvent{Time: now, Port: *wp, Prev: prev, Initial: initial, Restart: true})
				continue
			}
		}
		if wp.State != prev || initial {
			evs = append(evs, WatchEvent{Time: now, Port: *wp, Prev: prev, Initial: initial})
		}
	}
	return evs
}

// counts dice si un socket entra en la cuenta de esta espera.
func (w *Watch) counts(s ports.Socket) bool {
	if !w.spec.Filter.Wants(s.Proto) {
		return false
	}
	if w.spec.For == WatchFree {
		f := w.spec.Filter
		f.Ports = ports.Set{}
		return f.Relate(s) == ports.Holds
	}
	// Escuchar: TCP en LISTEN o UDP atado a ESE puerto (un efímero es un
	// cliente al que el sistema le tocó ese número, no un servidor).
	if s.Proto == ports.TCP && s.State != ports.Listen || s.Proto == ports.UDP && s.Ephemeral {
		return false
	}
	return w.spec.NewSince.IsZero() || s.Since.After(w.spec.NewSince)
}

// witness son los sockets de p que deciden: con dirección pedida, los que la
// contestan; si no, todos los que cuentan.
func (w *Watch) witness(p uint16, cands []ports.Socket) []ports.Socket {
	ds := w.spec.Request.Dests(p)
	if len(ds) == 0 {
		return cands
	}
	var out []ports.Socket
	seen := map[sockKey]bool{}
	for _, proto := range wantedProtos(w.spec.Filter) {
		for _, d := range ds {
			who, _ := ports.AnswerDest(d, proto, p, cands)
			for _, s := range who {
				if k := keyOf(s); !seen[k] {
					seen[k] = true
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// since es desde cuándo se sostiene un testigo de sockets: la hora de
// creación del más nuevo. Es evidencia exacta aunque no se lo haya mirado en
// el medio, porque un socket no reaparece: el mismo objeto del kernel estuvo
// ahí sin cortes desde que nació, y otro nacimiento es otra hora. Un socket
// sin hora (o con una hora que el reloj todavía no alcanzó) cuenta desde la
// primera lectura que lo vio.
func (w *Watch) since(now time.Time, witness []ports.Socket) time.Time {
	var out time.Time
	for _, s := range witness {
		t := s.Since
		if t.IsZero() || t.After(now) {
			t = w.first[keyOf(s)]
		}
		if t.After(out) {
			out = t
		}
	}
	return out
}

// witnessKey es el testigo como texto canónico: los mismos sockets, el mismo
// texto, en cualquier orden.
func witnessKey(ss []ports.Socket) string {
	ks := make([]string, len(ss))
	for i, s := range ss {
		k := keyOf(s)
		ks[i] = strconv.FormatUint(uint64(k.pid), 10) + "/" + k.proto.String() + "/" + k.local.String() + "/" + strconv.FormatInt(k.since, 10)
	}
	slices.Sort(ks)
	return strings.Join(ks, " ")
}

// Done dice si la espera se cumplió: todos los puertos a la vez (en esta
// misma lectura), o uno con Any.
func (w *Watch) Done() bool {
	if !w.seen || len(w.ports) == 0 {
		return false
	}
	met, total := 0, 0
	for _, p := range w.ports {
		if p.State == WatchBlocked {
			continue
		}
		total++
		if p.State == WatchMet {
			met++
		}
	}
	if w.spec.Any {
		return met > 0
	}
	return total > 0 && met == total && !w.anyBlocked()
}

// Impossible dice si ya no se puede cumplir: sin Any, con un solo puerto
// bloqueado; con Any, con todos.
func (w *Watch) Impossible() bool {
	if len(w.ports) == 0 {
		return false
	}
	if !w.spec.Any {
		return w.anyBlocked()
	}
	for _, p := range w.ports {
		if p.State != WatchBlocked {
			return false
		}
	}
	return true
}

func (w *Watch) anyBlocked() bool {
	for _, p := range w.ports {
		if p.State == WatchBlocked {
			return true
		}
	}
	return false
}

// Next es el primer instante en que algún puerto que se está sosteniendo
// llegaría a cumplir lo pedido (cero si no hay ninguno): hasta ahí, a lo
// sumo, conviene dormir.
func (w *Watch) Next() time.Time {
	var out time.Time
	for _, p := range w.ports {
		if p.State != WatchSettling {
			continue
		}
		if t := p.Since.Add(w.spec.Stable); out.IsZero() || t.Before(out) {
			out = t
		}
	}
	return out
}

// --- El bucle ----------------------------------------------------------------

// WatchOS es lo que la espera necesita del sistema. Lo implementa el mismo
// adaptador que OS, y en las pruebas, un reloj falso con una historia
// guionada de la tabla.
type WatchOS interface {
	// Now es la hora actual (virtual en las pruebas).
	Now() time.Time
	// Listening lee la tabla LIVIANA: TCP en LISTEN y UDP, con la hora de
	// creación de cada socket y sin preguntar por el módulo dueño.
	Listening(ctx context.Context) ([]ports.Socket, error)
	// AllSockets lee la tabla completa, conexiones incluidas, también sin
	// preguntar por el módulo dueño (solo hace falta con --all-states).
	AllSockets(ctx context.Context) ([]ports.Socket, error)
	// Snapshot es la foto de los procesos (para saber a quién esperar).
	Snapshot(ctx context.Context) ([]Proc, error)
	// OpenWait abre un proceso SOLO para esperar su salida, con identidad
	// (ErrGone, ErrRecycled, ErrAccessDenied como Open).
	OpenWait(id ID) (Handle, error)
	// Wait es la espera sin sondeo de OS: hasta la salida de alguno de hs,
	// el plazo o la cancelación. Sin handles, una espera pura.
	Wait(ctx context.Context, hs []Handle, deadline time.Time) (Handle, ExitInfo, WaitOutcome)
}

// Intervalos de relectura: el primero corto (el servidor suele estar por
// aparecer, o el kernel por soltar el socket de un proceso que acaba de
// salir) y creciendo hasta un tope que deja la latencia por debajo de lo que
// un humano o un script notan.
const (
	watchPollMin = 25 * time.Millisecond
	watchPollMax = 200 * time.Millisecond
)

// WatchOutcome es en qué terminó la espera.
type WatchOutcome uint8

const (
	WatchDone       WatchOutcome = iota + 1 // se cumplió
	WatchTimeout                            // venció el plazo
	WatchCanceled                           // el usuario cortó (Ctrl+C)
	WatchImpossible                         // lo pedido no se puede cumplir (Windows tapa el puerto)
	WatchFailed                             // no se pudo leer la tabla
)

func (o WatchOutcome) String() string {
	switch o {
	case WatchDone:
		return "cumplido"
	case WatchTimeout:
		return "venció"
	case WatchCanceled:
		return "cancelado"
	case WatchImpossible:
		return "imposible"
	case WatchFailed:
		return "error"
	}
	return ""
}

// WatchReport es cómo terminó la espera.
type WatchReport struct {
	Outcome    WatchOutcome
	Ports      []WatchPort
	Start, End time.Time
	Reads      int   // cuántas veces se leyó la tabla
	Err        error // WatchFailed: por qué
}

// WaitPorts espera lo que dice spec hasta deadline, leyendo la tabla con
// backoff y durmiendo en el kernel entre lectura y lectura. emit recibe cada
// cambio apenas se lo ve (nil = nadie escucha). Siempre hace al menos una
// lectura: con un plazo ya vencido, es "mirar una vez".
//
// Invariantes (probadas en watch_test con reloj falso):
//   - no se declara cumplido nada que la última lectura no muestre cumplido;
//   - entre dos lecturas nunca pasa más de watchPollMax (salvo que se acabe
//     el plazo), así que un cambio se ve a lo sumo 200 ms tarde;
//   - una salida de un proceso dueño despierta al instante (WatchFree);
//   - todo handle abierto se cierra antes de volver.
func WaitPorts(ctx context.Context, os WatchOS, spec WatchSpec, deadline time.Time, emit func(WatchEvent)) WatchReport {
	w := NewWatch(spec)
	rep := WatchReport{Start: os.Now()}
	read := os.Listening
	if spec.For == WatchFree && spec.Filter.AllStates {
		read = os.AllSockets
	}
	own := newOwnerWaits(os)
	defer own.close()
	finish := func(o WatchOutcome) WatchReport {
		rep.Outcome, rep.Ports, rep.End = o, w.Ports(), os.Now()
		return rep
	}

	interval := watchPollMin
	for {
		socks, err := read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return finish(WatchCanceled)
			}
			rep.Err = err
			return finish(WatchFailed)
		}
		rep.Reads++
		now := os.Now()
		evs := w.Observe(now, socks)
		if emit != nil {
			for _, e := range evs {
				emit(e)
			}
		}
		switch {
		case w.Done():
			return finish(WatchDone)
		case w.Impossible():
			return finish(WatchImpossible)
		case ctx.Err() != nil:
			return finish(WatchCanceled)
		case !now.Before(deadline):
			return finish(WatchTimeout)
		}
		if len(evs) > 0 {
			interval = watchPollMin // algo se está moviendo: mirar seguido
		}

		wake := now.Add(interval)
		if n := w.Next(); !n.IsZero() && n.Before(wake) {
			wake = n
		}
		if wake.After(deadline) {
			wake = deadline
		}
		var hs []Handle
		if spec.For == WatchFree {
			hs = own.handles(ctx, w.ports)
		}
		h, _, out := os.Wait(ctx, hs, wake)
		switch out {
		case WaitCanceled:
			return finish(WatchCanceled)
		case WaitExited:
			own.exited(h)
			interval = watchPollMin // el kernel está por soltar su socket
		default:
			interval = min(interval*2, watchPollMax)
		}
	}
}

// ownerWaits son los handles de los procesos que ocupan los puertos que se
// esperan libres. Se abren por identidad y solo para esperar; la foto de
// procesos se pide solo cuando cambian los dueños.
type ownerWaits struct {
	os    WatchOS
	open  map[ID]Handle
	dead  map[ID]bool // ya salieron: no se vuelven a abrir
	which string      // de qué sockets salió el juego actual
}

func newOwnerWaits(os WatchOS) *ownerWaits {
	return &ownerWaits{os: os, open: map[ID]Handle{}, dead: map[ID]bool{}}
}

// handles devuelve a quién esperar: los dueños de los sockets que todavía
// ocupan algún puerto. Un socket cuyo PID ya no existe (o lo reusa un proceso
// más nuevo que el socket) lo sostiene un hijo que lo heredó: se espera a los
// candidatos a herederos. Quien no se deja abrir (System, otro usuario sin
// permiso) no se espera: para él queda la relectura.
func (o *ownerWaits) handles(ctx context.Context, wps []WatchPort) []Handle {
	var holders []ports.Socket
	for _, wp := range wps {
		if wp.State == WatchPending {
			holders = append(holders, wp.Witness...)
		}
	}
	if key := witnessKey(holders); key != o.which {
		o.which = key
		o.resolve(ctx, holders)
	}
	hs := make([]Handle, 0, len(o.open))
	for _, h := range o.open {
		hs = append(hs, h)
	}
	slices.SortFunc(hs, func(a, b Handle) int { return int(a.ID().PID) - int(b.ID().PID) })
	return hs
}

// resolve reabre el juego de handles para estos sockets.
func (o *ownerWaits) resolve(ctx context.Context, holders []ports.Socket) {
	want := map[ID]bool{}
	if len(holders) > 0 {
		if procs, err := o.os.Snapshot(ctx); err == nil {
			f := NewForest(procs)
			for _, s := range holders {
				h := f.Hold(s.PID, s.Since)
				if h.Live {
					want[h.Owner.ID] = true
					continue
				}
				for _, heir := range h.Heirs {
					want[heir.ID] = true
				}
			}
		}
	}
	for id, h := range o.open {
		if !want[id] {
			h.Close()
			delete(o.open, id)
		}
	}
	for id := range want {
		if _, ok := o.open[id]; ok || o.dead[id] || id.PID <= 4 {
			continue
		}
		if h, err := o.os.OpenWait(id); err == nil {
			o.open[id] = h
		}
	}
}

// exited saca de la espera a un proceso que salió.
func (o *ownerWaits) exited(h Handle) {
	if h == nil {
		return
	}
	id := h.ID().Canon()
	o.dead[id] = true
	if x, ok := o.open[id]; ok {
		x.Close()
		delete(o.open, id)
	}
}

func (o *ownerWaits) close() {
	for id, h := range o.open {
		h.Close()
		delete(o.open, id)
	}
}

// --- Falla rápida ------------------------------------------------------------

// Blocked dice si p NO VA A PODER ESCUCHAR mientras WinNAT no cambie sus
// bloques: en cada protocolo que pide el filtro, las dos familias caen en un
// bloque de WinNAT (excluido sin "*") y el bind exclusivo no lo ata (o no se
// sondeó). Esperar que un servidor escuche ahí es esperar en vano, así que la
// espera corta enseguida. Un puerto que se deja atar en alguna combinación, o
// que solo tapa una exclusión administrada (que un programa sí puede usar),
// no está bloqueado: el sondeo exclusivo que falla sin exclusión de por medio
// es ambiguo (un socket atado que todavía no escucha) y tampoco cuenta.
// Devuelve el diagnóstico del primer bloque, con sus salidas.
func (d *Diagnoser) Blocked(p uint16) (PortResult, bool) {
	fams := [...]ports.Family{ports.IPv4, ports.IPv6}
	// Primero el sondeo, que cuesta microsegundos: si alguna combinación se
	// deja atar (o ya la tiene alguien, que es lo contrario de estar tapado),
	// no hay bloqueo y no hace falta leer las exclusiones, que es correr
	// netsh. En la espera, el caso de siempre.
	if d.probe {
		for _, proto := range wantedProtos(d.filter) {
			for _, fam := range fams {
				if d.os.ProbeBind(proto, fam, p) != ports.BindForbidden {
					return PortResult{}, false
				}
			}
		}
	}
	d.load()
	var first PortResult
	got := false
	for _, proto := range wantedProtos(d.filter) {
		for _, fam := range fams {
			res := d.one(p, proto, fam)
			if res.Status != PortReserved || res.Reserved == (ports.ExcludedRange{}) || res.Reserved.Administered {
				return PortResult{}, false
			}
			if !got {
				first, got = res, true
			}
		}
	}
	return first, got
}
