package reaper

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// La espera (--wait / --wait-free): la máquina de estados con horas escritas a
// mano, y el bucle contra un RELOJ FALSO con una historia guionada de la
// tabla y de los procesos.

// escucha arma un socket TCP que escucha, creado en el instante since.
func escucha(local string, pid uint32, since time.Time) ports.Socket {
	return ports.Socket{Proto: ports.TCP, Local: netip.MustParseAddrPort(local), State: ports.Listen, PID: pid, Since: since}
}

func pedidoW(t *testing.T, args ...string) ports.Request {
	t.Helper()
	q, err := ports.ParseRequest(args)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func estados(w *Watch) string {
	var out []string
	for _, p := range w.Ports() {
		out = append(out, p.State.String())
	}
	return strings.Join(out, ",")
}

// --- Watch, pura ---------------------------------------------------------------

func TestWatchEscucharSimple(t *testing.T) {
	w := NewWatch(WatchSpec{For: WatchListen, Request: pedidoW(t, "5173"), Filter: ports.Filter{TCP: true}})
	if evs := w.Observe(at(0), nil); len(evs) != 1 || !evs[0].Initial || evs[0].Port.State != WatchPending || w.Done() {
		t.Fatalf("sin nadie: %+v done=%v", evs, w.Done())
	}
	// Una conexión con ese puerto local no es alguien que escucha, ni un UDP
	// (se pidió TCP), ni un TCP que escucha en otro puerto.
	ruido := []ports.Socket{
		conn("127.0.0.1:5173", 9, ports.Established),
		{Proto: ports.UDP, Local: netip.MustParseAddrPort("0.0.0.0:5173"), PID: 8, Since: at(1)},
		escucha("0.0.0.0:5174", 7, at(1)),
	}
	if evs := w.Observe(at(50), ruido); len(evs) != 0 || w.Done() {
		t.Fatalf("con ruido: %+v", evs)
	}
	s := escucha("[::1]:5173", 812, at(80))
	evs := w.Observe(at(100), append(ruido, s))
	if len(evs) != 1 || evs[0].Port.State != WatchMet || evs[0].Prev != WatchPending || evs[0].Initial || !w.Done() {
		t.Fatalf("al aparecer: %+v done=%v", evs, w.Done())
	}
	p := w.Ports()[0]
	if len(p.Witness) != 1 || p.Witness[0].PID != 812 || !p.MetAt.Equal(at(100)) || !p.Since.Equal(at(80)) {
		t.Fatalf("testigo: %+v", p)
	}
}

// --stable: la hora de creación del socket es la evidencia; un reinicio es
// otro socket y la cuenta vuelve a cero.
func TestWatchEstableConReinicios(t *testing.T) {
	w := NewWatch(WatchSpec{For: WatchListen, Request: pedidoW(t, "5173"), Filter: ports.Filter{TCP: true}, Stable: 750 * time.Millisecond})
	s1 := escucha("0.0.0.0:5173", 100, at(1000))
	w.Observe(at(1010), []ports.Socket{s1})
	if estados(w) != "sosteniéndose" || !w.Next().Equal(at(1750)) {
		t.Fatalf("recién nacido: %s next=%v", estados(w), w.Next())
	}
	// nodemon: el hijo se va y vuelve con otro PID y otro socket.
	s2 := escucha("0.0.0.0:5173", 101, at(1500))
	evs := w.Observe(at(1600), []ports.Socket{s2})
	if len(evs) != 1 || !evs[0].Restart || estados(w) != "sosteniéndose" || !w.Next().Equal(at(2250)) {
		t.Fatalf("reinicio: %+v %s next=%v", evs, estados(w), w.Next())
	}
	// Un hueco (nadie escucha) también corta.
	w.Observe(at(1700), nil)
	if estados(w) != "pendiente" || !w.Next().IsZero() {
		t.Fatalf("hueco: %s", estados(w))
	}
	s3 := escucha("0.0.0.0:5173", 102, at(1800))
	w.Observe(at(1810), []ports.Socket{s3})
	w.Observe(at(2549), []ports.Socket{s3})
	if w.Done() {
		t.Fatal("se dio por estable antes de tiempo")
	}
	w.Observe(at(2550), []ports.Socket{s3})
	p := w.Ports()[0]
	if !w.Done() || p.Changes != 2 || !p.MetAt.Equal(at(2550)) {
		t.Fatalf("estable: done=%v %+v", w.Done(), p)
	}
}

// Un servidor que ya escuchaba hace rato ya está estable: su socket lo prueba.
func TestWatchYaEscuchabaYEstable(t *testing.T) {
	w := NewWatch(WatchSpec{For: WatchListen, Request: pedidoW(t, "5173"), Stable: time.Second})
	evs := w.Observe(at(60000), []ports.Socket{escucha("0.0.0.0:5173", 1, at(1000))})
	if !w.Done() || len(evs) != 1 || !evs[0].Initial || !w.Ports()[0].Already {
		t.Fatalf("ya escuchaba: %+v", evs)
	}
	// Un reinicio, o un corte, ya no es "ya estaba".
	w.Observe(at(60100), []ports.Socket{escucha("0.0.0.0:5173", 2, at(60050))})
	if w.Ports()[0].Already {
		t.Fatal("se reinició: ya no es el de antes")
	}
	// Sin hora conocida (o una hora que el reloj no alcanzó), cuenta desde
	// la primera lectura que lo vio.
	for _, since := range []time.Time{{}, at(99999)} {
		w := NewWatch(WatchSpec{For: WatchListen, Request: pedidoW(t, "5173"), Stable: time.Second})
		s := escucha("0.0.0.0:5173", 1, since)
		w.Observe(at(60000), []ports.Socket{s})
		w.Observe(at(60999), []ports.Socket{s})
		if w.Done() {
			t.Fatalf("since=%v: estable sin evidencia", since)
		}
		w.Observe(at(61000), []ports.Socket{s})
		if !w.Done() {
			t.Fatalf("since=%v: no se estabilizó", since)
		}
		// Se estaba sosteniendo en la primera lectura: se cumplió después, no
		// "ya estaba".
		if w.Ports()[0].Already {
			t.Fatalf("since=%v: se cumplió a los 1000 ms y figura como que ya estaba", since)
		}
	}
}

// --new: el servidor viejo no cuenta, ni uno sin hora.
func TestWatchSoloNuevos(t *testing.T) {
	w := NewWatch(WatchSpec{For: WatchListen, Request: pedidoW(t, "5173"), NewSince: at(5000)})
	viejo := escucha("0.0.0.0:5173", 1, at(1000))
	sinHora := escucha("[::]:5173", 2, time.Time{})
	w.Observe(at(6000), []ports.Socket{viejo, sinHora})
	if w.Done() {
		t.Fatal("el viejo no cuenta")
	}
	nuevo := escucha("127.0.0.1:5173", 3, at(6100))
	w.Observe(at(6200), []ports.Socket{viejo, sinHora, nuevo})
	if p := w.Ports()[0]; !w.Done() || len(p.Witness) != 1 || p.Witness[0].PID != 3 {
		t.Fatalf("el nuevo: %+v", p)
	}
}

// UDP: un servidor atado a ese puerto cuenta; un efímero (un cliente al que
// el sistema le tocó ese número), no.
func TestWatchUDP(t *testing.T) {
	w := NewWatch(WatchSpec{For: WatchListen, Request: pedidoW(t, "5353"), Filter: ports.Filter{UDP: true}})
	ef := ports.Socket{Proto: ports.UDP, Local: netip.MustParseAddrPort("0.0.0.0:5353"), PID: 4, Since: at(1), Ephemeral: true}
	w.Observe(at(10), []ports.Socket{ef})
	if w.Done() {
		t.Fatal("un efímero no escucha")
	}
	srv := ports.Socket{Proto: ports.UDP, Local: netip.MustParseAddrPort("0.0.0.0:5353"), PID: 5, Since: at(2)}
	w.Observe(at(20), []ports.Socket{ef, srv})
	if !w.Done() || w.Ports()[0].Already {
		t.Fatal("el servidor UDP escucha (y no desde la primera lectura)")
	}
}

// Con dirección: localhost:5173 espera al que contesta localhost.
func TestWatchConDireccion(t *testing.T) {
	// Solo un v6only en [::]: 127.0.0.1 no llega; localhost sí (por ::1).
	v6 := escucha("[::]:5173", 1, at(1))
	w := NewWatch(WatchSpec{For: WatchListen, Request: pedidoW(t, "127.0.0.1:5173")})
	w.Observe(at(10), []ports.Socket{v6})
	if w.Done() {
		t.Fatal("127.0.0.1 no llega a un [::] solo IPv6")
	}
	w = NewWatch(WatchSpec{For: WatchListen, Request: pedidoW(t, "localhost:5173")})
	w.Observe(at(10), []ports.Socket{v6})
	if !w.Done() {
		t.Fatal("localhost llega por ::1")
	}
	// Dos procesos: localhost llega al de [::1], y ese es el testigo.
	red := escucha("0.0.0.0:5173", 2, at(1))
	loc := escucha("[::1]:5173", 3, at(2))
	w = NewWatch(WatchSpec{For: WatchListen, Request: pedidoW(t, "localhost:5173")})
	w.Observe(at(10), []ports.Socket{red, loc})
	if p := w.Ports()[0]; len(p.Witness) != 1 || p.Witness[0].PID != 3 {
		t.Fatalf("testigo de localhost: %+v", p.Witness)
	}
}

// Todos a la vez, en la misma lectura; con Any, alcanza uno.
func TestWatchTodosOAlguno(t *testing.T) {
	a := escucha("0.0.0.0:3000", 1, at(1))
	b := escucha("0.0.0.0:8080", 2, at(1))
	w := NewWatch(WatchSpec{For: WatchListen, Request: pedidoW(t, "3000", "8080")})
	w.Observe(at(10), []ports.Socket{a})
	if w.Done() {
		t.Fatal("falta el 8080")
	}
	w.Observe(at(20), []ports.Socket{b}) // el 3000 se cayó justo cuando llegó el 8080
	if w.Done() || estados(w) != "pendiente,cumplido" {
		t.Fatalf("no a la vez: %s", estados(w))
	}
	w.Observe(at(30), []ports.Socket{a, b})
	if !w.Done() {
		t.Fatal("los dos a la vez")
	}
	w = NewWatch(WatchSpec{For: WatchListen, Request: pedidoW(t, "3000", "8080"), Any: true})
	w.Observe(at(10), []ports.Socket{b})
	if !w.Done() {
		t.Fatal("con --any alcanza el 8080")
	}
}

// Un puerto que tapa WinNAT: sin --any la espera es imposible; con --any, se
// descarta ese y se espera a los otros.
func TestWatchBloqueado(t *testing.T) {
	bl := map[uint16]PortResult{55200: {Port: 55200, Status: PortReserved}}
	w := NewWatch(WatchSpec{For: WatchListen, Request: pedidoW(t, "3000", "55200"), Blocked: bl})
	evs := w.Observe(at(0), []ports.Socket{escucha("0.0.0.0:3000", 1, at(0))})
	if !w.Impossible() || w.Done() || len(evs) != 2 || evs[1].Port.State != WatchBlocked {
		t.Fatalf("sin --any: imposible=%v done=%v %+v", w.Impossible(), w.Done(), evs)
	}
	w = NewWatch(WatchSpec{For: WatchListen, Request: pedidoW(t, "3000", "55200"), Blocked: bl, Any: true})
	w.Observe(at(0), nil)
	if w.Impossible() || w.Done() {
		t.Fatal("con --any queda el 3000")
	}
	w.Observe(at(10), []ports.Socket{escucha("0.0.0.0:3000", 1, at(5))})
	if !w.Done() {
		t.Fatal("con --any, el 3000 alcanza")
	}
	w = NewWatch(WatchSpec{For: WatchListen, Request: pedidoW(t, "55200"), Blocked: bl, Any: true})
	w.Observe(at(0), nil)
	if !w.Impossible() {
		t.Fatal("con --any y todos tapados, imposible")
	}
	// En WatchFree no hay bloqueados: un puerto reservado ya está libre.
	w = NewWatch(WatchSpec{For: WatchFree, Request: pedidoW(t, "55200"), Blocked: bl})
	w.Observe(at(0), nil)
	if !w.Done() || w.Impossible() {
		t.Fatal("libre aunque WinNAT lo tape")
	}
}

// --wait-free: hasta que nadie lo ocupe; con --stable, que se sostenga libre
// desde la primera lectura libre (no hay hora de "dejó de estar").
func TestWatchLibre(t *testing.T) {
	w := NewWatch(WatchSpec{For: WatchFree, Request: pedidoW(t, "3000"), Stable: 500 * time.Millisecond})
	a := escucha("0.0.0.0:3000", 1, at(0))
	w.Observe(at(0), []ports.Socket{a})
	if p := w.Ports()[0]; p.State != WatchPending || len(p.Witness) != 1 {
		t.Fatalf("ocupado: %+v", p)
	}
	w.Observe(at(100), nil)
	if estados(w) != "sosteniéndose" || !w.Next().Equal(at(600)) {
		t.Fatalf("recién libre: %s %v", estados(w), w.Next())
	}
	w.Observe(at(300), []ports.Socket{escucha("0.0.0.0:3000", 2, at(250))}) // lo retomó el supervisor
	w.Observe(at(400), nil)
	w.Observe(at(899), nil)
	if w.Done() {
		t.Fatal("libre de nuevo recién desde el 400")
	}
	w.Observe(at(900), nil)
	if p := w.Ports()[0]; !w.Done() || p.Changes != 1 {
		t.Fatalf("libre y sostenido: %+v", p)
	}
}

// --wait-free con --all-states: una conexión ocupa; los restos del PID 0, no.
func TestWatchLibreConConexiones(t *testing.T) {
	c := conn("127.0.0.1:3000", 44, ports.Established)
	resto := ports.Socket{Proto: ports.TCP, Local: netip.MustParseAddrPort("127.0.0.1:3000"), State: ports.TimeWait}
	w := NewWatch(WatchSpec{For: WatchFree, Request: pedidoW(t, "3000"), Filter: ports.Filter{AllStates: true}})
	w.Observe(at(0), []ports.Socket{c, resto})
	if w.Done() {
		t.Fatal("la conexión lo ocupa")
	}
	w.Observe(at(10), []ports.Socket{resto})
	if !w.Done() {
		t.Fatal("un TIME_WAIT del PID 0 no lo ocupa")
	}
	// Sin --all-states, la conexión no cuenta.
	w = NewWatch(WatchSpec{For: WatchFree, Request: pedidoW(t, "3000")})
	w.Observe(at(0), []ports.Socket{c})
	if !w.Done() {
		t.Fatal("sin --all-states solo cuenta lo que escucha")
	}
}

// --- El bucle, con reloj falso ------------------------------------------------

// tramo es un socket presente en [desde, hasta) (hasta cero = para siempre).
type tramo struct {
	s            ports.Socket
	desde, hasta time.Time
}

// procW es un proceso de la historia: vive hasta exitAt (cero = siempre).
type procW struct {
	id     ID
	parent uint32
	exitAt time.Time
}

// watchSim es un sistema falso para WaitPorts: reloj virtual, la tabla como
// función del tiempo, procesos que salen a una hora fija. El tiempo avanza
// solo en Wait, hasta el plazo, la primera salida esperada o el Ctrl+C.
type watchSim struct {
	now      time.Time
	tramos   []tramo
	procs    []procW
	cancelAt time.Time
	cancel   context.CancelFunc
	failRead error

	reads  []time.Time
	snaps  int
	opened int
	open   map[*watchHandle]bool
	waits  [][]ID // a quiénes se esperó en cada Wait
}

type watchHandle struct {
	s  *watchSim
	id ID
}

func (h *watchHandle) ID() ID             { return h.id }
func (h *watchHandle) CanTerminate() bool { return false }
func (h *watchHandle) Terminate() error   { return ErrAccessDenied }
func (h *watchHandle) Close()             { delete(h.s.open, h) }
func (s *watchSim) Now() time.Time        { return s.now }
func (s *watchSim) alive(p procW) bool    { return p.exitAt.IsZero() || s.now.Before(p.exitAt) }
func newWatchSim(t0 time.Time) *watchSim  { return &watchSim{now: t0, open: map[*watchHandle]bool{}} }
func (s *watchSim) add(x ports.Socket, desde, hasta int) {
	tr := tramo{s: x, desde: at(desde)}
	if hasta > 0 {
		tr.hasta = at(hasta)
	}
	s.tramos = append(s.tramos, tr)
}

func (s *watchSim) table() []ports.Socket {
	var out []ports.Socket
	for _, tr := range s.tramos {
		if !s.now.Before(tr.desde) && (tr.hasta.IsZero() || s.now.Before(tr.hasta)) {
			out = append(out, tr.s)
		}
	}
	return out
}

func (s *watchSim) Listening(ctx context.Context) ([]ports.Socket, error) {
	if s.failRead != nil {
		return nil, s.failRead
	}
	s.reads = append(s.reads, s.now)
	var out []ports.Socket
	for _, x := range s.table() {
		if x.Proto == ports.UDP || x.State == ports.Listen {
			out = append(out, x)
		}
	}
	return out, nil
}

func (s *watchSim) AllSockets(ctx context.Context) ([]ports.Socket, error) {
	s.reads = append(s.reads, s.now)
	return s.table(), nil
}

func (s *watchSim) Snapshot(ctx context.Context) ([]Proc, error) {
	s.snaps++
	var out []Proc
	for _, p := range s.procs {
		if s.alive(p) {
			out = append(out, Proc{ID: p.id, Parent: p.parent, Name: "p.exe"})
		}
	}
	return out, nil
}

func (s *watchSim) OpenWait(id ID) (Handle, error) {
	for _, p := range s.procs {
		if p.id == id && s.alive(p) {
			h := &watchHandle{s: s, id: id}
			s.open[h] = true
			s.opened++
			return h, nil
		}
	}
	return nil, ErrGone
}

func (s *watchSim) Wait(ctx context.Context, hs []Handle, deadline time.Time) (Handle, ExitInfo, WaitOutcome) {
	var ids []ID
	target, who := deadline, Handle(nil)
	for _, h := range hs {
		ids = append(ids, h.ID())
		for _, p := range s.procs {
			if p.id == h.ID() && !p.exitAt.IsZero() && p.exitAt.Before(target.Add(1)) && (who == nil || p.exitAt.Before(target)) {
				target, who = p.exitAt, h
			}
		}
	}
	s.waits = append(s.waits, ids)
	if !s.cancelAt.IsZero() && !s.cancelAt.After(target) {
		if s.cancelAt.After(s.now) {
			s.now = s.cancelAt
		}
		s.cancel()
		return nil, ExitInfo{}, WaitCanceled
	}
	if target.After(s.now) {
		s.now = target
	}
	if who != nil {
		return who, ExitInfo{}, WaitExited
	}
	return nil, ExitInfo{}, WaitDeadline
}

func correrEspera(s *watchSim, spec WatchSpec, plazo int) WatchReport {
	c, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.cancel = cancel
	return WaitPorts(c, s, spec, at(plazo), nil)
}

// Entre dos lecturas nunca pasa más de 200 ms.
func huecosOK(t *testing.T, s *watchSim) {
	t.Helper()
	for i := 1; i < len(s.reads); i++ {
		if d := s.reads[i].Sub(s.reads[i-1]); d > watchPollMax {
			t.Fatalf("pasaron %v entre las lecturas %d y %d", d, i-1, i)
		}
	}
}

func TestWaitPortsEscucha(t *testing.T) {
	s := newWatchSim(at(0))
	s.add(escucha("[::1]:5173", 812, at(500)), 500, 0)
	rep := correrEspera(s, WatchSpec{For: WatchListen, Request: pedidoW(t, "5173")}, 60000)
	if rep.Outcome != WatchDone {
		t.Fatalf("resultado %v", rep.Outcome)
	}
	// Lecturas en 0, 25, 75, 175, 375 (el backoff crece) y 575: la primera
	// que lo ve. A lo sumo 200 ms tarde.
	if got := rep.End.Sub(at(500)); got < 0 || got > watchPollMax {
		t.Fatalf("lo vio %v después de aparecer", got)
	}
	if rep.Reads != 6 || !s.reads[5].Equal(at(575)) {
		t.Fatalf("lecturas: %v", s.reads)
	}
	huecosOK(t, s)
}

func TestWaitPortsVence(t *testing.T) {
	s := newWatchSim(at(0))
	rep := correrEspera(s, WatchSpec{For: WatchListen, Request: pedidoW(t, "5173")}, 1000)
	if rep.Outcome != WatchTimeout || !rep.End.Equal(at(1000)) || !s.reads[len(s.reads)-1].Equal(at(1000)) {
		t.Fatalf("resultado %v a las %v; lecturas %v", rep.Outcome, rep.End, s.reads)
	}
	huecosOK(t, s)
	// Plazo ya vencido: se mira una vez.
	s = newWatchSim(at(0))
	if rep := correrEspera(s, WatchSpec{For: WatchListen, Request: pedidoW(t, "5173")}, 0); rep.Outcome != WatchTimeout || rep.Reads != 1 {
		t.Fatalf("plazo cero: %v, %d lecturas", rep.Outcome, rep.Reads)
	}
}

func TestWaitPortsCancelado(t *testing.T) {
	s := newWatchSim(at(0))
	s.cancelAt = at(300)
	rep := correrEspera(s, WatchSpec{For: WatchListen, Request: pedidoW(t, "5173")}, 60000)
	if rep.Outcome != WatchCanceled || !rep.End.Equal(at(300)) {
		t.Fatalf("resultado %v a las %v", rep.Outcome, rep.End)
	}
}

func TestWaitPortsErrorDeLectura(t *testing.T) {
	s := newWatchSim(at(0))
	s.failRead = errors.New("sin tabla")
	rep := correrEspera(s, WatchSpec{For: WatchListen, Request: pedidoW(t, "5173")}, 1000)
	if rep.Outcome != WatchFailed || rep.Err == nil {
		t.Fatalf("resultado %v", rep.Outcome)
	}
}

// nodemon: tres reinicios; con --stable 750ms se cumple exactamente 750 ms
// después de que nació el último socket, sin esperar al próximo tick.
func TestWaitPortsEstableNodemon(t *testing.T) {
	s := newWatchSim(at(0))
	s.add(escucha("0.0.0.0:5173", 101, at(100)), 100, 400)
	s.add(escucha("0.0.0.0:5173", 102, at(450)), 450, 900)
	s.add(escucha("0.0.0.0:5173", 103, at(950)), 950, 0)
	rep := correrEspera(s, WatchSpec{For: WatchListen, Request: pedidoW(t, "5173"), Stable: 750 * time.Millisecond}, 60000)
	if rep.Outcome != WatchDone || !rep.End.Equal(at(1700)) {
		t.Fatalf("resultado %v a las %v; lecturas %v", rep.Outcome, rep.End, s.reads)
	}
	if p := rep.Ports[0]; p.Witness[0].PID != 103 || p.Changes < 2 {
		t.Fatalf("puerto: %+v", p)
	}
	huecosOK(t, s)
}

// --wait-free: la salida del dueño despierta la espera al instante (aunque el
// backoff ya estaba en 200 ms) y en la lectura siguiente el puerto está libre.
func TestWaitPortsLibreDespiertaConLaSalida(t *testing.T) {
	s := newWatchSim(at(0))
	dueño := ID{PID: 812, Created: at(-5000)}
	s.procs = []procW{{id: dueño, parent: 1, exitAt: at(1234)}}
	s.add(escucha("0.0.0.0:3000", 812, at(-4000)), -4000, 1240) // el kernel lo suelta 6 ms después
	rep := correrEspera(s, WatchSpec{For: WatchFree, Request: pedidoW(t, "3000")}, 60000)
	if rep.Outcome != WatchDone {
		t.Fatalf("resultado %v", rep.Outcome)
	}
	if got := rep.End.Sub(at(1234)); got < 0 || got > watchPollMin {
		t.Fatalf("lo vio libre %v después de la salida (lecturas %v)", got, s.reads)
	}
	if len(s.open) != 0 || s.opened != 1 {
		t.Fatalf("handles: abiertos %d, quedaron %d", s.opened, len(s.open))
	}
	esperó := false
	for _, ids := range s.waits {
		esperó = esperó || (len(ids) == 1 && ids[0] == dueño)
	}
	if !esperó {
		t.Fatal("no esperó al dueño")
	}
}

// Un socket heredado: su PID murió y lo sostiene un hijo que nació después
// del socket. Se espera a ESE hijo.
func TestWaitPortsLibreFantasma(t *testing.T) {
	s := newWatchSim(at(0))
	hijo := ID{PID: 900, Created: at(-3000)}
	otro := ID{PID: 901, Created: at(-9000)} // hijo del mismo PID, pero nacido antes del socket
	s.procs = []procW{{id: hijo, parent: 812, exitAt: at(700)}, {id: otro, parent: 812}}
	s.add(escucha("0.0.0.0:3000", 812, at(-4000)), -4000, 705)
	rep := correrEspera(s, WatchSpec{For: WatchFree, Request: pedidoW(t, "3000")}, 60000)
	if rep.Outcome != WatchDone || rep.End.Sub(at(700)) > watchPollMin {
		t.Fatalf("resultado %v a las %v", rep.Outcome, rep.End)
	}
	for _, ids := range s.waits {
		for _, id := range ids {
			if id == otro {
				t.Fatal("esperó a un proceso que no pudo heredar el socket")
			}
		}
	}
}

// El PID del socket lo reusa un proceso MÁS NUEVO que el socket: ese no lo
// puede tener. Se espera a los herederos del dueño original, no al nuevo.
func TestWaitPortsLibrePIDReciclado(t *testing.T) {
	s := newWatchSim(at(0))
	nuevo := ID{PID: 812, Created: at(-1000)} // nació después del socket: no es el dueño
	heredero := ID{PID: 900, Created: at(-3000)}
	s.procs = []procW{{id: nuevo, parent: 1}, {id: heredero, parent: 812, exitAt: at(700)}}
	s.add(escucha("0.0.0.0:3000", 812, at(-4000)), -4000, 705)
	rep := correrEspera(s, WatchSpec{For: WatchFree, Request: pedidoW(t, "3000")}, 60000)
	if rep.Outcome != WatchDone || rep.End.Sub(at(700)) > watchPollMin {
		t.Fatalf("resultado %v a las %v", rep.Outcome, rep.End)
	}
	for _, ids := range s.waits {
		for _, id := range ids {
			if id == nuevo {
				t.Fatal("esperó al proceso que reusó el PID")
			}
		}
	}
}

// Los dueños no se vuelven a buscar mientras no cambien: una foto de
// procesos, no una por lectura.
func TestWaitPortsLibreUnaFoto(t *testing.T) {
	s := newWatchSim(at(0))
	s.procs = []procW{{id: ID{PID: 812, Created: at(-5000)}}}
	s.add(escucha("0.0.0.0:3000", 812, at(-4000)), -4000, 0)
	rep := correrEspera(s, WatchSpec{For: WatchFree, Request: pedidoW(t, "3000")}, 3000)
	if rep.Outcome != WatchTimeout || s.snaps != 1 || rep.Reads < 10 || len(s.open) != 0 {
		t.Fatalf("resultado %v, %d fotos, %d lecturas, %d handles abiertos", rep.Outcome, s.snaps, rep.Reads, len(s.open))
	}
}

// Imposible: corta en la primera lectura.
func TestWaitPortsImposible(t *testing.T) {
	s := newWatchSim(at(0))
	bl := map[uint16]PortResult{55200: {Port: 55200, Status: PortReserved}}
	rep := correrEspera(s, WatchSpec{For: WatchListen, Request: pedidoW(t, "55200"), Blocked: bl}, 60000)
	if rep.Outcome != WatchImpossible || rep.Reads != 1 || !rep.End.Equal(at(0)) {
		t.Fatalf("resultado %v, %d lecturas", rep.Outcome, rep.Reads)
	}
}

// PROPIEDAD, con historias al azar (semillas fijas): el bucle declara
// cumplido solo lo que la última lectura muestra cumplido (un Watch nuevo que
// ve toda la historia de lecturas llega a lo mismo), nunca deja pasar más de
// 200 ms entre lecturas, termina a más tardar en el plazo, y no deja handles
// abiertos.
func TestWaitPortsPropiedades(t *testing.T) {
	for seed := uint64(1); seed <= 300; seed++ {
		rng := rand.New(rand.NewPCG(seed, 0xA17))
		s := newWatchSim(at(0))
		for i := range 1 + rng.IntN(6) {
			pid := uint32(800 + rng.IntN(4))
			desde := rng.IntN(3000)
			hasta := 0
			if rng.IntN(2) == 0 {
				hasta = desde + 1 + rng.IntN(1500)
			}
			addr := []string{"0.0.0.0", "127.0.0.1", "[::1]", "[::]"}[rng.IntN(4)]
			sinc := at(desde)
			if rng.IntN(5) == 0 {
				sinc = time.Time{}
			}
			s.add(escucha(addr+":"+[]string{"3000", "3001"}[rng.IntN(2)], pid, sinc), desde, hasta)
			if i < 3 {
				s.procs = append(s.procs, procW{id: ID{PID: pid, Created: at(-10000 - i)}, exitAt: at(rng.IntN(4000))})
			}
		}
		spec := WatchSpec{For: []WatchFor{WatchListen, WatchFree}[rng.IntN(2)], Request: pedidoW(t, []string{"3000", "3000,3001", "localhost:3000"}[rng.IntN(3)]),
			Any: rng.IntN(2) == 0, Stable: time.Duration(rng.IntN(4)) * 300 * time.Millisecond}
		if spec.For == WatchListen && rng.IntN(3) == 0 {
			spec.NewSince = at(rng.IntN(1000))
		}
		plazo := 500 + rng.IntN(5000)
		rep := correrEspera(s, spec, plazo)

		huecosOK(t, s)
		if rep.End.After(at(plazo)) {
			t.Fatalf("semilla %d: terminó a las %v con plazo %v", seed, rep.End, at(plazo))
		}
		if len(s.open) != 0 {
			t.Fatalf("semilla %d: quedaron %d handles abiertos", seed, len(s.open))
		}
		// Rehacer la cuenta con las mismas lecturas.
		w := NewWatch(spec)
		for _, r := range s.reads {
			w.Observe(r, tablaEn(s, r, spec))
		}
		if (rep.Outcome == WatchDone) != w.Done() {
			t.Fatalf("semilla %d: el bucle dice %v y la historia dice done=%v", seed, rep.Outcome, w.Done())
		}
		if rep.Outcome != WatchDone && rep.Outcome != WatchTimeout {
			t.Fatalf("semilla %d: resultado %v", seed, rep.Outcome)
		}
		// Oráculo INDEPENDIENTE (sin Watch) para lo más simple —puertos
		// enteros, sin --stable ni --new—: la condición vale en la tabla de la
		// última lectura si y solo si terminó cumplida, y no valía en ninguna
		// lectura anterior (no se pasó de largo).
		if spec.Stable == 0 && spec.NewSince.IsZero() && !spec.Request.Aimed() {
			for i, r := range s.reads {
				vale := condicionIngenua(spec, tablaEn(s, r, spec))
				ultima := i == len(s.reads)-1
				if ultima && vale != (rep.Outcome == WatchDone) {
					t.Fatalf("semilla %d: en la última lectura la condición vale=%v y el resultado es %v", seed, vale, rep.Outcome)
				}
				if !ultima && vale {
					t.Fatalf("semilla %d: la condición ya valía en la lectura %d (%v) y siguió esperando", seed, i, r)
				}
			}
		}
	}
}

// condicionIngenua dice si lo pedido se cumple en esta tabla, mirado puerto
// por puerto y sin nada de la máquina de estados.
func condicionIngenua(spec WatchSpec, tabla []ports.Socket) bool {
	cumplidos, total := 0, 0
	for p := range spec.Request.Ports.All() {
		total++
		hay := false
		for _, s := range tabla {
			hay = hay || (s.Port() == p && s.State == ports.Listen)
		}
		if hay == (spec.For == WatchListen) {
			cumplidos++
		}
	}
	if spec.Any {
		return cumplidos > 0
	}
	return cumplidos == total
}

// tablaEn es lo que se leyó en el instante r.
func tablaEn(s *watchSim, r time.Time, spec WatchSpec) []ports.Socket {
	saved := s.now
	defer func() { s.now = saved }()
	s.now = r
	var out []ports.Socket
	for _, x := range s.table() {
		if x.Proto == ports.UDP || x.State == ports.Listen {
			out = append(out, x)
		}
	}
	return out
}

// --- Falla rápida ------------------------------------------------------------

func TestDiagnoserBlocked(t *testing.T) {
	tcp := ports.Filter{TCP: true}
	casos := []struct {
		nombre string
		excl   ports.Exclusions
		binds  map[bindKey]ports.BindResult
		filtro ports.Filter
		tapado bool
	}{
		{"bloque de WinNAT en las dos familias", winnatBlock(55150, 55249),
			map[bindKey]ports.BindResult{{ports.TCP, ports.IPv4, 55200}: ports.BindForbidden, {ports.TCP, ports.IPv6, 55200}: ports.BindForbidden}, tcp, true},
		{"solo en IPv4: un servidor en ::1 puede", winnatBlock(55150, 55249)[:1],
			map[bindKey]ports.BindResult{{ports.TCP, ports.IPv4, 55200}: ports.BindForbidden}, tcp, false},
		{"la tabla es vieja y el bind anda", winnatBlock(55150, 55249), nil, tcp, false},
		{"exclusión administrada: un programa la puede usar", administered(55150, 55249),
			map[bindKey]ports.BindResult{{ports.TCP, ports.IPv4, 55200}: ports.BindForbidden, {ports.TCP, ports.IPv6, 55200}: ports.BindForbidden}, tcp, false},
		{"bind negado sin exclusión: ambiguo, no se corta", nil,
			map[bindKey]ports.BindResult{{ports.TCP, ports.IPv4, 55200}: ports.BindForbidden, {ports.TCP, ports.IPv6, 55200}: ports.BindForbidden}, tcp, false},
		{"TCP tapado pero se esperan los dos protocolos", winnatBlock(55150, 55249),
			map[bindKey]ports.BindResult{{ports.TCP, ports.IPv4, 55200}: ports.BindForbidden, {ports.TCP, ports.IPv6, 55200}: ports.BindForbidden}, ports.Filter{}, false},
		// Los dos binds negados, pero en IPv6 sin exclusión que lo explique (un
		// socket atado que todavía no escucha): ambiguo, no se corta.
		{"IPv4 tapado por WinNAT e IPv6 negado sin exclusión", winnatBlock(55150, 55249)[:1],
			map[bindKey]ports.BindResult{{ports.TCP, ports.IPv4, 55200}: ports.BindForbidden, {ports.TCP, ports.IPv6, 55200}: ports.BindForbidden}, tcp, false},
	}
	for _, c := range casos {
		s := newSim(epoch)
		s.excluded = c.excl
		for k, v := range c.binds {
			s.bindsBy[k] = v
		}
		res, tapado := NewDiagnoser(s, c.filtro, true).Blocked(55200)
		if !c.tapado && c.binds == nil && s.exclReads != 0 {
			t.Errorf("%s: el bind anda y leyó las exclusiones igual (netsh de gusto)", c.nombre)
		}
		if tapado != c.tapado {
			t.Errorf("%s: tapado=%v", c.nombre, tapado)
			continue
		}
		if tapado && (res.Status != PortReserved || res.Reserved.Administered || len(res.Remedies) == 0 || remedio(res.Remedies[0]) != "reservar 55200/TCP") {
			t.Errorf("%s: diagnóstico %+v", c.nombre, res)
		}
	}
	// Sin sondeo (un pedido de miles de puertos) decide la tabla: las dos
	// familias tienen que estar en un bloque de WinNAT.
	for _, c := range []struct {
		excl   ports.Exclusions
		tapado bool
	}{{winnatBlock(55150, 55249), true}, {winnatBlock(55150, 55249)[:1], false}, {administered(55150, 55249), false}} {
		s := newSim(epoch)
		s.excluded = c.excl
		if _, tapado := NewDiagnoser(s, tcp, false).Blocked(55200); tapado != c.tapado || s.probes != 0 {
			t.Errorf("sin sondeo, %v: tapado=%v (sondeos %d)", c.excl, tapado, s.probes)
		}
	}
}
