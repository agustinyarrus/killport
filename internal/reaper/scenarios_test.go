package reaper

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

var req = ports.Of(3000)

// assertVictim revisa el destino y el método de la única víctima.
func assertVictim(t *testing.T, rep Report, o Outcome, m Method) {
	t.Helper()
	if len(rep.Victims) != 1 {
		t.Fatalf("esperaba 1 víctima, hubo %d: %+v", len(rep.Victims), rep.Victims)
	}
	v := rep.Victims[0]
	if v.Outcome != o || (m != MethodNone && v.Method != m) {
		t.Fatalf("víctima = %v/%v, esperaba %v/%v", v.Outcome, v.Method, o, m)
	}
}

func assertPort(t *testing.T, rep Report, st PortStatus) PortResult {
	t.Helper()
	if len(rep.Ports) != 1 {
		t.Fatalf("esperaba 1 puerto, hubo %d", len(rep.Ports))
	}
	if rep.Ports[0].Status != st {
		t.Fatalf("puerto = %v (%s), esperaba %v", rep.Ports[0].Status, rep.Ports[0].Advice, st)
	}
	return rep.Ports[0]
}

// 1. Cierra con Ctrl+C tras 420 ms.
func TestEscCtrlC(t *testing.T) {
	s, cfg, opts := escenario()
	n := nodeTarget(s)
	n.console = []uint32{100}
	n.ctrlCReacts, n.ctrlCAfter = true, 420*time.Millisecond
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	if len(pl.Targets) != 1 || pl.Targets[0].Kind != KillProcess {
		t.Fatalf("plan = %+v", pl.Targets)
	}
	rep, rec := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeClosed, MethodCtrlC)
	ev, _ := rec.first(EvExited)
	if ev.Elapsed != 420*time.Millisecond || !ev.Time.Equal(at(420)) {
		t.Fatalf("salida en %v a las %v, esperaba 420 ms a las +420", ev.Elapsed, ev.Time.Sub(epoch))
	}
	if !rec.has(EvCtrlC) || rec.has(EvTerminated) {
		t.Fatalf("debía mandar Ctrl+C y NO terminar en seco: %v", rec.kinds())
	}
	assertPort(t, rep, PortFree)
	if !s.now.Equal(at(420)) {
		t.Fatalf("reloj virtual en %v, esperaba +420 ms", s.now.Sub(epoch))
	}
}

// 2. Ignora el Ctrl+C: hay que terminarlo en seco al vencer el plazo.
func TestEscIgnoraCtrlC(t *testing.T) {
	s, cfg, opts := escenario()
	n := nodeTarget(s)
	n.console = []uint32{100}
	n.ctrlCReacts = false // sordo al Ctrl+C
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	rep, rec := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeTerminated, MethodTerminate)
	if !rec.has(EvCtrlC) || !rec.has(EvTerminated) {
		t.Fatalf("esperaba Ctrl+C y luego terminación: %v", rec.kinds())
	}
	// El cierre prolijo consumió el plazo entero (3 s) antes de terminar.
	if ev, _ := rec.first(EvTerminated); !ev.Time.Equal(at(3000)) {
		t.Fatalf("terminó a las %v, esperaba +3 s", ev.Time.Sub(epoch))
	}
	assertPort(t, rep, PortFree)
}

// 3. Cierra con WM_CLOSE (proceso con ventana, sin consola).
func TestEscWMClose(t *testing.T) {
	s, cfg, opts := escenario()
	n := nodeTarget(s)
	n.name = "app.exe"
	n.windows, n.closeAfter = 1, 300*time.Millisecond
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	rep, rec := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeClosed, MethodClose)
	if !rec.has(EvClose) || rec.has(EvCtrlC) {
		t.Fatalf("esperaba WM_CLOSE y NO Ctrl+C: %v", rec.kinds())
	}
	if ev, _ := rec.first(EvExited); ev.Elapsed != 300*time.Millisecond {
		t.Fatalf("cerró en %v, esperaba 300 ms", ev.Elapsed)
	}
	assertPort(t, rep, PortFree)
}

// 4. Acceso denegado al abrir: candidato a elevación, no se toca.
func TestEscAccesoDenegado(t *testing.T) {
	s, cfg, opts := escenario()
	n := nodeTarget(s)
	n.openErr = ErrAccessDenied
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	rep, rec := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeDenied, MethodNone)
	if !rec.has(EvOpenFailed) {
		t.Fatal("esperaba EvOpenFailed")
	}
	assertPort(t, rep, PortBusy) // sigue ocupado: no se pudo
}

// 5. Protegido (PPL): ni con admin.
func TestEscProtegido(t *testing.T) {
	s, cfg, opts := escenario()
	n := nodeTarget(s)
	n.openErr = ErrProtected
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	rep, _ := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeProtected, MethodNone)
}

// 6. Ya estaba muerto entre la instantánea y la apertura.
func TestEscYaMuerto(t *testing.T) {
	s, cfg, opts := escenario()
	n := nodeTarget(s)
	before, pl := buildPlan(s, cfg, req, opts.Filter) // se planifica con node vivo
	n.exited = true                                   // muere justo antes de abrirlo
	rep, _ := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeGone, MethodNone)
	assertPort(t, rep, PortFree) // su socket se fue con él
}

// 7. PID reciclado entre la instantánea y la apertura: no se toca al impostor.
func TestEscReciclado(t *testing.T) {
	s, cfg, opts := escenario()
	n := nodeTarget(s)
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	n.openErr = ErrRecycled // el PID 100 ahora es otro proceso
	n.exited = true         // el node original ya no está (su socket tampoco)
	rep, _ := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeRecycled, MethodNone)
	assertPort(t, rep, PortFree)
}

// 8. Un supervisor relanza al hijo con OTRO PID: respawn detectado por LCA cruzado.
func TestEscRespawn(t *testing.T) {
	s, cfg, opts := escenario()
	s.add(&simProc{id: ID{PID: 10, Created: at(0)}, name: "nodemon.exe", parent: 500})
	n := s.add(&simProc{id: ID{PID: 100, Created: at(50)}, name: "node.exe", parent: 10})
	s.sockets = append(s.sockets, tcpListen(3000, 100, at(60)))
	n.console = []uint32{100}
	n.ctrlCReacts, n.ctrlCAfter = true, 200*time.Millisecond
	before, pl := buildPlan(s, cfg, req, opts.Filter)

	// Tras matarlo, nodemon relanza node con PID 101.
	s.afterFn = func(*simOS) []ports.Socket { return []ports.Socket{tcpListen(3000, 101, at(1000))} }
	s.snapFn = func(*simOS) []Proc {
		return []Proc{
			{ID: ID{PID: 10, Created: at(0)}, Name: "nodemon.exe", Parent: 500},
			{ID: ID{PID: 101, Created: at(1000)}, Name: "node.exe", Parent: 10},
		}
	}
	rep, _ := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeClosed, MethodCtrlC) // la víctima SÍ cerró
	pr := assertPort(t, rep, PortRespawned)
	if pr.SupervisorName != "nodemon.exe" {
		t.Fatalf("supervisor = %q, esperaba nodemon.exe (advice: %s)", pr.SupervisorName, pr.Advice)
	}
}

// 9. Socket heredado por un hijo tras morir el padre: fantasma en la verificación.
func TestEscFantasma(t *testing.T) {
	s, cfg, opts := escenario()
	n := nodeTarget(s)
	n.console = []uint32{100}
	n.ctrlCReacts, n.ctrlCAfter = true, 100*time.Millisecond
	before, pl := buildPlan(s, cfg, req, opts.Filter)

	// Tras matar a node, el socket queda a nombre de un PID muerto (999) y lo
	// sostiene un hijo heredero (PID 50) que nació después del socket.
	s.afterFn = func(*simOS) []ports.Socket { return []ports.Socket{tcpListen(3000, 999, at(60))} }
	s.snapFn = func(*simOS) []Proc {
		return []Proc{{ID: ID{PID: 50, Created: at(400)}, Name: "worker.exe", Parent: 999}}
	}
	rep, _ := exec(s, before, req, pl, cfg, opts)
	pr := assertPort(t, rep, PortPhantom)
	if len(pr.Heirs) != 1 || pr.Heirs[0].PID != 50 {
		t.Fatalf("herederos = %v, esperaba el PID 50", pr.Heirs)
	}
}

// 10. Servicio de terceros que tarda en parar.
func TestEscServicioLento(t *testing.T) {
	s, cfg, opts := escenario()
	s.add(&simProc{id: ID{PID: 200, Created: at(0)}, name: "postgres.exe", parent: 1, session: 0, path: `C:\Program Files\PostgreSQL\bin\postgres.exe`})
	s.sockets = append(s.sockets, tcpListen(5432, 200, at(10)))
	s.svc["postgresql-x64"] = &simSvc{name: "postgresql-x64", ownerPID: 200, stopFor: 800 * time.Millisecond}
	requested := ports.Of(5432)
	before, pl := buildPlan(s, cfg, requested, opts.Filter)
	if len(pl.Targets) != 1 || pl.Targets[0].Kind != StopThirdSvc || pl.Targets[0].Service != "postgresql-x64" {
		t.Fatalf("esperaba un objetivo StopThirdSvc: %+v", pl.Targets)
	}
	rep, rec := exec(s, before, requested, pl, cfg, opts)
	if len(rep.Victims) != 1 || rep.Victims[0].Outcome != OutcomeServiceStopped {
		t.Fatalf("víctima = %+v", rep.Victims)
	}
	if ev, ok := rec.first(EvServiceStopped); !ok || !ev.Time.Equal(at(800)) {
		t.Fatalf("servicio detenido a las %v, esperaba +800 ms", ev.Time.Sub(epoch))
	}
	if rep.Ports[0].Status != PortFree {
		t.Fatalf("puerto = %v", rep.Ports[0].Status)
	}
}

// 11. Consola compartida con un ajeno: NO se manda Ctrl+C, se termina en seco.
func TestEscConsolaCompartida(t *testing.T) {
	s, cfg, opts := escenario()
	n := nodeTarget(s)
	n.console = []uint32{100, 777} // 777 es un proceso ajeno
	n.ctrlCReacts = true
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	rep, rec := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeTerminated, MethodTerminate)
	ev, ok := rec.first(EvCtrlCSkipped)
	if !ok || rec.has(EvCtrlC) {
		t.Fatalf("no debía mandar Ctrl+C a una consola compartida: %v", rec.kinds())
	}
	if ev.Err == nil {
		t.Fatal("el evento omitido debería llevar el error con los intrusos")
	}
}

// 12. La consola propia de killport: jamás se le manda Ctrl+C.
func TestEscConsolaPropia(t *testing.T) {
	s, cfg, opts := escenario()
	n := nodeTarget(s)
	n.console = []uint32{100, kpPID} // killport comparte la consola
	n.ctrlCReacts = true
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	rep, rec := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeTerminated, MethodTerminate)
	ev, ok := rec.first(EvCtrlCSkipped)
	if !ok || rec.has(EvCtrlC) {
		t.Fatalf("no debía mandar Ctrl+C a su propia consola: %v", rec.kinds())
	}
	if ev.Detail == "" {
		t.Fatal("debería explicar que es la consola propia")
	}
}

// 13. El usuario corta con Ctrl+C a mitad de la escalada: se deja todo como está.
func TestEscCancelacion(t *testing.T) {
	s, cfg, opts := escenario()
	n := nodeTarget(s)
	n.console = []uint32{100}
	n.ctrlCReacts, n.ctrlCAfter = true, 2*time.Second // cerraría recién a los 2 s
	s.cancelAt = at(1000)                             // el usuario corta al segundo
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	rep, rec := exec(s, before, req, pl, cfg, opts)
	if !rep.Canceled {
		t.Fatal("el reporte debería marcar la cancelación")
	}
	if !rec.has(EvCanceled) {
		t.Fatalf("esperaba EvCanceled: %v", rec.kinds())
	}
	if ev, _ := rec.first(EvCanceled); !ev.Time.Equal(at(1000)) {
		t.Fatalf("canceló a las %v, esperaba +1 s", ev.Time.Sub(epoch))
	}
	if rep.Victims[0].Outcome != OutcomePending {
		t.Fatalf("la víctima quedó como estaba: %v, no %v", OutcomePending, rep.Victims[0].Outcome)
	}
	if rec.has(EvTerminated) {
		t.Fatal("no debía terminar en seco después de cancelar")
	}
}

// Puerto sin dueño que cae en un bloque de WinNAT: no hay a quién matar.
func TestEscReservado(t *testing.T) {
	s, cfg, opts := escenario()
	requested := ports.Of(50000)
	s.excluded = ports.Exclusions{{Proto: ports.TCP, Family: ports.IPv4, Range: ports.Range{Lo: 50000, Hi: 50099}}}
	s.bindsBy[bindKey{ports.TCP, ports.IPv4, 50000}] = ports.BindForbidden
	before, pl := buildPlan(s, cfg, requested, opts.Filter)
	if !pl.Empty() || !pl.Ownerless.Contains(50000) {
		t.Fatalf("un puerto sin socket no tiene objetivos y es 'sin dueño': %+v", pl)
	}
	rep, _ := exec(s, before, requested, pl, cfg, opts)
	pr := assertPort(t, rep, PortReserved)
	if pr.Reserved.Lo != 50000 || pr.Advice == "" || pr.Reason == "" || len(pr.Remedies) == 0 {
		t.Fatalf("rango reservado = %+v", pr)
	}
}

// Puerto que ya estaba libre: nada que hacer.
func TestEscYaLibre(t *testing.T) {
	s, cfg, opts := escenario()
	requested := ports.Of(3000)
	before, pl := buildPlan(s, cfg, requested, opts.Filter)
	if !pl.Empty() {
		t.Fatalf("sin sockets no hay nada que planear: %+v", pl)
	}
	rep, _ := exec(s, before, requested, pl, cfg, opts)
	assertPort(t, rep, PortFree)
	if len(rep.Victims) != 0 {
		t.Fatalf("no debería haber víctimas: %+v", rep.Victims)
	}
}

// C-07: el tiempo de "cerró en X" se mide desde que se le PIDIÓ que cerrara,
// hasta la hora de salida que da el kernel. El ayudante del Ctrl+C tarda
// 100 ms en volver: uno que cede a los 30 ms ya salió cuando empieza la
// espera (antes figuraba con 0 ms), y a uno que tarda 420 ms no se le
// descuenta lo que tardó el ayudante.
func TestEscTiempoDesdeLaSenal(t *testing.T) {
	for _, c := range []struct{ cede, want time.Duration }{{30 * time.Millisecond, 30 * time.Millisecond}, {420 * time.Millisecond, 420 * time.Millisecond}} {
		s, cfg, opts := escenario()
		s.ctrlCCost = 100 * time.Millisecond
		n := nodeTarget(s)
		n.console = []uint32{100}
		n.ctrlCReacts, n.ctrlCAfter = true, c.cede
		before, pl := buildPlan(s, cfg, req, opts.Filter)
		rep, rec := exec(s, before, req, pl, cfg, opts)
		assertVictim(t, rep, OutcomeClosed, MethodCtrlC)
		ev, _ := rec.first(EvExited)
		if ev.Elapsed != c.want || rep.Victims[0].Elapsed != c.want {
			t.Errorf("cede a los %v: el evento dice %v y el resultado %v", c.cede, ev.Elapsed, rep.Victims[0].Elapsed)
		}
	}
}

// C-08: el usuario corta mientras se espera que un servicio de terceros quede
// detenido. No es una falla del servicio: el pedido ya se mandó.
type simCortaServicio struct{ *simOS }

func (s simCortaServicio) StopService(ctx context.Context, name string, deadline time.Time) error {
	s.cancelFn() // el Ctrl+C llega mientras se espera STOPPED
	return ctx.Err()
}

func TestEscServicioCancelado(t *testing.T) {
	s, cfg, opts := escenario()
	s.add(&simProc{id: ID{PID: 200, Created: at(0)}, name: "postgres.exe", parent: 1, session: 0, path: `C:\Program Files\PostgreSQL\bin\postgres.exe`})
	s.sockets = append(s.sockets, tcpListen(5432, 200, at(10)))
	s.svc["postgresql-x64"] = &simSvc{name: "postgresql-x64", ownerPID: 200, stopFor: 800 * time.Millisecond}
	requested := ports.Of(5432)
	before, pl := buildPlan(s, cfg, requested, opts.Filter)
	rec := &recorder{}
	opts.Emit = rec.emit
	c, cancel := context.WithCancel(context.Background())
	s.cancelFn = cancel
	rep := Run(c, simCortaServicio{s}, before, requested, pl, cfg, opts)
	cancel()
	if !rep.Canceled || rep.Victims[0].Outcome != OutcomePending || !errors.Is(rep.Victims[0].Err, ErrStopPending) {
		t.Fatalf("víctima = %+v (cancelado %v)", rep.Victims[0], rep.Canceled)
	}
	if rec.has(EvServiceFailed) {
		t.Fatalf("un Ctrl+C no es una falla del servicio: %v", rec.kinds())
	}
	if pr := rep.Ports[0]; pr.Status != PortBusy || !strings.Contains(pr.Reason, "mientras se detenía") {
		t.Fatalf("puerto = %+v", pr)
	}
}
