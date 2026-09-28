package reaper

import (
	"context"
	"net/netip"
	"slices"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// simOS es un SIMULADOR DE EVENTOS DISCRETOS: un sistema operativo falso con
// reloj VIRTUAL y procesos de comportamiento guionado. No hay hilos ni reloj
// real: el tiempo solo avanza cuando el motor pide una espera (Wait/Sleep/
// StopService), y avanza EXACTAMENTE hasta el próximo evento programado. Así
// los tests verifican secuencias de eventos, resultados y tiempos virtuales al
// milisegundo, sin flakiness.
type simOS struct {
	now      time.Time
	procs    map[ID]*simProc
	svc      map[string]*simSvc
	sockets  []ports.Socket              // tabla actual (los dueños que salieron se filtran)
	afterFn  func(*simOS) []ports.Socket // override de la tabla para la verificación
	snapFn   func(*simOS) []Proc         // override de la instantánea (respawn)
	excluded ports.Exclusions
	dynamic  ports.DynamicRanges
	binds    map[uint16]ports.BindResult
	bindsBy  map[bindKey]ports.BindResult // por protocolo y familia; le gana a binds
	critical map[ID]bool                  // procesos marcados críticos: Open devuelve ErrCritical
	cancelAt time.Time                    // Ctrl+C del usuario (virtual); cero = nunca
	cancelFn context.CancelFunc
	canceled bool

	ctrlCAllow [][]ID // el conjunto permitido de cada Ctrl+C pedido (para verificarlo)
	sleeps     int    // cuántas veces se durmió (el backoff de la verificación)
	exclReads  int    // cuántas veces se leyeron los rangos excluidos (netsh)
	dynReads   int    // cuántas veces se leyó el rango dinámico (netsh)
	probes     int    // cuántos sondeos de bind
	skewWait   bool   // Wait devuelve la identidad en zona local (no canónica)
	// ctrlCCost es lo que tarda el ayudante del Ctrl+C en volver: el Ctrl+C
	// llega al principio, y el reloj avanza esto antes de que CtrlC vuelva.
	ctrlCCost time.Duration

	ext      map[string]*simExt // objetivos externos guionados, por Key
	extCalls []simExtCall       // cada StopExternal pedido (para verificar plazo y --force)
}

// bindKey es un sondeo de bind: protocolo, familia y puerto.
type bindKey struct {
	proto ports.Proto
	fam   ports.Family
	port  uint16
}

// simExt es un objetivo externo guionado (un contenedor, un proceso Linux) y
// el intermediario que publica sus puertos.
type simExt struct {
	closeAfter time.Duration // cede prolijo tras esto (si cae dentro del plazo)
	deaf       bool          // no cede: el dominio lo fuerza al vencer el plazo
	method     Method        // con qué cede prolijo (por defecto MethodEngine)
	result     ExternalStop  // si Outcome != Pending: veredicto fijo, al instante (Gone, Denied, Error)
	relayPID   uint32        // quién sostiene sus sockets en la tabla
	ports      []uint16      // qué puertos publica a través del intermediario
	release    time.Duration // cuánto tarda el intermediario en soltarlos tras detenerlo
	never      bool          // el intermediario no los suelta nunca (relay zombi)
	// otros: más intermediarios que sirven al mismo externo (Docker Desktop
	// publica también con wslrelay en [::1]), cada uno con su demora.
	otros []simRelay

	stoppedAt time.Time
	stopped   bool
}

// simRelay es un intermediario adicional de un externo.
type simRelay struct {
	pid     uint32
	release time.Duration
	never   bool
}

type simExtCall struct {
	keys     []string
	deadline time.Time
	force    bool
}

// skewHandle imita un adaptador que entrega la identidad en una representación
// no canónica del mismo instante (otra zona horaria).
type skewHandle struct{ Handle }

func (h skewHandle) ID() ID {
	id := h.Handle.ID()
	return ID{PID: id.PID, Created: id.Created.In(time.FixedZone("SKEW", 5*3600))}
}

// simProc es un proceso guionado.
type simProc struct {
	id           ID
	name         string
	parent       uint32
	session      uint32
	path         string
	console      []uint32      // PID de la consola; vacío = sin consola
	windows      int           // ventanas de nivel superior
	closeAfter   time.Duration // tarda esto en salir tras un WM_CLOSE que lo afecta
	ctrlCReacts  bool          // reacciona al Ctrl+C (un shell, p. ej., no)
	ctrlCAfter   time.Duration // tarda esto en salir tras el Ctrl+C
	openErr      error         // ErrGone/ErrRecycled/ErrAccessDenied/ErrProtected al abrir
	noTerminate  bool          // el handle se abre SIN permiso de terminar
	terminateErr error         // Terminate devuelve esto (p. ej. acceso denegado)

	exited    bool
	exitAt    time.Time
	scheduled bool
}

func (p *simProc) schedule(t time.Time) {
	if p.exited {
		return
	}
	if !p.scheduled || t.Before(p.exitAt) {
		p.exitAt, p.scheduled = t, true
	}
}

type simSvc struct {
	name     string
	ownerPID uint32
	stopFor  time.Duration
	err      error // ErrServiceAccessDenied, *ServiceDependentsError…
}

func newSim(t0 time.Time) *simOS {
	return &simOS{now: t0, procs: map[ID]*simProc{}, svc: map[string]*simSvc{}, binds: map[uint16]ports.BindResult{},
		bindsBy: map[bindKey]ports.BindResult{}, critical: map[ID]bool{}, ext: map[string]*simExt{}}
}

func (s *simOS) add(p *simProc) *simProc { s.procs[p.id] = p; return p }

func (s *simOS) byPID(pid uint32) *simProc {
	var newest *simProc
	for _, p := range s.procs {
		if p.PID() == pid && !p.exited && (newest == nil || newest.id.Created.Before(p.id.Created)) {
			newest = p
		}
	}
	return newest
}

func (p *simProc) PID() uint32 { return p.id.PID }

func (s *simOS) advance(t time.Time) {
	if t.After(s.now) {
		s.now = t
	}
}

func (s *simOS) liveProcs() []Proc {
	var out []Proc
	for _, p := range s.procs {
		if p.exited {
			continue
		}
		out = append(out, Proc{ID: p.id, Parent: p.parent, Name: p.name, Path: p.path, SessionID: p.session})
	}
	return out
}

// --- OS ---------------------------------------------------------------------

func (s *simOS) Now() time.Time { return s.now }

func (s *simOS) Sleep(ctx context.Context, d time.Duration) bool {
	s.sleeps++
	target := s.now.Add(d)
	if !s.cancelAt.IsZero() && !s.cancelAt.After(target) {
		s.advance(s.cancelAt)
		s.fireCancel()
		return false
	}
	s.advance(target)
	return ctx.Err() == nil
}

func (s *simOS) fireCancel() {
	if !s.canceled {
		s.canceled = true
		if s.cancelFn != nil {
			s.cancelFn()
		}
	}
}

func (s *simOS) Sockets(ctx context.Context) ([]ports.Socket, error) {
	if s.afterFn != nil {
		return s.afterFn(s), nil
	}
	var out []ports.Socket
	for _, sk := range s.sockets {
		if p := s.procForSocket(sk); p != nil && p.exited {
			continue // el dueño salió: el socket se fue con él
		}
		if s.releasedByExt(sk) {
			continue // el intermediario ya lo soltó: su externo se detuvo
		}
		out = append(out, sk)
	}
	return out, nil
}

func (s *simOS) procForSocket(sk ports.Socket) *simProc {
	for _, p := range s.procs {
		if p.PID() == sk.PID {
			return p
		}
	}
	return nil
}

func (s *simOS) Snapshot(ctx context.Context) ([]Proc, error) {
	if s.snapFn != nil {
		return s.snapFn(s), nil
	}
	return s.liveProcs(), nil
}

func (s *simOS) Detail(id ID) (Detail, error) {
	if p := s.procs[id]; p != nil && !p.exited {
		return Detail{}, nil
	}
	return Detail{}, ErrGone
}

func (s *simOS) Services() (map[uint32][]Service, error) {
	m := map[uint32][]Service{}
	for _, sv := range s.svc {
		m[sv.ownerPID] = append(m[sv.ownerPID], Service{Name: sv.name, Display: sv.name})
	}
	return m, nil
}

func (s *simOS) Excluded() (ports.Exclusions, error) { s.exclReads++; return s.excluded, nil }

func (s *simOS) DynamicRanges() (ports.DynamicRanges, error) { s.dynReads++; return s.dynamic, nil }

func (s *simOS) Open(id ID) (Handle, error) {
	p := s.procs[id]
	if p == nil {
		return nil, ErrGone
	}
	if p.openErr != nil { // se comprueba antes que exited: un PID reciclado
		return nil, p.openErr // "existe" pero con otra identidad
	}
	if p.exited {
		return nil, ErrGone
	}
	if s.critical[p.id] {
		return nil, ErrCritical
	}
	return &simHandle{s: s, p: p}, nil
}

func (s *simOS) Wait(ctx context.Context, hs []Handle, deadline time.Time) (Handle, ExitInfo, WaitOutcome) {
	// Próxima salida programada entre los handles dados.
	var best *simProc
	for _, h := range hs {
		p := h.(*simHandle).p
		if p.exited || !p.scheduled {
			continue
		}
		if best == nil || p.exitAt.Before(best.exitAt) {
			best = p
		}
	}
	// El primero de: cancelación, salida, vencimiento.
	cancelSet := !s.cancelAt.IsZero() && !s.canceled
	if cancelSet && !s.cancelAt.After(deadline) && (best == nil || !s.cancelAt.After(best.exitAt)) {
		s.advance(s.cancelAt)
		s.fireCancel()
		return nil, ExitInfo{}, WaitCanceled
	}
	if best != nil && !best.exitAt.After(deadline) {
		s.advance(best.exitAt)
		best.exited = true
		info := ExitInfo{At: best.exitAt} // la hora de salida que da el kernel
		if s.skewWait {
			// Simula un adaptador que devuelve la identidad en OTRA
			// representación del mismo instante (zona local, como el
			// FILETIME→time.Unix de win). El motor la tiene que reencontrar.
			return skewHandle{&simHandle{s: s, p: best}}, info, WaitExited
		}
		return &simHandle{s: s, p: best}, info, WaitExited
	}
	s.advance(deadline)
	return nil, ExitInfo{}, WaitDeadline
}

func (s *simOS) CloseWindows(id ID) (int, error) {
	p := s.procs[id]
	if p == nil || p.exited {
		return 0, ErrGone
	}
	if p.windows > 0 {
		// una ventana de nivel superior responde al WM_CLOSE cerrándose
		p.schedule(s.now.Add(p.closeAfter))
	}
	return p.windows, nil
}

func (s *simOS) ConsolePIDs(id ID) ([]uint32, error) {
	p := s.procs[id]
	if p == nil || len(p.console) == 0 {
		return nil, ErrNoConsole
	}
	return p.console, nil
}

func (s *simOS) CtrlC(id ID, allow []ID) error {
	p := s.procs[id]
	if p == nil || len(p.console) == 0 {
		return ErrNoConsole
	}
	// Igual que el ayudante real: un miembro es permitido si su IDENTIDAD
	// actual (el proceso vivo con ese PID) está en allow; un PID reciclado
	// desde la foto es un intruso aunque el número coincida.
	allowSet := map[ID]bool{}
	for _, a := range allow {
		allowSet[a] = true
	}
	var intruders []uint32
	for _, pid := range p.console {
		q := s.byPID(pid)
		if q == nil || !allowSet[q.id] {
			intruders = append(intruders, pid)
		}
	}
	s.ctrlCAllow = append(s.ctrlCAllow, allow)
	if len(intruders) > 0 {
		return &SharedConsoleError{Intruders: intruders}
	}
	for _, pid := range p.console {
		if q := s.byPID(pid); q != nil && q.ctrlCReacts {
			q.schedule(s.now.Add(q.ctrlCAfter))
		}
	}
	s.advance(s.now.Add(s.ctrlCCost))
	return nil
}

func (s *simOS) StopService(ctx context.Context, name string, deadline time.Time) error {
	sv := s.svc[name]
	if sv == nil {
		return ErrGone
	}
	if sv.err != nil {
		return sv.err
	}
	t := s.now.Add(sv.stopFor)
	if t.After(deadline) {
		s.advance(deadline)
		return ErrServiceTimeout
	}
	s.advance(t)
	if q := s.byPID(sv.ownerPID); q != nil {
		q.exited = true // el proceso del servicio muere al detenerse
	}
	return nil
}

func (s *simOS) ProbeBind(proto ports.Proto, fam ports.Family, port uint16) ports.BindResult {
	s.probes++
	if r, ok := s.bindsBy[bindKey{proto, fam, port}]; ok {
		return r
	}
	if r, ok := s.binds[port]; ok {
		return r
	}
	return ports.BindFree
}

// releasedByExt dice si el intermediario ya soltó ese socket porque el
// externo que lo publicaba se detuvo (y pasó su demora en soltarlo).
func (s *simOS) releasedByExt(sk ports.Socket) bool {
	for _, x := range s.ext {
		if !x.stopped || !slices.Contains(x.ports, sk.Port()) {
			continue
		}
		relays := append([]simRelay{{x.relayPID, x.release, x.never}}, x.otros...)
		for _, r := range relays {
			if r.pid == sk.PID && !r.never && !s.now.Before(x.stoppedAt.Add(r.release)) {
				return true
			}
		}
	}
	return false
}

// StopExternal resuelve los externos en orden de tiempo virtual: cada uno
// termina cuando cede (closeAfter), cuando su dominio lo fuerza (el plazo), o
// al instante con un veredicto fijo. La cancelación del usuario corta en su
// instante, dejando a los que faltan sin veredicto.
func (s *simOS) StopExternal(ctx context.Context, xs []External, deadline time.Time, force bool, done func(int, ExternalStop)) {
	call := simExtCall{deadline: deadline, force: force}
	type fin struct {
		i  int
		at time.Time
		r  ExternalStop
	}
	var fins []fin
	for i, x := range xs {
		call.keys = append(call.keys, x.Key)
		se := s.ext[x.Key]
		switch {
		case se == nil:
			fins = append(fins, fin{i, s.now, ExternalStop{Outcome: OutcomeGone}})
		case se.result.Outcome != OutcomePending:
			fins = append(fins, fin{i, s.now, se.result})
		case force:
			fins = append(fins, fin{i, s.now, ExternalStop{Outcome: OutcomeTerminated, Method: MethodTerminate}})
		case !se.deaf && !s.now.Add(se.closeAfter).After(deadline):
			m := se.method
			if m == MethodNone {
				m = MethodEngine
			}
			fins = append(fins, fin{i, s.now.Add(se.closeAfter), ExternalStop{Outcome: OutcomeClosed, Method: m}})
		default:
			fins = append(fins, fin{i, deadline, ExternalStop{Outcome: OutcomeTerminated, Method: MethodTerminate}})
		}
	}
	s.extCalls = append(s.extCalls, call)
	slices.SortStableFunc(fins, func(a, b fin) int { return a.at.Compare(b.at) })
	for _, f := range fins {
		if !s.cancelAt.IsZero() && !s.canceled && s.cancelAt.Before(f.at) {
			s.advance(s.cancelAt)
			s.fireCancel()
			return
		}
		s.advance(f.at)
		if se := s.ext[xs[f.i].Key]; se != nil && f.r.Outcome.Done() {
			se.stopped, se.stoppedAt = true, s.now
		}
		done(f.i, f.r)
	}
}

// simHandle es un handle del simulador.
type simHandle struct {
	s *simOS
	p *simProc
}

func (h *simHandle) ID() ID             { return h.p.id }
func (h *simHandle) CanTerminate() bool { return !h.p.noTerminate }
func (h *simHandle) Terminate() error {
	if h.p.exited {
		return ErrGone
	}
	if h.p.terminateErr != nil {
		return h.p.terminateErr
	}
	h.p.schedule(h.s.now) // terminación inmediata
	return nil
}
func (h *simHandle) Close() {}

// --- helpers de armado ------------------------------------------------------

func tcpListen(port uint16, pid uint32, since time.Time) ports.Socket {
	return ports.Socket{Proto: ports.TCP, Local: netip.AddrPortFrom(netip.MustParseAddr("0.0.0.0"), port), State: ports.Listen, PID: pid, Since: since}
}

// recorder junta los eventos que emite el motor, para verificar la secuencia.
type recorder struct{ evs []Event }

func (r *recorder) emit(e Event) { r.evs = append(r.evs, e) }

func (r *recorder) kinds() []EventKind {
	out := make([]EventKind, len(r.evs))
	for i, e := range r.evs {
		out[i] = e.Kind
	}
	return out
}

func (r *recorder) first(k EventKind) (Event, bool) {
	for _, e := range r.evs {
		if e.Kind == k {
			return e, true
		}
	}
	return Event{}, false
}

func (r *recorder) has(k EventKind) bool { _, ok := r.first(k); return ok }
