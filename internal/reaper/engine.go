package reaper

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Options ajusta la escalada. El tiempo NO sale del reloj de la máquina: sale
// de OS.Now(), y toda espera pasa por OS.Wait/Sleep. Así los tests fijan
// tiempos exactos con un reloj virtual.
type Options struct {
	Timeout        time.Duration // plazo del cierre prolijo (peldaño 3); por defecto 3 s
	TerminateGrace time.Duration // espera breve tras terminar en seco (peldaño 4)
	// Settle es cuánto tiene que SEGUIR libre un puerto recién liberado antes
	// de declararlo libre. Cero = no confirmar (se declara al primer instante
	// libre): es el modo de los tests, con timing exacto. En producción es
	// corto (~300 ms): atrapa al supervisor (nodemon/pm2) que vuelve a tomar
	// el puerto apenas se libera, que es la queja clásica de "lo maté pero el
	// puerto sigue ocupado". No agrega espera si el puerto lo retomó alguien
	// (eso ya es un respawn, se informa al toque) ni si nunca estuvo ocupado.
	Settle time.Duration
	Filter ports.Filter // qué cuenta como "puerto ocupado" al verificar (proto/--all-states)
	Emit   func(Event)  // callback de eventos en vivo; nil = nadie escucha
}

func (o Options) timeout() time.Duration {
	if o.Timeout <= 0 {
		return 3 * time.Second
	}
	return o.Timeout
}

func (o Options) grace() time.Duration {
	if o.TerminateGrace <= 0 {
		return 500 * time.Millisecond
	}
	return o.TerminateGrace
}

// Run ejecuta la escalada del plan y verifica el resultado. before es el bosque
// de la instantánea con la que se planificó (se usa para nombres, ancestros y
// para desenmascarar respawns). requested son todos los puertos que pidió el
// usuario (para el veredicto por puerto). Devuelve un Report con el destino de
// cada víctima y de cada puerto.
//
// La escalada es un solo deadline por peldaño, todas las víctimas EN PARALELO,
// sin polling: se abre, se detienen servicios, se pide el cierre prolijo, se
// espera, y a los que quedan vivos se los termina en seco. Un Ctrl+C del
// usuario corta y deja todo como está.
//
// Invariantes (probadas en scenarios_test):
//   - a nadie se lo termina en seco sin haber pasado antes por su oportunidad
//     de cerrar prolijo, salvo --force o que no haya por dónde pedírselo;
//   - el plazo del peldaño 3 solo corre si ALGUIEN recibió una señal prolija:
//     esperar a un proceso al que no se le pidió nada es tiempo perdido (no
//     tiene motivo para irse). Así --force y "sin consola ni ventanas" van
//     directo al peldaño 4, sin regalar el --timeout entero;
//   - después de un Ctrl+C del usuario no se manda ninguna señal más.
func Run(ctx context.Context, os OS, before *Forest, requested ports.Set, plan Plan, cfg Config, opts Options) Report {
	em := emitter(opts.Emit)
	st := newRunState(plan, before)
	defer st.closeAll()

	// Peldaño 0: abrir handles CON IDENTIDAD. Sostener el handle impide que el
	// PID se recicle entre acá y el final.
	st.openAll(os, em)

	// Peldaño 1: servicios de terceros. Detener el servicio, no matar el
	// proceso (si lo mato, el administrador de servicios lo reinicia).
	canceled := st.stopServices(ctx, os, plan, opts, em)
	canceled = canceled || ctxCanceled(ctx)

	// Peldaño 1b: objetivos externos (contenedores, procesos Linux). Se
	// detienen con la operación de su dominio, todos a la vez y con un solo
	// plazo; cada dominio hace su propia escalera (docker stop: SIGTERM y al
	// vencer, SIGKILL; en Linux: SIGINT, SIGTERM, SIGKILL). El intermediario
	// que los sirve no se toca.
	if !canceled {
		canceled = st.stopExternals(ctx, os, cfg, opts, em)
	}

	// Peldaño 2: cierre prolijo (salvo --force).
	if !canceled && !cfg.Force {
		st.sendGraceful(os, before, cfg, em)
	}

	// Peldaño 3: esperar hasta el plazo a los que recibieron una señal
	// prolija, avisando cada salida (también la de los que se van solos). La
	// espera termina apenas salieron todos los señalados: los demás no tienen
	// por qué irse y los recoge el peldaño 4.
	if !canceled && st.anyLive(signaled) {
		deadline := os.Now().Add(opts.timeout())
		em.emit(Event{Kind: EvWaiting, Time: os.Now(), Deadline: deadline})
		canceled = st.waitExits(ctx, os, deadline, st.gracefulMethod, signaled, em)
	}
	canceled = canceled || ctxCanceled(ctx)

	// Peldaño 4: a los que siguen vivos, terminación en seco (raíz primero,
	// después el subárbol: el orden de plan.Victims ya lo garantiza).
	if !canceled {
		canceled = st.terminateRest(ctx, os, opts, em)
	}

	// Cerrar cuentas: lo que quedó sin veredicto.
	st.settle(canceled)

	if canceled {
		em.emit(Event{Kind: EvCanceled, Time: os.Now()})
	}

	report := Report{Victims: st.results(), Externals: st.ext, Canceled: canceled, Reached: st.reached}
	report.Ports = Verify(ctx, os, before, requested, plan, report, opts)
	return report
}

// openAll implementa el peldaño 0: abre cada víctima con identidad (PID y hora
// de creación). La que no se deja abrir queda con su veredicto: ya había
// cerrado, se recicló el PID, está protegida, es crítica o se negó el acceso.
func (st *runState) openAll(os OS, em emitter) {
	for _, id := range st.order {
		v := st.v[id]
		h, err := os.Open(id)
		switch {
		case err == nil:
			v.handle = h
			em.emit(Event{Kind: EvOpened, Time: os.Now(), Victim: id, Name: v.name})
		case is(err, ErrGone):
			v.set(OutcomeGone, MethodNone, err)
			em.emit(Event{Kind: EvOpenFailed, Time: os.Now(), Victim: id, Name: v.name, Detail: "ya había cerrado", Err: err})
		case is(err, ErrRecycled):
			v.set(OutcomeRecycled, MethodNone, err)
			em.emit(Event{Kind: EvOpenFailed, Time: os.Now(), Victim: id, Name: v.name, Detail: "el PID se recicló: es otro proceso, no lo toco", Err: err})
		case is(err, ErrProtected):
			v.set(OutcomeProtected, MethodNone, err)
			em.emit(Event{Kind: EvOpenFailed, Time: os.Now(), Victim: id, Name: v.name, Detail: "protegido (PPL)", Err: err})
		case is(err, ErrCritical):
			// Marcado como crítico: terminarlo (o que se vaya con un Ctrl+C)
			// tira un bugcheck. Sin handle no recibe ninguna señal: ni la
			// prolija ni la otra.
			v.set(OutcomeCritical, MethodNone, err)
			em.emit(Event{Kind: EvOpenFailed, Time: os.Now(), Victim: id, Name: v.name, Detail: "está marcado como proceso crítico: no lo toco", Err: err})
		case is(err, ErrAccessDenied):
			v.set(OutcomeDenied, MethodNone, err)
			em.emit(Event{Kind: EvOpenFailed, Time: os.Now(), Victim: id, Name: v.name, Detail: "acceso denegado", Err: err})
		default:
			v.set(OutcomeError, MethodNone, err)
			em.emit(Event{Kind: EvOpenFailed, Time: os.Now(), Victim: id, Name: v.name, Err: err})
		}
	}
}

// stopServices implementa el peldaño 1: detiene los servicios de terceros por
// el administrador de servicios. Devuelve true si se canceló.
func (st *runState) stopServices(ctx context.Context, os OS, plan Plan, opts Options, em emitter) bool {
	canceled := false
	for _, t := range plan.Targets {
		if t.Kind != StopThirdSvc {
			continue
		}
		if canceled || ctxCanceled(ctx) {
			canceled = true
			break
		}
		em.emit(Event{Kind: EvServiceStopping, Time: os.Now(), Victim: t.Root, Name: t.Service})
		err := os.StopService(ctx, t.Service, os.Now().Add(opts.timeout()))
		switch {
		case err == nil:
			for _, id := range t.Victims {
				st.v[id].set(OutcomeServiceStopped, MethodService, nil)
			}
			em.emit(Event{Kind: EvServiceStopped, Time: os.Now(), Victim: t.Root, Name: t.Service})
		case is(err, ErrServiceAccessDenied):
			st.failService(t, OutcomeDenied, err)
			em.emit(Event{Kind: EvServiceFailed, Time: os.Now(), Victim: t.Root, Name: t.Service, Detail: "necesita administrador", Err: err})
		case ctxCanceled(ctx):
			// El usuario cortó mientras se esperaba el STOPPED. El pedido de
			// detenerlo ya se mandó y el servicio sigue deteniéndose: no es
			// una falla del servicio. La víctima queda sin veredicto, como
			// todo lo que el Ctrl+C deja a mitad, con el motivo al lado.
			for _, id := range t.Victims {
				st.v[id].err = ErrStopPending
			}
			canceled = true
		default:
			st.failService(t, OutcomeError, err)
			em.emit(Event{Kind: EvServiceFailed, Time: os.Now(), Victim: t.Root, Name: t.Service, Detail: err.Error(), Err: err})
		}
	}
	return canceled
}

// terminateRest implementa el peldaño 4: terminación en seco de los que siguen
// vivos, en el orden del plan, y una espera breve a que el kernel lo confirme.
// Devuelve true si se canceló durante esa espera.
func (st *runState) terminateRest(ctx context.Context, os OS, opts Options, em emitter) bool {
	terminated := false
	for _, id := range st.order {
		v := st.v[id]
		if v.handle == nil || v.exited || v.isService || v.outcome != OutcomePending {
			continue
		}
		if !v.handle.CanTerminate() {
			v.set(OutcomeDenied, MethodNone, ErrAccessDenied)
			em.emit(Event{Kind: EvTerminateFailed, Time: os.Now(), Victim: id, Name: v.name, Detail: "acceso denegado para terminar", Err: ErrAccessDenied})
			continue
		}
		sent := os.Now()
		err := v.handle.Terminate()
		switch {
		case err == nil, is(err, ErrGone):
			v.terminating = true
			v.sentAt = sent
			terminated = true
			em.emit(Event{Kind: EvTerminated, Time: os.Now(), Victim: id, Name: v.name})
		case is(err, ErrAccessDenied):
			v.set(OutcomeDenied, MethodNone, err)
			em.emit(Event{Kind: EvTerminateFailed, Time: os.Now(), Victim: id, Name: v.name, Err: err})
		default:
			v.set(OutcomeError, MethodNone, err)
			em.emit(Event{Kind: EvTerminateFailed, Time: os.Now(), Victim: id, Name: v.name, Err: err})
		}
	}
	if !terminated {
		return false
	}
	// Espera breve a que el kernel confirme la terminación.
	deadline := os.Now().Add(opts.grace())
	em.emit(Event{Kind: EvWaiting, Time: os.Now(), Deadline: deadline, Method: MethodTerminate})
	return st.waitExits(ctx, os, deadline, func(ID) Method { return MethodTerminate }, terminating, em)
}

// settle cierra las cuentas de la corrida: los que se terminaron y no se llegó
// a confirmar se dan por terminados (el kernel los está bajando); el resto de
// los pendientes, error, salvo que se haya cancelado (quedan sin veredicto). Un
// externo sin veredicto de su dominio, error.
func (st *runState) settle(canceled bool) {
	for _, id := range st.order {
		v := st.v[id]
		if v.outcome != OutcomePending {
			continue
		}
		switch {
		case v.terminating:
			v.set(OutcomeTerminated, MethodTerminate, nil)
		case canceled:
			// Se cortó a mitad: la víctima queda como estaba, sin veredicto.
			// Va antes que la de los servicios: un servicio pendiente es uno
			// al que el Ctrl+C no dejó llegar (o que se cortó mientras se
			// detenía), no uno detenido.
		case v.isService:
			v.set(OutcomeServiceStopped, MethodService, nil)
		default:
			v.set(OutcomeError, MethodNone, nil)
		}
	}

	for i := range st.ext {
		if e := &st.ext[i]; e.Outcome == OutcomePending && !canceled {
			// El dominio volvió sin dar veredicto (un adaptador que no cumplió
			// su contrato): no se lo puede dar por detenido.
			e.Outcome, e.Err = OutcomeError, errNoVerdict
		}
	}
}

// errNoVerdict: el dominio de un externo no informó cómo terminó.
var errNoVerdict = errors.New("el dominio no informó si lo detuvo")

// stopExternals implementa el peldaño 1b. Devuelve true si se canceló.
func (st *runState) stopExternals(ctx context.Context, os OS, cfg Config, opts Options, em emitter) bool {
	if len(st.ext) == 0 {
		return false
	}
	// Con --force no hay cierre prolijo que narrar: el dominio va directo a la
	// fuerza (docker stop con t=0, SIGKILL), y los eventos lo dicen para que la
	// interfaz no prometa un SIGINT que nadie va a mandar.
	how, waiting := MethodNone, MethodEngine
	if cfg.Force {
		how, waiting = MethodTerminate, MethodTerminate
	}
	xs := make([]External, len(st.ext))
	for i, e := range st.ext {
		xs[i] = e.External
		em.emit(Event{Kind: EvExternalStopping, Time: os.Now(), Name: e.External.Name, External: e.External, Method: how})
	}
	start := os.Now()
	deadline := start.Add(opts.timeout())
	em.emit(Event{Kind: EvWaiting, Time: start, Deadline: deadline, Method: waiting})
	os.StopExternal(ctx, xs, deadline, cfg.Force, func(i int, r ExternalStop) {
		// Defensivo: un índice fuera de rango o un segundo veredicto para el
		// mismo externo no pisan lo ya decidido.
		if i < 0 || i >= len(st.ext) || st.ext[i].Outcome != OutcomePending {
			return
		}
		if r.Outcome == OutcomePending {
			r.Outcome, r.Err = OutcomeError, errNoVerdict
		}
		e := &st.ext[i]
		e.Outcome, e.Method, e.Err, e.Elapsed = r.Outcome, r.Method, r.Err, os.Now().Sub(start)
		ev := Event{Kind: EvExternalStopped, Time: os.Now(), Name: e.External.Name, External: e.External,
			Outcome: e.Outcome, Method: e.Method, Elapsed: e.Elapsed, Err: e.Err}
		if !e.Outcome.Done() {
			ev.Kind = EvExternalFailed
			if e.Err != nil {
				ev.Detail = e.Err.Error()
			}
		}
		em.emit(ev)
	})
	return ctxCanceled(ctx)
}

// --- Estado de una corrida -------------------------------------------------

type vstate struct {
	id          ID
	name        string
	handle      Handle
	isService   bool // pertenece a un objetivo StopThirdSvc
	signal      Method
	sentAt      time.Time // cuándo se le pidió que saliera (Ctrl+C, WM_CLOSE o terminación): de ahí se mide cuánto tardó
	exited      bool
	terminating bool
	outcome     Outcome
	method      Method
	elapsed     time.Duration
	err         error
}

func (v *vstate) set(o Outcome, m Method, err error) {
	v.outcome, v.method, v.err = o, m, err
}

// Predicados de "a quién se espera" en cada peldaño.
func signaled(v *vstate) bool    { return v.signal != MethodNone }
func terminating(v *vstate) bool { return v.terminating }

type runState struct {
	order     []ID
	v         map[ID]*vstate
	ext       []ExternalResult // objetivos externos, en el orden del plan
	waitStart time.Time
	// aside: los que quedaron al margen del pedido (Plan.Bystanders). Al
	// usuario se le dijo que no se tocan, así que tampoco pueden recibir el
	// Ctrl+C de una consola que comparten con un objetivo.
	aside map[ID]bool
	// reached: los que recibieron un Ctrl+C sin ser víctimas (Report.Reached).
	reached []Proc
}

func newRunState(plan Plan, before *Forest) *runState {
	st := &runState{v: map[ID]*vstate{}, aside: asideOf(plan)}
	svcVictim := map[ID]bool{}
	for _, t := range plan.Targets {
		if t.Kind == StopThirdSvc {
			for _, id := range t.Victims {
				svcVictim[id] = true
			}
		}
	}
	for _, t := range plan.Targets {
		if t.Kind == StopExternal {
			st.ext = append(st.ext, ExternalResult{External: t.External, Outcome: OutcomePending})
		}
	}
	for _, id := range plan.Victims() {
		id = id.Canon() // las claves del mapa son SIEMPRE canónicas (ver ID.Canon)
		if _, dup := st.v[id]; dup {
			continue // una víctima que aparece en dos objetivos se abre una vez
		}
		name := ""
		if p, ok := before.Lookup(id); ok {
			name = p.Name
		}
		st.order = append(st.order, id)
		st.v[id] = &vstate{id: id, name: name, isService: svcVictim[id], outcome: OutcomePending}
	}
	return st
}

func (st *runState) closeAll() {
	for _, v := range st.v {
		if v.handle != nil {
			v.handle.Close()
		}
	}
}

func (st *runState) failService(t Target, o Outcome, err error) {
	for _, id := range t.Victims {
		st.v[id].set(o, MethodNone, err)
	}
}

// live dice si la víctima está abierta y sin salir. Los servicios que ya
// bajamos no se esperan acá: su proceso muere por el servicio, no por nosotros.
func (v *vstate) live() bool { return v.handle != nil && !v.exited && !v.isService }

// openLive devuelve los handles de víctimas todavía vivas.
func (st *runState) openLive() []Handle {
	var hs []Handle
	for _, id := range st.order {
		if v := st.v[id]; v.live() {
			hs = append(hs, v.handle)
		}
	}
	return hs
}

// anyLive dice si queda alguna víctima viva que cumpla awaited.
func (st *runState) anyLive(awaited func(*vstate) bool) bool {
	for _, id := range st.order {
		if v := st.v[id]; v.live() && awaited(v) {
			return true
		}
	}
	return false
}

// waitExits espera las salidas hasta deadline (o cancelación), marcando cada
// una con el método que corresponda. Escucha a TODAS las vivas (si una sin
// señal se va sola, se la registra), pero vuelve apenas no queda ninguna viva
// que cumpla awaited: esperar a quien no tiene motivo para irse es regalar el
// plazo. Devuelve true si se canceló.
func (st *runState) waitExits(ctx context.Context, os OS, deadline time.Time, methodFor func(ID) Method, awaited func(*vstate) bool, em emitter) bool {
	st.waitStart = os.Now()
	for st.anyLive(awaited) {
		h, info, outcome := os.Wait(ctx, st.openLive(), deadline)
		switch outcome {
		case WaitExited:
			// Canon: el handle puede traer la hora de creación en otra
			// representación (zona local, lectura monotónica) que la clave del
			// mapa; sin canonizar, la víctima no se reencontraría y la espera
			// giraría en falso. Ver ID.Canon y newRunState.
			id := h.ID().Canon()
			v := st.v[id]
			if v == nil || v.exited {
				continue
			}
			v.exited = true
			v.elapsed = st.took(v, info, os.Now())
			m := methodFor(id)
			if v.isService {
				v.set(OutcomeServiceStopped, MethodService, nil)
			} else if m == MethodTerminate {
				v.set(OutcomeTerminated, MethodTerminate, nil)
			} else {
				v.set(OutcomeClosed, m, nil)
			}
			em.emit(Event{Kind: EvExited, Time: os.Now(), Victim: id, Name: v.name, Method: v.method, Elapsed: v.elapsed})
		case WaitDeadline:
			return false
		case WaitCanceled:
			return true
		}
	}
	return false
}

// took es cuánto tardó una víctima en salir: desde que se le pidió (o, si no
// se le pidió nada, desde que se empezó a esperar) hasta la hora de salida que
// da el kernel, o hasta ahora si no la da. Sin esto, uno que cede mientras el
// ayudante del Ctrl+C todavía está volviendo figuraba con 0 ms, y a los
// lentos se les descontaba lo que tarda el ayudante.
func (st *runState) took(v *vstate, info ExitInfo, now time.Time) time.Duration {
	from := v.sentAt
	if from.IsZero() {
		from = st.waitStart
	}
	at := info.At
	if at.IsZero() || at.After(now) {
		at = now
	}
	return max(at.Sub(from), 0)
}

// gracefulMethod dice con qué señal salió una víctima durante el cierre
// prolijo: el Ctrl+C o el WM_CLOSE que le mandamos, o "salió solo" si no le
// mandamos nada (pero igual se fue).
func (st *runState) gracefulMethod(id ID) Method {
	if m := st.v[id].signal; m != MethodNone {
		return m
	}
	return MethodSelf
}

// sendGraceful implementa el peldaño 2: UN Ctrl+C por consola distinta de las
// raíces (con el conjunto permitido = ancestros ∪ raíz ∪ subárbol; nunca la
// consola propia ni una compartida con ajenos) y WM_CLOSE a las víctimas con
// ventanas.
func (st *runState) sendGraceful(os OS, before *Forest, cfg Config, em emitter) {
	// Agrupar las raíces vivas por consola. La clave es el conjunto ordenado de
	// PID de la consola: dos raíces con la misma lista comparten consola y no
	// hay que mandarles dos Ctrl+C.
	type consoleGroup struct {
		rep     ID
		pids    []uint32
		allow   map[ID]bool
		members []ID
	}
	byConsole := map[string]*consoleGroup{}
	var consoleOrder []string
	for _, id := range st.order {
		v := st.v[id]
		if !v.live() {
			continue
		}
		pids, err := os.ConsolePIDs(id)
		if err != nil || len(pids) == 0 {
			continue // sin consola: se resolverá con WM_CLOSE o terminación
		}
		key := consoleKey(pids)
		g := byConsole[key]
		if g == nil {
			g = &consoleGroup{rep: id, pids: pids, allow: map[ID]bool{}}
			byConsole[key] = g
			consoleOrder = append(consoleOrder, key)
		}
		g.members = append(g.members, id)
		// Conjunto permitido: la raíz, sus ancestros y su subárbol, sin lo que
		// quedó al margen del pedido (ver ctrlCAllow: la misma regla con que
		// la herramienta avisa antes de preguntar). Un descendiente marcado
		// crítico tampoco entra: si comparte la consola, el Ctrl+C no se manda
		// (lo bajaría también a él).
		ctrlCAllow(before, id, st.aside, func(d ID) bool {
			x := st.v[d]
			return x != nil && x.outcome == OutcomeCritical
		}, g.allow)
	}

	for _, key := range consoleOrder {
		g := byConsole[key]
		// Nunca a la consola propia de killport (nos llevaríamos puesta la
		// terminal del usuario y a killport mismo).
		if sharesOwnConsole(g.pids, cfg.OwnConsole) {
			em.emit(Event{Kind: EvCtrlCSkipped, Time: os.Now(), Victim: g.rep, Name: st.v[g.rep].name, Detail: "es la consola propia de killport"})
			continue
		}
		allow := make([]ID, 0, len(g.allow))
		for id := range g.allow {
			allow = append(allow, id)
		}
		slices.SortFunc(allow, func(a, b ID) int { return int(a.PID) - int(b.PID) })
		// La hora es la de ANTES de pedirlo: el Ctrl+C lo manda un ayudante
		// que tarda en arrancar, y un proceso que cede rápido ya salió
		// cuando el ayudante vuelve.
		sent := os.Now()
		err := os.CtrlC(g.rep, allow)
		switch {
		case err == nil:
			members := slices.Clone(g.members)
			for _, id := range g.members {
				st.v[id].signal, st.v[id].sentAt = MethodCtrlC, sent
			}
			// Un ancestro que comparte la consola también recibe el Ctrl+C: si
			// resulta ser una víctima, marcarlo.
			for _, id := range st.order {
				if v := st.v[id]; v.signal == MethodNone && consoleHas(g.pids, id.PID) {
					v.signal, v.sentAt = MethodCtrlC, sent
					members = append(members, id)
				}
			}
			// Y los que no son víctimas: se dice (Event.Also, Report.Reached).
			var also []Proc
			for _, pid := range g.pids {
				if p, ok := before.LivePID(pid); ok && g.allow[p.ID] && st.v[p.ID] == nil {
					also = append(also, p)
				}
			}
			st.reached = append(st.reached, also...)
			em.emit(Event{Kind: EvCtrlC, Time: os.Now(), Victim: g.rep, Name: st.v[g.rep].name, Members: members, Also: also})
		case is(err, ErrNoConsole):
			// entre la consulta y el envío perdió la consola: nada que hacer
		default:
			var shared *SharedConsoleError
			detail := "la consola la comparten otros procesos"
			if asShared(err, &shared) {
				detail = shared.Error()
			}
			em.emit(Event{Kind: EvCtrlCSkipped, Time: os.Now(), Victim: g.rep, Name: st.v[g.rep].name, Detail: detail, Err: err})
		}
	}

	// WM_CLOSE a las víctimas con ventanas.
	for _, id := range st.order {
		v := st.v[id]
		if !v.live() {
			continue
		}
		sent := os.Now()
		if n, err := os.CloseWindows(id); err == nil && n > 0 {
			if v.signal == MethodNone {
				v.signal, v.sentAt = MethodClose, sent
			}
			em.emit(Event{Kind: EvClose, Time: os.Now(), Victim: id, Name: v.name})
		}
	}
}

func (st *runState) results() []VictimResult {
	out := make([]VictimResult, 0, len(st.order))
	for _, id := range st.order {
		v := st.v[id]
		out = append(out, VictimResult{ID: id, Name: v.name, Outcome: v.outcome, Method: v.method, Elapsed: v.elapsed, Err: v.err})
	}
	return out
}

// --- Verificación ----------------------------------------------------------

// probeLimit acota el sondeo de bind: con más puertos sin dueño que esto (un
// "killport 1-65535"), el diagnóstico se queda con la tabla de rangos
// excluidos. Cuatro binds por puerto (TCP/UDP × v4/v6) son ~0,2 ms: 256
// puertos son 50 ms; 65.536 serían 13 s de sondeo para confirmar lo obvio.
const probeLimit = 256

// Verify relee las tablas con backoff exponencial corto y dictamina puerto por
// puerto: libre, fantasma, respawn, reservado, sigue ocupado o no se toca.
// done es lo que pasó en la escalada —el destino de cada víctima y de cada
// externo, el que arma Run o el que se recompone al volver de la elevación;
// sus Ports se ignoran—: de ahí sale POR QUÉ un puerto sigue ocupado. Sin
// escalada (solo mirar), va vacío. Se exporta para que la herramienta vuelva
// a verificar por su cuenta después de que actuó un proceso elevado (no le
// cree al informe ajeno).
//
// La espera es la justa: se relee solo mientras algún puerto pedido esté en
// manos de un "muerto reciente" (una víctima que ya salió, o el dueño
// original de un socket heredado), que es lo único que el kernel está por
// soltar. Un puerto libre, o tomado por alguien vivo que no es víctima
// (respawn, heredero), ya está asentado: esperar no lo cambia. Tope ~2 s.
func Verify(ctx context.Context, os OS, before *Forest, requested ports.Set, plan Plan, done Report, opts Options) []PortResult {
	if requested.IsEmpty() {
		return nil
	}
	em := emitter(opts.Emit)
	em.emit(Event{Kind: EvVerifying, Time: os.Now()})
	filter := opts.Filter
	filter.Ports = requested
	sc := newVerifyScope(plan, filter)

	// Los muertos recientes: víctimas que ya salieron y dueños originales de
	// los sockets de los objetivos que no son víctimas (los fantasmas).
	// Mientras la tabla les atribuya un puerto, vale esperar. Un intermediario
	// cuenta SOLO en los puertos de un externo que de verdad se detuvo: suelta
	// ese puerto un instante después, pero los demás que sostiene (los que se
	// negaron, los de otros contenedores) no los va a soltar, y esperarlos
	// sería regalar el tope entero.
	victims := done.Victims
	extFate := map[string]ExternalResult{}
	for _, e := range done.Externals {
		extFate[e.External.Key] = e
	}
	type pidPort struct {
		pid  uint32
		port uint16
	}
	lag := map[uint32]bool{}
	lagPort := map[pidPort]bool{}
	victimPID := map[uint32]bool{}
	for _, v := range victims {
		victimPID[v.ID.PID] = true
		if v.Outcome.Done() {
			lag[v.ID.PID] = true
		}
	}
	fate := map[ID]VictimResult{}
	for _, v := range victims {
		fate[v.ID.Canon()] = v
	}
	for _, t := range plan.Targets {
		if t.Kind == StopExternal {
			if e, ok := extFate[t.External.Key]; ok && e.Outcome.Done() {
				for _, s := range t.Sockets {
					lagPort[pidPort{s.PID, s.Port()}] = true
				}
			}
			continue
		}
		for _, s := range t.Sockets {
			switch {
			case victimPID[s.PID]:
			case !t.Via.Zero() && s.PID == t.Via.PID:
				// El socket es del intermediario (http.sys): lo suelta un
				// instante después de que se va el proceso de atrás, y solo
				// ese puerto. Los demás que sostiene no se esperan.
				if rootDone(t, fate) {
					lagPort[pidPort{s.PID, s.Port()}] = true
				}
			default:
				lag[s.PID] = true // el dueño original de un socket heredado
			}
		}
	}
	unsettled := func(socks []ports.Socket) bool {
		for _, s := range socks {
			if filter.Relate(s) == ports.Holds && (lag[s.PID] || lagPort[pidPort{s.PID, s.Port()}]) {
				return true
			}
		}
		return false
	}
	// Puertos que ATACAMOS (los de los objetivos): son los que, si quedan
	// libres, hay que confirmar que SIGAN libres —un supervisor los recupera al
	// instante—. Un puerto que solo aparecía como negativa o sin dueño no se
	// vigila (nadie lo va a relanzar por nosotros).
	acted := ports.Of()
	for _, t := range plan.Targets {
		acted = acted.Union(t.Ports)
	}
	// watchedFree: puertos atacados que ahora mismo están libres (los que
	// esperan confirmación). Vacío ⇒ o no atacamos nada, o lo que atacamos ya
	// lo tiene alguien de nuevo (respawn/busy): no hay nada que confirmar.
	watchedFree := func(socks []ports.Socket) int {
		ocup := sc.holders(socks)
		n := 0
		for p := range acted.All() {
			if _, held := ocup[p]; !held {
				n++
			}
		}
		return n
	}

	socks := sampleUntilQuiet(ctx, os, unsettled, watchedFree, opts.Settle)

	// Bosque de DESPUÉS para el dictamen.
	var after *Forest
	if procs, err := os.Snapshot(ctx); err == nil {
		after = NewForest(procs)
	} else {
		after = NewForest(nil)
	}

	// Un supervisor que relanza a la víctima más lento de lo que dura Settle
	// (ver relaunch.go): se lo espera, o no se da el puerto por libre. Es
	// parte de la confirmación, así que sin Settle (los tests con tiempos
	// exactos) no se mira.
	var relaunched map[uint16]relaunch
	if opts.Settle > 0 && ctx.Err() == nil {
		if roots := fallenRoots(before, plan, done); len(roots) > 0 {
			var self map[ID]bool
			if plan.w != nil {
				self = plan.w.selfChain
			}
			socks, after, relaunched = watchRelaunch(ctx, os, before, after, roots, self, sc, socks)
		}
	}

	idx := newPortIndex(before, plan, victims, extFate)
	// Socket que ocupa cada puerto ahora: el primero de la tabla, o el que
	// contesta la dirección pedida; nunca el de alguien que no se pidió.
	nowSock := sc.holders(socks)
	sc.now = socks

	// Los rangos excluidos (netsh, caro) se leen una sola vez y solo si algún
	// puerto quedó sin dueño.
	diag := NewDiagnoser(os, opts.Filter, requested.Len() <= probeLimit).Seen(socks, after)

	var out []PortResult
	for p := range requested.All() {
		res := classifyOne(before, after, diag, idx, sc, nowSock, p)
		if rl, ok := relaunched[p]; ok && res.Status == PortFree {
			res = relaunchResult(p, rl)
		}
		if res.Status == PortFree {
			em.emit(Event{Kind: EvPortFreed, Time: os.Now(), Port: p})
		} else {
			em.emit(Event{Kind: EvPortStuck, Time: os.Now(), Port: p, Detail: res.Advice})
		}
		out = append(out, res)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// sampleUntilQuiet relee las tablas con backoff hasta que el estado se
// aquieta: ni un muerto reciente ocupando un puerto pedido (unsettled), y los
// puertos que atacamos, si quedaron libres, confirmados libres por al menos
// `settle`. Corta antes si no queda ningún puerto atacado libre que vigilar
// (todo resuelto: libre-y-nada-que-esperar, o ya retomado). Devuelve la última
// lectura. Tope ~2 s; respeta la cancelación de ctx.
func sampleUntilQuiet(ctx context.Context, os OS, unsettled func([]ports.Socket) bool, watchedFree func([]ports.Socket) int, settle time.Duration) []ports.Socket {
	socks, _ := os.Sockets(ctx)
	if ctx.Err() != nil {
		return socks
	}
	var limpioDesde time.Time
	for _, d := range backoff(2 * time.Second) {
		switch {
		case unsettled(socks):
			limpioDesde = time.Time{} // el kernel todavía suelta: reiniciar la cuenta
		case watchedFree(socks) == 0:
			return socks // nada libre que vigilar, o ya lo retomó alguien (respawn)
		default: // estado limpio con puertos atacados libres: confirmar que sigan
			if limpioDesde.IsZero() {
				limpioDesde = os.Now() // primer instante libre
			}
			if os.Now().Sub(limpioDesde) >= settle { // con settle 0, confirma ya
				return socks
			}
		}
		if !os.Sleep(ctx, d) {
			return socks // cancelado
		}
		socks, _ = os.Sockets(ctx)
	}
	return socks
}

// portIndex es lo que se sabía de cada puerto ANTES de actuar.
type portIndex struct {
	holders map[uint16]map[ID]bool // quiénes lo ocupaban: dueños de sus sockets y víctimas de su objetivo
	rep     map[uint16]ID          // víctima de referencia (la raíz del objetivo) para el LCA cruzado
	refusal map[uint16][]*Refusal  // negativas que lo cubrían (puede haber varias: el motor y wslrelay en --no-docker)
	fate    map[ID]VictimResult    // destino de cada víctima
	ext     map[uint16]extPort     // externo que lo publicaba, con su intermediario y su destino
	via     map[uint16]Target      // proceso de Windows que lo tenía A TRAVÉS de un intermediario (http.sys)
}

// extPort es el externo detrás de un puerto y cómo le fue.
type extPort struct {
	target Target
	fate   ExternalResult
	known  bool // hubo escalada (si no, solo se está mirando)
}

func newPortIndex(before *Forest, plan Plan, victims []VictimResult, extFate map[string]ExternalResult) portIndex {
	idx := portIndex{holders: map[uint16]map[ID]bool{}, rep: map[uint16]ID{}, refusal: map[uint16][]*Refusal{}, fate: map[ID]VictimResult{},
		ext: map[uint16]extPort{}, via: map[uint16]Target{}}
	add := func(p uint16, id ID) {
		if idx.holders[p] == nil {
			idx.holders[p] = map[ID]bool{}
		}
		idx.holders[p][id] = true
	}
	for _, t := range plan.Targets {
		for p := range t.Ports.All() {
			if t.Kind == StopExternal {
				f, ok := extFate[t.External.Key]
				idx.ext[p] = extPort{target: t, fate: f, known: ok}
				continue // no hay víctima de Windows: el LCA cruzado no aplica
			}
			idx.rep[p] = t.Root
			for _, v := range t.Victims {
				add(p, v)
			}
			if !t.Via.Zero() {
				idx.via[p] = t
			}
		}
		for _, s := range t.Sockets {
			// El dueño del socket, si es el real. El que hoy tiene el PID de
			// un fantasma es otro proceso: no "ocupaba" nada.
			if h := before.Hold(s.PID, s.Since); h.Live {
				add(s.Port(), h.Owner.ID)
			}
		}
	}
	for i := range plan.Refusals {
		r := &plan.Refusals[i]
		for p := range r.Ports.All() {
			idx.refusal[p] = append(idx.refusal[p], r)
			if !r.Owner.Zero() {
				add(p, r.Owner)
			}
			if !r.Via.Zero() {
				add(p, r.Via.Canon()) // el que figura en la tabla es el intermediario
			}
		}
	}
	for _, v := range victims {
		idx.fate[v.ID] = v
	}
	return idx
}

func classifyOne(before, after *Forest, diag *Diagnoser, idx portIndex, sc *verifyScope, nowSock map[uint16]ports.Socket, p uint16) PortResult {
	res := PortResult{Port: p, Status: PortFree}
	s, held := nowSock[p]
	if !held {
		if b, note, answers, ok := sc.stillThere(p); ok {
			// Libre para lo pedido, y atado por alguien que no se pidió: no
			// hay nada que diagnosticar (no lo reserva Windows: lo tiene
			// alguien), solo decir quién. Si ese alguien contesta ahora la
			// dirección pedida, la dirección NO quedó libre.
			res.Bystander, res.BystanderName, res.Reason = b.Owner, b.Name, note
			if answers {
				res.Status = PortAnswered
			}
			return res
		}
		if len(idx.holders[p]) > 0 {
			// Lo tenía alguien hace un instante: no puede caer en un rango
			// excluido (Windows le habría negado el bind). Está libre, y no
			// hace falta preguntarle nada a netsh.
			return res
		}
		// Nadie lo tenía ni lo tiene: ¿está de verdad libre, o lo reserva
		// Windows? Es el misterio de "ocupado sin dueño".
		return diag.Port(p)
	}
	res.Proto = s.Proto

	hold := after.Hold(s.PID, s.Since)
	owner := hold.Owner
	if !hold.Live {
		// Fantasma: el dueño murió y unos hijos heredaron el socket (los hijos
		// del que reusó su PID, no: ver Forest.Heirs).
		res.Status = PortPhantom
		heirs := hold.Heirs
		for _, h := range heirs {
			res.Heirs = append(res.Heirs, h.ID)
		}
		if len(heirs) > 0 {
			res.Owner, res.OwnerName = heirs[0].ID, heirs[0].Name
		}
		switch r := idx.refusalOf(p, heirs); {
		case len(heirs) == 0:
			res.Advice = "el dueño murió pero el socket sigue; debería liberarse solo en unos segundos"
		case r != nil:
			// El heredero es intocable (la planificación lo negó): volver a
			// correr killport no lo va a cerrar.
			res.Status = PortRefused
			res.Reason, res.Advice = r.Reason, r.Advice
		case idx.holders[p][heirs[0].ID]:
			// El heredero YA era objetivo de esta corrida: volver a correr
			// killport no agrega nada. Si se actuó, su destino dice por qué
			// sigue; si solo se miró, es a quién se cerraría.
			if _, acted := idx.fate[heirs[0].ID]; acted {
				res.Reason, res.Advice = busyWhy(idx.fate, heirs[0])
			} else {
				res.Reason = "lo sostiene " + procLabel(heirs[0]) + ", que heredó el socket"
			}
		default:
			res.Advice = "lo sostiene un hijo que heredó el socket, " + procLabel(heirs[0]) + "; volvé a correr killport para cerrarlo"
		}
		return res
	}

	res.Owner, res.OwnerName = owner.ID, owner.Name
	if idx.holders[p][owner.ID] {
		// El mismo de antes sigue ahí. O no se lo tocó a propósito (negativa),
		// o no se pudo: el motivo sale de su destino.
		// La negativa manda, sea del que figura en la tabla o de un proceso
		// que está detrás de él: si en el mismo puerto de http.sys hay dos
		// URL, la de un objetivo y la de un servicio que no se toca, el puerto
		// no se libera cerrando al primero.
		if r := idx.refusalOf(p, []Proc{owner}); r != nil {
			res.Status = PortRefused
			res.Reason, res.Advice = r.Reason, r.Advice
			if r.Owner != owner.ID && r.Name != "" {
				res.Reason = "lo tiene " + r.Name + " (a través de " + displayName(owner) + "): " + r.Reason
			}
			return res
		}
		x, isExt := idx.ext[p]
		v, isVia := idx.via[p]
		res.Status = PortBusy
		switch {
		case isExt && serves(x.target, owner):
			res.Reason, res.Advice = externalBusyWhy(x, owner)
		case isVia && serves(v, owner):
			res.Reason, res.Advice = viaBusyWhy(v, owner, idx.fate)
		default:
			res.Reason, res.Advice = busyWhy(idx.fate, owner)
		}
		return res
	}

	// Dueño nuevo: alguien lo relanzó (respawn). El supervisor es el ancestro
	// común entre la víctima de antes y el nuevo dueño de ahora.
	res.Status = PortRespawned
	prev, hadOwner := idx.rep[p]
	if !hadOwner {
		res.Advice = "lo tomó otro proceso mientras tanto"
		return res
	}
	res.Advice = "algo lo relanzó apenas se liberó"
	if supID, found := CrossLCA(before, prev, after, owner.ID); found {
		res.Supervisor = supID
		if sup, ok := after.Lookup(supID); ok {
			res.SupervisorName = sup.Name
			if strings.EqualFold(sup.Name, "services.exe") {
				res.Advice = "lo relanza un servicio de Windows; pará el servicio dueño en vez de matar el proceso"
			} else {
				res.Advice = "lo relanza " + procLabel(sup) + ": cerrá ese supervisor (por ej. nodemon/pm2) y después el puerto queda libre"
			}
		}
	}
	return res
}

// refusalOf es la negativa del puerto p que cubre a alguno de esos dueños
// (el negado mismo, o el intermediario detrás del que está), o nil. En un
// puerto puede haber varias: el motor de contenedores y el wslrelay de la
// misma publicación, con --no-docker.
func (idx portIndex) refusalOf(p uint16, owners []Proc) *Refusal {
	for _, r := range idx.refusal[p] {
		for _, o := range owners {
			if r.Owner == o.ID || (!r.Via.Zero() && r.Via.Canon() == o.ID) {
				return r
			}
		}
	}
	return nil
}

// busyWhy explica por qué el dueño de antes sigue ocupando el puerto, según
// cómo le fue como víctima. Si no fue víctima (solo se miró: --json sin
// --yes, o no llegó a actuarse), dice quién lo tiene, sin hablar de una
// verificación que no hubo.
func busyWhy(fate map[ID]VictimResult, owner Proc) (reason, advice string) {
	v, ok := fate[owner.ID]
	if !ok {
		return "lo tiene " + procLabel(owner), ""
	}
	switch v.Outcome {
	case OutcomeDenied:
		return "acceso denegado", "hace falta permiso de administrador"
	case OutcomeProtected:
		return "es un proceso protegido (PPL)", "ni un administrador puede cerrarlo: se detiene desde el programa que lo instaló"
	case OutcomeCritical:
		return "está marcado como proceso crítico", "terminarlo colgaría o reiniciaría Windows: killport no lo hace"
	case OutcomePending:
		if is(v.Err, ErrStopPending) {
			return "cancelaste mientras se detenía el servicio: el pedido ya se mandó y sigue deteniéndose", "en unos segundos debería quedar libre: volvé a correr killport para confirmarlo"
		}
		return "cancelaste antes de cerrarlo", ""
	case OutcomeError:
		if v.Err != nil {
			return "no se pudo cerrar: " + v.Err.Error(), ""
		}
		return "no se pudo cerrar", ""
	}
	return "sigue ocupado por el mismo proceso", ""
}

// rootDone dice si la raíz de un objetivo ya se fue (cerró, se la terminó, o
// se detuvo su servicio).
func rootDone(t Target, fate map[ID]VictimResult) bool {
	v, ok := fate[t.Root.Canon()]
	return ok && v.Outcome.Done()
}

// viaBusyWhy explica por qué el intermediario de un proceso de Windows (hoy,
// http.sys) sigue con el puerto, según cómo le fue al proceso.
func viaBusyWhy(t Target, holder Proc, fate map[ID]VictimResult) (reason, advice string) {
	quien := displayName(Proc{ID: t.Root, Name: t.Name})
	if _, acted := fate[t.Root.Canon()]; !acted {
		return "lo tiene " + NameWithPID(t.Name, t.Root.PID) + ", a través de " + displayName(holder), ""
	}
	if rootDone(t, fate) {
		return quien + " cerró, pero " + displayName(holder) + " sigue escuchando: otro proceso tiene registrada una URL en ese puerto",
			"volvé a correr killport: ahora muestra quién queda"
	}
	return busyWhy(fate, Proc{ID: t.Root.Canon(), Name: t.Name})
}

// serves dice si holder es uno de los intermediarios que sostienen sockets de
// ese objetivo (puede haber más de uno: el Via y los demás).
func serves(t Target, holder Proc) bool {
	if t.Via.Canon() == holder.ID {
		return true
	}
	return slices.ContainsFunc(t.Sockets, func(s ports.Socket) bool { return s.PID == holder.PID })
}

// externalBusyWhy explica por qué un intermediario de un externo (holder: el
// que tiene el puerto AHORA) lo sigue sosteniendo, según cómo le fue al
// externo.
func externalBusyWhy(x extPort, holder Proc) (reason, advice string) {
	que := x.target.External.Label()
	if !x.known {
		return "lo tiene " + que + " (a través de " + x.target.ViaName + ")", ""
	}
	e := x.fate
	switch {
	case e.Outcome.Done():
		reason = que + " se detuvo, pero " + displayName(holder) + " no soltó el puerto"
		switch RelayOf(holder) {
		case RelayWSL:
			advice = "si sigue así, el relay quedó colgado: volvé a correr killport, que lo detecta y ofrece cerrarlo"
		case RelayContainer:
			advice = "puede que otro contenedor publique el mismo puerto: `docker ps` lo muestra"
		}
		return reason, advice
	case e.Outcome == OutcomeDenied:
		if e.Err != nil {
			return "acceso denegado", e.Err.Error()
		}
		return "acceso denegado", ""
	case e.Outcome == OutcomePending:
		return "cancelaste antes de detenerlo", ""
	}
	if e.Err != nil {
		return "no se pudo detener " + que + ": " + e.Err.Error(), ""
	}
	return "no se pudo detener " + que, ""
}

// Diagnoser explica los puertos que ninguna tabla atribuye a nadie: libres de
// verdad, reservados por Windows, o tomados por alguien que no figura
// escuchando. Memoiza los rangos excluidos y los dinámicos (leerlos es correr
// netsh ocho veces): se piden una vez y solo si hace falta.
//
// El orden importa y es el contrario del intuitivo: PRIMERO se sondea, después
// se mira la tabla de exclusiones. Una exclusión administrada (la fila con
// "*", la que deja `netsh … add excludedportrange`) solo saca el puerto del
// reparto dinámico: un programa que lo pide por número lo ata sin problema. Es
// justo lo que hace `killport --reserve`, y leer la tabla antes que el sondeo
// llevaba a decirle al usuario "reservado, borrá esa exclusión" sobre el
// arreglo que él mismo había puesto. Lo que la tabla sí decide es CÓMO leer un
// bind que falló: con uso exclusivo un WSAEACCES es ambiguo.
type Diagnoser struct {
	os     OS
	filter ports.Filter
	probe  bool
	socks  []ports.Socket // la última lectura de las tablas (todos los estados)
	procs  *Forest        // para nombrar a quien tiene conexiones sobre el puerto
	excl   ports.Exclusions
	dyn    ports.DynamicRanges
	loaded bool
}

// NewDiagnoser prepara el diagnóstico. probe decide si además de la tabla de
// rangos excluidos se sondea un bind por puerto; sin sondeo (un pedido de
// miles de puertos) una exclusión administrada se informa como libre, con la
// salvedad de que no se confirmó.
func NewDiagnoser(os OS, filter ports.Filter, probe bool) *Diagnoser {
	return &Diagnoser{os: os, filter: filter, probe: probe}
}

// Seen le da al diagnóstico lo que ya se leyó del sistema —las tablas de
// sockets con todos sus estados y el bosque—, para que pueda decir QUIÉN tiene
// un puerto que no se deja atar aunque nadie escuche en él.
func (d *Diagnoser) Seen(socks []ports.Socket, f *Forest) *Diagnoser {
	d.socks, d.procs = socks, f
	return d
}

func (d *Diagnoser) load() {
	if d.loaded {
		return
	}
	d.excl, _ = d.os.Excluded()
	d.dyn, _ = d.os.DynamicRanges()
	d.loaded = true
}

// Port dictamina sobre un puerto sin dueño. Recorre los protocolos que pide
// el filtro, en las dos familias, y se queda con el primer veredicto que no
// sea "libre"; si todos lo son, con la primera salvedad que haya aparecido.
func (d *Diagnoser) Port(p uint16) PortResult {
	d.load()
	free := PortResult{Port: p, Status: PortFree}
	for _, proto := range wantedProtos(d.filter) {
		for _, fam := range [...]ports.Family{ports.IPv4, ports.IPv6} {
			res := d.one(p, proto, fam)
			if res.Status != PortFree {
				return res
			}
			if free.Reason == "" && res.Reason != "" {
				free = res
			}
		}
	}
	return free
}

// one dictamina para un protocolo y una familia.
func (d *Diagnoser) one(p uint16, proto ports.Proto, fam ports.Family) PortResult {
	excl, excluded := d.excl.Find(proto, fam, p)
	bind := ports.BindError // sin sondeo, no se sabe
	if d.probe {
		bind = d.os.ProbeBind(proto, fam, p)
	}
	free := PortResult{Port: p, Status: PortFree}

	switch bind {
	case ports.BindFree:
		// Se pudo atar: está libre, diga lo que diga la tabla (que se leyó en
		// otro instante).
		if excluded && excl.Administered {
			free.Reserved = excl
			free.Reason = "cae en la exclusión administrada " + excl.Range.String() + ": Windows no lo reparte solo, pero un programa que lo pide lo puede usar"
		}
		return free

	case ports.BindForbidden:
		switch {
		case excluded && !excl.Administered:
			return d.reserved(p, proto, fam, excl)
		case excluded:
			return PortResult{Port: p, Status: PortReserved, Proto: proto, Reserved: excl,
				Reason: "cae en la exclusión administrada " + excl.Range.String() + " y aun así Windows niega el bind: es la reserva de otro programa, o alguien lo tiene tomado sin figurar en las tablas",
				Advice: "usá otro puerto; no borres una exclusión que no pusiste vos"}
		}
		if note, ok := d.connections(p, proto, fam); ok {
			free.Reason, free.Dynamic, free.Remedies = note.reason, note.dynamic, note.remedies
			return free
		}
		return PortResult{Port: p, Status: PortReserved, Proto: proto,
			Reason: "Windows niega el bind (acceso denegado) y el puerto no figura en los rangos excluidos",
			Advice: "lo puede tener otro usuario o un programa que no lo comparte: `netstat -anoq` muestra los sockets atados que no escuchan"}

	case ports.BindInUse:
		if note, ok := d.connections(p, proto, fam); ok {
			free.Reason, free.Dynamic, free.Remedies = note.reason, note.dynamic, note.remedies
			return free
		}
		return PortResult{Port: p, Status: PortBusy, Proto: proto,
			Reason: "nadie figura escuchando, pero alguien lo tiene atado (bind)",
			Advice: "`netstat -anoq` muestra los sockets atados que no escuchan (BOUND), con su PID"}
	}

	// Sin sondeo (o el sondeo no pudo decir nada): queda la tabla.
	switch {
	case excluded && !excl.Administered:
		return d.reserved(p, proto, fam, excl)
	case excluded:
		free.Reserved = excl
		free.Reason = "cae en la exclusión administrada " + excl.Range.String() + ": Windows no lo reparte solo (no sondeé si se puede usar)"
	}
	return free
}

// reserved arma el veredicto de un puerto que cae en una exclusión sin "*",
// según de quién es (ports.ExcludedRange.Origin). Solo un bloque de WinNAT
// lleva las salidas de WinNAT: reservar el puerto (que detiene WinNAT) o
// reiniciarlo no le hacen nada a lo que apartó http.sys o un programa, y
// ofrecerlo sería cortarles la red a WSL, Docker y Hyper-V para nada.
func (d *Diagnoser) reserved(p uint16, proto ports.Proto, fam ports.Family, excl ports.ExcludedRange) PortResult {
	switch excl.Origin() {
	case ports.OriginWinNAT:
		return d.winnat(p, proto, fam, excl)
	case ports.OriginHTTP:
		return httpReserved(p, proto, excl)
	}
	return programReserved(p, proto, excl)
}

// httpReserved es el puerto que atiende http.sys: Windows lo aparta mientras
// lo atiende y lo suelta solo. No hay nada que reservar ni que reiniciar.
func httpReserved(p uint16, proto ports.Proto, excl ports.ExcludedRange) PortResult {
	h := excl.HTTP
	reason := "cae en la exclusión " + excl.Range.String() + ", que Windows pone mientras http.sys atiende ese puerto"
	if h.What != "" {
		reason += ": es " + h.What
	}
	var detail []string
	if h.URL != "" {
		detail = append(detail, h.URL)
	}
	switch {
	case h.Service != "":
		detail = append(detail, "servicio "+h.Service)
	case h.Account != "":
		detail = append(detail, "reservada a "+h.Account)
	}
	if len(detail) > 0 {
		reason += " (" + strings.Join(detail, ", ") + ")"
	}
	return PortResult{Port: p, Status: PortReserved, Proto: proto, Reserved: excl, Reason: reason,
		Advice: "no es de WinNAT: ni reservarlo ni reiniciar WinNAT la sacan. Windows la quita sola cuando http.sys deja de atender el puerto; sin tocar nada, usá otro"}
}

// programReserved es la reserva que pidió un programa mientras corre: ni de
// WinNAT (no es un bloque de 100) ni de un administrador (no tiene "*"), y
// Windows no dice de quién es.
func programReserved(p uint16, proto ports.Proto, excl ports.ExcludedRange) PortResult {
	return PortResult{Port: p, Status: PortReserved, Proto: proto, Reserved: excl,
		Reason: "cae en la exclusión " + excl.Range.String() + ", que no es de WinNAT (sus bloques son de 100 puertos) ni la agregó un administrador: la tiene reservada un programa mientras corre, y Windows no dice cuál",
		Advice: "no hay proceso a la vista que cerrar: la reserva se va cuando ese programa la suelta o termina; ni reservarlo ni reiniciar WinNAT la sacan"}
}

// winnat arma el veredicto de un puerto que cae en un bloque de WinNAT, con
// las salidas que hay, de la más duradera a la más barata.
func (d *Diagnoser) winnat(p uint16, proto ports.Proto, fam ports.Family, excl ports.ExcludedRange) PortResult {
	res := PortResult{Port: p, Status: PortReserved, Proto: proto, Reserved: excl,
		Reason: "cae en el bloque " + excl.Range.String() + " que WinNAT reservó para Hyper-V, WSL o Docker (los elige de nuevo en cada arranque)",
		Advice: "no hay proceso que cerrar"}
	// La reserva es del protocolo tapado: reservar el otro dejaría el puerto
	// tapado igual.
	res.Remedies = append(res.Remedies, Remedy{
		Kind: RemedyReserve, Port: p, Proto: proto,
		Effect: "lo reserva para vos: WinNAT no lo vuelve a tomar (permanente; pide administrador)",
	})
	if dyn, low := d.lowDynamic(proto, fam, p, excl.Range); low {
		res.Dynamic = dyn
		res.Reason += "; tu rango dinámico " + proto.String() + " es " + dyn.Range().String() + " (el de fábrica es 49152–65535): por eso reserva entre tus puertos de desarrollo"
		res.Remedies = append(res.Remedies, fixDynamic)
	}
	// Detener y arrancar son UNA salida con dos pasos: quien ejecute solo el
	// primero deja sin red a WSL, Docker y Hyper-V.
	res.Remedies = append(res.Remedies, Remedy{
		Kind:   RemedySteps,
		Steps:  []string{"net stop winnat", "net start winnat"},
		Effect: "como administrador: lo libera ahora; en el próximo arranque WinNAT vuelve a elegir sus bloques",
	})
	return res
}

// fixDynamic es la salida para un rango dinámico corrido.
var fixDynamic = Remedy{
	Kind:   RemedyFixDynamic,
	Effect: "devuelve el rango dinámico a 49152–65535: Windows deja de repartir entre los puertos de desarrollo (pide administrador)",
}

// lowDynamic dice si el rango dinámico de ese protocolo y familia arranca
// antes del 49152 Y eso explica lo que pasa con p: lo contiene, o se cruza con
// el bloque que lo tapa.
func (d *Diagnoser) lowDynamic(proto ports.Proto, fam ports.Family, p uint16, block ports.Range) (ports.DynamicRange, bool) {
	dyn, ok := d.dyn.Find(proto, fam)
	if !ok || !dyn.Low() {
		return ports.DynamicRange{}, false
	}
	r := dyn.Range()
	touches := r.Contains(p)
	if block != (ports.Range{}) {
		touches = touches || (block.Lo <= r.Hi && r.Lo <= block.Hi)
	}
	return dyn, touches
}

// caveat es lo que hay que saber de un puerto que está libre para un
// servidor común pero no se dejó atar con uso exclusivo.
type caveat struct {
	reason   string
	dynamic  ports.DynamicRange
	remedies []Remedy
}

// connections explica un bind exclusivo que falló por CONEXIONES sobre ese
// puerto local, que es lo único de la tabla que no cuenta como "ocupar el
// puerto" (nadie escucha): los restos de conexiones que se están cerrando, o
// una conexión viva que lo usa de puerto de salida. Un servidor común lo ata
// igual —una dirección comodín y una específica no chocan—; solo falla el que
// pide uso exclusivo. ok=false si en la tabla no hay nada sobre ese puerto.
func (d *Diagnoser) connections(p uint16, proto ports.Proto, fam ports.Family) (caveat, bool) {
	var owners []string
	seen := map[uint32]bool{}
	remnants := 0
	for _, s := range d.socks {
		if s.Proto != proto || s.Port() != p || s.Proto == ports.UDP || s.State == ports.Listen {
			continue
		}
		if s.PID == 0 {
			remnants++
			continue
		}
		if seen[s.PID] {
			continue
		}
		seen[s.PID] = true
		name := NameWithPID("", s.PID)
		if d.procs != nil {
			if o, ok := d.procs.LivePID(s.PID); ok {
				name = NameWithPID(o.Name, s.PID)
			}
		}
		owners = append(owners, name)
	}
	switch {
	case len(owners) > 0:
		n := caveat{reason: "nadie escucha, pero " + strings.Join(owners, ", ") + " lo usa como puerto local de una conexión: un servidor común lo puede tomar igual; uno que pide uso exclusivo, no"}
		if dyn, low := d.lowDynamic(proto, fam, p, ports.Range{}); low {
			n.dynamic = dyn
			n.reason += ". Tu rango dinámico " + proto.String() + " es " + dyn.Range().String() + " (el de fábrica es 49152–65535): Windows reparte ese puerto como puerto de salida"
			n.remedies = []Remedy{fixDynamic}
		}
		return n, true
	case remnants > 0:
		return caveat{reason: "quedan conexiones cerrándose sobre ese puerto (TIME_WAIT): un servidor común ya lo puede tomar; uno que pide uso exclusivo, en unos segundos"}, true
	}
	return caveat{}, false
}

// --- Helpers ---------------------------------------------------------------

// backoff genera 10, 20, 40, 80, 160, 200, 200… ms hasta cubrir cap. Corto y
// creciente: el kernel suele liberar el puerto en el primer paso, y si tarda
// (un TIME_WAIT o un servicio que reinicia) no gastamos CPU sondeando.
func backoff(cap time.Duration) []time.Duration {
	var out []time.Duration
	var total time.Duration
	d := 10 * time.Millisecond
	for total < cap {
		out = append(out, d)
		total += d
		if d < 200*time.Millisecond {
			d *= 2
			if d > 200*time.Millisecond {
				d = 200 * time.Millisecond
			}
		}
	}
	return out
}

func wantedProtos(f ports.Filter) []ports.Proto {
	switch {
	case f.TCP && !f.UDP:
		return []ports.Proto{ports.TCP}
	case f.UDP && !f.TCP:
		return []ports.Proto{ports.UDP}
	}
	return []ports.Proto{ports.TCP, ports.UDP}
}

func consoleKey(pids []uint32) string {
	cp := slices.Clone(pids)
	slices.Sort(cp)
	return fmt.Sprint(cp)
}

func consoleHas(pids []uint32, pid uint32) bool { return slices.Contains(pids, pid) }

func sharesOwnConsole(pids []uint32, own map[uint32]bool) bool {
	for _, p := range pids {
		if own[p] {
			return true
		}
	}
	return false
}

// is es errors.Is con un nombre corto (se usa en cada rama de la escalada).
func is(err, target error) bool { return errors.Is(err, target) }

// asShared extrae un *SharedConsoleError si el error lo es.
func asShared(err error, dst **SharedConsoleError) bool { return errors.As(err, dst) }

// ctxCanceled dice si el contexto ya se canceló (Ctrl+C del usuario) sin bloquear.
func ctxCanceled(ctx context.Context) bool { return ctx.Err() != nil }
