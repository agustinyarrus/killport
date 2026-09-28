package reaper

import (
	"math/rand/v2"
	"net/netip"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Invariantes del motor y de la verificación que no son "un caso": son reglas
// que tienen que valer siempre. Cada test nombra la regla que defiende.

// Regla: el plazo del cierre prolijo solo corre si alguien recibió una señal.
// Con --force nadie la recibe: se termina en el instante cero, sin regalar el
// --timeout (antes se esperaban los 3 s enteros para nada).
func TestInvForceNoEspera(t *testing.T) {
	s, cfg, opts := escenario()
	n := nodeTarget(s)
	n.console = []uint32{100}
	n.ctrlCReacts, n.ctrlCAfter = true, 100*time.Millisecond
	cfg.Force = true
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	rep, rec := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeTerminated, MethodTerminate)
	if ev, _ := rec.first(EvTerminated); !ev.Time.Equal(at(0)) {
		t.Fatalf("con --force terminó a las %v, esperaba +0", ev.Time.Sub(epoch))
	}
	if rec.has(EvCtrlC) || rec.has(EvClose) {
		t.Fatalf("con --force no se pide cierre prolijo: %v", rec.kinds())
	}
	assertPort(t, rep, PortFree)
}

// Regla: sin consola ni ventanas no hay por dónde pedir el cierre: directo a
// terminar, sin esperar.
func TestInvSinCanalNoEspera(t *testing.T) {
	s, cfg, opts := escenario()
	nodeTarget(s) // sin consola ni ventanas (p. ej. lanzado DETACHED)
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	rep, rec := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeTerminated, MethodTerminate)
	if ev, _ := rec.first(EvTerminated); !ev.Time.Equal(at(0)) {
		t.Fatalf("terminó a las %v, esperaba +0 (no había a quién esperar)", ev.Time.Sub(epoch))
	}
}

// Regla: si el Ctrl+C se omite (consola propia o compartida) y no hay otra
// señal, tampoco se espera.
func TestInvCtrlCOmitidoNoEspera(t *testing.T) {
	for _, tc := range []struct {
		nombre  string
		console []uint32
	}{
		{"consola propia", []uint32{100, kpPID}},
		{"consola compartida", []uint32{100, 777}},
	} {
		t.Run(tc.nombre, func(t *testing.T) {
			s, cfg, opts := escenario()
			n := nodeTarget(s)
			n.console = tc.console
			n.ctrlCReacts = true
			before, pl := buildPlan(s, cfg, req, opts.Filter)
			rep, rec := exec(s, before, req, pl, cfg, opts)
			assertVictim(t, rep, OutcomeTerminated, MethodTerminate)
			if ev, _ := rec.first(EvTerminated); !ev.Time.Equal(at(0)) {
				t.Fatalf("terminó a las %v, esperaba +0", ev.Time.Sub(epoch))
			}
		})
	}
}

// Regla: la espera termina cuando salieron TODOS los señalados, no cuando
// vence el plazo; y el supervisor (la raíz) muere ANTES de que se termine a
// su hijo sin señal. npm (consola, cierra con Ctrl+C a los 300 ms) → worker
// (sin consola): el worker se termina a los 300 ms, no a los 3 s.
func TestInvEsperaSoloSenalados(t *testing.T) {
	s, cfg, opts := escenario()
	cfg.Tree = true
	root := s.add(&simProc{id: ID{PID: 100, Created: at(50)}, name: "npm.exe", parent: 500,
		console: []uint32{100}, ctrlCReacts: true, ctrlCAfter: 300 * time.Millisecond})
	s.add(&simProc{id: ID{PID: 110, Created: at(55)}, name: "worker.exe", parent: 100})
	s.sockets = append(s.sockets, tcpListen(3000, 110, at(60)))
	_ = root
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	// El worker es el dueño del socket; con --tree la raíz es el worker (no
	// su padre): se arma el plan a mano con npm como raíz para el caso.
	pl.Targets[0].Root = ID{PID: 100, Created: at(50)}
	pl.Targets[0].Victims = []ID{{PID: 100, Created: at(50)}, {PID: 110, Created: at(55)}}
	rep, rec := exec(s, before, req, pl, cfg, opts)

	var npmExit, workerTerm Event
	for _, e := range rec.evs {
		if e.Kind == EvExited && e.Victim.PID == 100 {
			npmExit = e
		}
		if e.Kind == EvTerminated && e.Victim.PID == 110 {
			workerTerm = e
		}
	}
	if !npmExit.Time.Equal(at(300)) || npmExit.Method != MethodCtrlC {
		t.Fatalf("npm salió a las %v con %v, esperaba +300 ms con Ctrl+C", npmExit.Time.Sub(epoch), npmExit.Method)
	}
	if !workerTerm.Time.Equal(at(300)) {
		t.Fatalf("el worker se terminó a las %v, esperaba +300 ms (apenas salió el único señalado)", workerTerm.Time.Sub(epoch))
	}
	if len(rep.Victims) != 2 || rep.Victims[1].Outcome != OutcomeTerminated {
		t.Fatalf("víctimas = %+v", rep.Victims)
	}
}

// Regla: el conjunto permitido del Ctrl+C va por IDENTIDAD. Si el shell que
// comparte la consola murió y su PID lo reusa otro proceso que se sumó a la
// consola, ese otro es un intruso aunque el número coincida: no se manda.
func TestInvCtrlCPorIdentidad(t *testing.T) {
	s, cfg, opts := escenario()
	cmd := s.add(&simProc{id: ID{PID: 300, Created: at(5)}, name: "cmd.exe", parent: 500})
	n := s.add(&simProc{id: ID{PID: 100, Created: at(50)}, name: "node.exe", parent: 300,
		console: []uint32{100, 300}, ctrlCReacts: true, ctrlCAfter: 100 * time.Millisecond})
	s.sockets = append(s.sockets, tcpListen(3000, 100, at(60)))
	before, pl := buildPlan(s, cfg, req, opts.Filter)

	// Caso feliz primero: el allow lleva las identidades de la cadena.
	rep, _ := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeClosed, MethodCtrlC)
	if len(s.ctrlCAllow) != 1 {
		t.Fatalf("esperaba un Ctrl+C, hubo %d", len(s.ctrlCAllow))
	}
	got := map[ID]bool{}
	for _, id := range s.ctrlCAllow[0] {
		got[id] = true
	}
	for _, want := range []ID{n.id, cmd.id, {PID: 500, Created: at(0)}} {
		if !got[want] {
			t.Fatalf("el allow %v no tiene a %v", s.ctrlCAllow[0], want)
		}
	}

	// Ahora el cmd muere antes del envío y su PID lo reusa un intruso nacido
	// después de la foto, adentro de la misma consola.
	s2, cfg2, opts2 := escenario()
	s2.add(&simProc{id: ID{PID: 300, Created: at(5)}, name: "cmd.exe", parent: 500})
	n2 := s2.add(&simProc{id: ID{PID: 100, Created: at(50)}, name: "node.exe", parent: 300,
		console: []uint32{100, 300}, ctrlCReacts: true, ctrlCAfter: 100 * time.Millisecond})
	s2.sockets = append(s2.sockets, tcpListen(3000, 100, at(60)))
	before2, pl2 := buildPlan(s2, cfg2, req, opts2.Filter)
	s2.procs[ID{PID: 300, Created: at(5)}].exited = true
	s2.add(&simProc{id: ID{PID: 300, Created: at(900)}, name: "ajeno.exe"})
	rep2, rec2 := exec(s2, before2, req, pl2, cfg2, opts2)
	if rec2.has(EvCtrlC) {
		t.Fatalf("con un PID reciclado en la consola NO se manda Ctrl+C: %v", rec2.kinds())
	}
	ev, ok := rec2.first(EvCtrlCSkipped)
	if !ok || ev.Err == nil {
		t.Fatalf("esperaba el Ctrl+C omitido con los intrusos: %v", rec2.kinds())
	}
	_ = n2
	assertVictim(t, rep2, OutcomeTerminated, MethodTerminate)
}

// Regla: un puerto con negativa (el kernel en el 445) queda "no se toca", NO
// "lo retomó otro proceso" (antes se lo confundía con un respawn porque nadie
// lo había atacado); y no se gasta ni un sueño de backoff esperando que se
// libere algo que nadie tocó.
func TestInvNegativaNoEsRespawn(t *testing.T) {
	s, cfg, opts := escenario()
	n := nodeTarget(s)
	n.console = []uint32{100}
	n.ctrlCReacts, n.ctrlCAfter = true, 50*time.Millisecond
	s.add(&simProc{id: ID{PID: 4, Created: at(0)}, name: "System"})
	s.sockets = append(s.sockets, tcpListen(445, 4, at(0)))
	requested := ports.Of(445, 3000)
	before, pl := buildPlan(s, cfg, requested, opts.Filter)
	if len(pl.Refusals) != 1 || len(pl.Targets) != 1 {
		t.Fatalf("plan = %+v", pl)
	}
	rep, _ := exec(s, before, requested, pl, cfg, opts)
	if len(rep.Ports) != 2 {
		t.Fatalf("puertos = %+v", rep.Ports)
	}
	p445, p3000 := rep.Ports[0], rep.Ports[1]
	if p445.Status != PortRefused || p445.Reason == "" {
		t.Fatalf("445 = %v (%q), esperaba 'no se toca' con motivo", p445.Status, p445.Reason)
	}
	if p3000.Status != PortFree {
		t.Fatalf("3000 = %v", p3000.Status)
	}
	if s.sleeps != 0 {
		t.Fatalf("durmió %d veces esperando un puerto que nadie tocó", s.sleeps)
	}
	if s.exclReads != 0 || s.probes != 0 {
		t.Fatalf("no hacía falta diagnosticar nada (netsh %d, sondeos %d)", s.exclReads, s.probes)
	}
}

// Regla: con --tree, si el antichain absorbió al dueño en su ancestro y ese
// dueño no se pudo cerrar, el puerto "sigue ocupado" por él: no es un respawn
// (el que lo tiene ya lo tenía antes).
func TestInvAbsorbidoNoEsRespawn(t *testing.T) {
	s, cfg, opts := escenario()
	cfg.Tree = true
	s.add(&simProc{id: ID{PID: 10, Created: at(1)}, name: "npm.exe", parent: 500})
	s.add(&simProc{id: ID{PID: 20, Created: at(2)}, name: "node.exe", parent: 10, openErr: ErrAccessDenied})
	s.sockets = append(s.sockets, tcpListen(3001, 10, at(3)), tcpListen(3000, 20, at(4)))
	requested := ports.Of(3000, 3001)
	before, pl := buildPlan(s, cfg, requested, opts.Filter)
	if len(pl.Targets) != 1 || pl.Targets[0].Root.PID != 10 {
		t.Fatalf("el antichain debía dejar a npm como única raíz: %+v", pl.Targets)
	}
	rep, _ := exec(s, before, requested, pl, cfg, opts)
	var p3000 PortResult
	for _, p := range rep.Ports {
		if p.Port == 3000 {
			p3000 = p
		}
	}
	if p3000.Status != PortBusy || p3000.Owner.PID != 20 || p3000.Reason != "acceso denegado" {
		t.Fatalf("3000 = %v dueño %d (%q), esperaba 'sigue ocupado' por node con acceso denegado", p3000.Status, p3000.Owner.PID, p3000.Reason)
	}
}

// Regla: la verificación espera lo justo. Si tras la salida la tabla le sigue
// atribuyendo el socket al muerto durante 50 ms (el kernel limpiando), se
// relee con backoff 10 → 20 → 40 ms y se declara libre en cuanto se asienta.
func TestInvVerificaEsperaAlMuertoReciente(t *testing.T) {
	s, cfg, opts := escenario()
	n := nodeTarget(s)
	n.console = []uint32{100}
	n.ctrlCReacts, n.ctrlCAfter = true, 420*time.Millisecond
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	s.afterFn = func(s *simOS) []ports.Socket {
		if s.now.Before(at(470)) {
			return []ports.Socket{tcpListen(3000, 100, at(60))} // resto que el kernel todavía no borró
		}
		return nil
	}
	rep, _ := exec(s, before, req, pl, cfg, opts)
	assertPort(t, rep, PortFree)
	if s.sleeps != 3 || !s.now.Equal(at(490)) {
		t.Fatalf("durmió %d veces y terminó a las %v; esperaba 3 sueños (10+20+40) y +490 ms", s.sleeps, s.now.Sub(epoch))
	}
}

// Regla: un respawn ya está asentado apenas aparece (lo tiene alguien vivo que
// no es víctima): no se esperan los 2 s de tope para darse cuenta.
func TestInvRespawnNoEspera(t *testing.T) {
	s, cfg, opts := escenario()
	s.add(&simProc{id: ID{PID: 10, Created: at(0)}, name: "nodemon.exe", parent: 500})
	n := s.add(&simProc{id: ID{PID: 100, Created: at(50)}, name: "node.exe", parent: 10,
		console: []uint32{100}, ctrlCReacts: true, ctrlCAfter: 200 * time.Millisecond})
	s.sockets = append(s.sockets, tcpListen(3000, 100, at(60)))
	_ = n
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	s.afterFn = func(*simOS) []ports.Socket { return []ports.Socket{tcpListen(3000, 101, at(1000))} }
	s.snapFn = func(*simOS) []Proc {
		return []Proc{
			{ID: ID{PID: 10, Created: at(0)}, Name: "nodemon.exe", Parent: 500},
			{ID: ID{PID: 101, Created: at(1000)}, Name: "node.exe", Parent: 10},
		}
	}
	rep, _ := exec(s, before, req, pl, cfg, opts)
	pr := assertPort(t, rep, PortRespawned)
	if pr.SupervisorName != "nodemon.exe" || pr.Supervisor.PID != 10 {
		t.Fatalf("supervisor = %q/%d", pr.SupervisorName, pr.Supervisor.PID)
	}
	if s.sleeps != 0 {
		t.Fatalf("durmió %d veces para descubrir un respawn que ya estaba a la vista", s.sleeps)
	}
}

// Regla: el diagnóstico de "sin dueño" lee los rangos excluidos UNA vez aunque
// haya muchos puertos, y con rangos grandes no sondea bind puerto por puerto.
func TestInvDiagnosticoMemoizadoYAcotado(t *testing.T) {
	s, cfg, opts := escenario()
	requested := ports.NewSet(ports.Range{Lo: 40000, Hi: 40000 + probeLimit}) // uno más que el tope
	before, pl := buildPlan(s, cfg, requested, opts.Filter)
	rep, _ := exec(s, before, requested, pl, cfg, opts)
	if len(rep.Ports) != requested.Len() {
		t.Fatalf("puertos = %d, esperaba %d", len(rep.Ports), requested.Len())
	}
	if s.exclReads != 1 {
		t.Fatalf("netsh se leyó %d veces, esperaba 1", s.exclReads)
	}
	if s.probes != 0 {
		t.Fatalf("con %d puertos no se sondea bind (hubo %d)", requested.Len(), s.probes)
	}

	s2, cfg2, opts2 := escenario()
	small := ports.NewSet(ports.Range{Lo: 40000, Hi: 40009})
	s2.binds[40005] = ports.BindForbidden
	before2, pl2 := buildPlan(s2, cfg2, small, opts2.Filter)
	rep2, _ := exec(s2, before2, small, pl2, cfg2, opts2)
	for _, p := range rep2.Ports {
		want := PortFree
		if p.Port == 40005 {
			want = PortReserved
		}
		if p.Status != want {
			t.Fatalf("%d = %v, esperaba %v", p.Port, p.Status, want)
		}
	}
	if s2.exclReads != 1 || s2.probes == 0 {
		t.Fatalf("netsh %d veces, sondeos %d", s2.exclReads, s2.probes)
	}
}

// Regla: la identidad es el instante, no su representación. Sobre instantes
// al azar, en cualquier ubicación y con o sin lectura monotónica, el bosque
// encuentra el proceso y == entre formas canónicas coincide con Equal.
func TestInvIdentidadCanonica(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 7))
	zonas := []*time.Location{time.UTC, time.Local, time.FixedZone("ART", -3*3600), time.FixedZone("X", 5*3600+1800)}
	for i := 0; i < 500; i++ {
		base := time.Unix(1_700_000_000+rng.Int64N(100_000_000), rng.Int64N(1e9)/100*100)
		pid := uint32(rng.IntN(1 << 20))
		// Dos representaciones del MISMO instante: una en una zona al azar, la
		// otra con lectura monotónica (now.Add(base.Sub(now)) cae exacto en
		// base, pero arrastra la monotónica de now) o en otra zona.
		guardada := base.In(zonas[rng.IntN(len(zonas))])
		variante := base.In(zonas[rng.IntN(len(zonas))])
		if rng.IntN(2) == 0 {
			now := time.Now()
			variante = now.Add(base.Sub(now))
		}
		if !variante.Equal(base) {
			t.Fatalf("iter %d: el armado de la variante está mal", i)
		}
		f := NewForest([]Proc{{ID: ID{PID: pid, Created: guardada}, Name: "x.exe"}})
		if _, ok := f.Lookup(ID{PID: pid, Created: variante}); !ok {
			t.Fatalf("iter %d: no encontró %d@%v (guardado %v)", i, pid, variante, f.All()[0].Created)
		}
		a, b := (ID{PID: pid, Created: base}).Canon(), (ID{PID: pid, Created: variante}).Canon()
		if (a == b) != a.Created.Equal(b.Created) {
			t.Fatalf("iter %d: == (%v) y Equal (%v) no coinciden", i, a == b, a.Created.Equal(b.Created))
		}
	}
}

// Regla: dentro de un svchost se nombra el servicio EXACTO del socket (lo que
// informa la pila TCP/IP), no la lista entera de servicios del host.
func TestInvSvchostServicioExacto(t *testing.T) {
	f := NewForest(con(svchostReal(800, 5)))
	sock := tcpListen(135, 800, at(10))
	sock.Service = "RpcSs"
	svc := map[uint32][]Service{800: {{Name: "RpcEptMapper"}, {Name: "RpcSs"}}}
	pl := PlanKill(ports.Of(135), []ports.Socket{sock}, f, svc, Config{SystemRoot: `C:\Windows`})
	if len(pl.Refusals) != 1 {
		t.Fatalf("plan = %+v", pl)
	}
	r := pl.Refusals[0]
	if want := "lo escucha el servicio de Windows RpcSs (dentro de svchost)"; r.Reason != want {
		t.Fatalf("motivo = %q, esperaba %q", r.Reason, want)
	}
	// Sin el dato del socket, se nombran todos los del host.
	sock.Service = ""
	pl = PlanKill(ports.Of(135), []ports.Socket{sock}, f, svc, Config{SystemRoot: `C:\Windows`})
	if want := "lo escucha el servicio de Windows RpcEptMapper, RpcSs (dentro de svchost)"; pl.Refusals[0].Reason != want {
		t.Fatalf("motivo = %q, esperaba %q", pl.Refusals[0].Reason, want)
	}
	// Un nombre de ejecutable no es un servicio.
	sock.Service = "svchost.exe"
	pl = PlanKill(ports.Of(135), []ports.Socket{sock}, f, svc, Config{SystemRoot: `C:\Windows`})
	if want := "lo escucha el servicio de Windows RpcEptMapper, RpcSs (dentro de svchost)"; pl.Refusals[0].Reason != want {
		t.Fatalf("motivo = %q, esperaba %q", pl.Refusals[0].Reason, want)
	}
}

// Regla (propiedad): sobre escenarios al azar, TODA víctima termina con un
// destino final (salvo cancelación), nadie se termina en seco sin que haya
// vencido su oportunidad, y el reloj virtual nunca pasa de timeout+grace+2 s.
func TestInvEscaladaPropiedad(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 31))
	for iter := 0; iter < 300; iter++ {
		s, cfg, opts := escenario()
		cfg.Force = rng.IntN(5) == 0
		k := 1 + rng.IntN(5)
		requested := ports.Of()
		for i := 0; i < k; i++ {
			pid := uint32(100 + i)
			p := s.add(&simProc{id: ID{PID: pid, Created: at(20 + i)}, name: "app.exe", parent: 500})
			switch rng.IntN(4) {
			case 0: // consola propia, reacciona
				p.console = []uint32{pid}
				p.ctrlCReacts, p.ctrlCAfter = true, time.Duration(rng.IntN(5000))*time.Millisecond
			case 1: // ventana
				p.windows, p.closeAfter = 1, time.Duration(rng.IntN(5000))*time.Millisecond
			case 2: // sordo
				p.console = []uint32{pid}
			}
			if rng.IntN(8) == 0 {
				p.openErr = ErrAccessDenied
			}
			port := uint16(3000 + i)
			s.sockets = append(s.sockets, ports.Socket{Proto: ports.TCP, Local: netip.AddrPortFrom(netip.IPv4Unspecified(), port), State: ports.Listen, PID: pid, Since: at(30 + i)})
			requested = requested.Union(ports.Of(port))
		}
		before, pl := buildPlan(s, cfg, requested, opts.Filter)
		rep, rec := exec(s, before, requested, pl, cfg, opts)

		for _, v := range rep.Victims {
			if v.Outcome == OutcomePending {
				t.Fatalf("iter %d: víctima %v sin destino", iter, v.ID)
			}
		}
		// Terminación en seco solo tras el plazo (o sin espera si nadie tenía
		// señal): nunca en medio de una espera prolija que todavía corría.
		var waitEnd time.Time
		for _, e := range rec.evs {
			if e.Kind == EvWaiting && e.Method != MethodTerminate {
				waitEnd = e.Deadline
			}
			if e.Kind == EvTerminated && !waitEnd.IsZero() && e.Time.Before(waitEnd) {
				// Solo vale si ya no quedaba ningún señalado vivo.
				for _, v := range rep.Victims {
					if v.Method == MethodCtrlC || v.Method == MethodClose {
						if ex, ok := exitTime(rec, v.ID); ok && ex.After(e.Time) {
							t.Fatalf("iter %d: terminó a %v en seco a las %v con %v todavía cerrando prolijo", iter, e.Victim, e.Time.Sub(epoch), v.ID)
						}
					}
				}
			}
		}
		if limit := at(0).Add(opts.Timeout + opts.TerminateGrace + 2*time.Second); s.now.After(limit) {
			t.Fatalf("iter %d: el reloj llegó a %v", iter, s.now.Sub(epoch))
		}
	}
}

// Regla: con Settle > 0, un puerto que se libera pero un supervisor RECUPERA
// dentro de la ventana de confirmación se informa como respawn, no como libre.
// Es la queja clásica: "lo maté y el puerto sigue ocupado". Sin Settle se
// declararía libre en el instante en que parpadeó vacío.
func TestInvSettleAtrapaRespawn(t *testing.T) {
	s, cfg, opts := escenario()
	opts.Settle = 300 * time.Millisecond
	s.add(&simProc{id: ID{PID: 10, Created: at(0)}, name: "nodemon.exe", parent: 500})
	n := s.add(&simProc{id: ID{PID: 100, Created: at(50)}, name: "node.exe", parent: 10,
		console: []uint32{100}, ctrlCReacts: true, ctrlCAfter: 100 * time.Millisecond})
	s.sockets = append(s.sockets, tcpListen(3000, 100, at(60)))
	_ = n
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	// El puerto queda libre al cerrar (~100 ms) y nodemon lo relanza a los
	// +260 ms (dentro de la ventana de 300 ms de confirmacion).
	s.afterFn = func(s *simOS) []ports.Socket {
		if s.now.Before(at(260)) {
			return nil // libre un rato
		}
		return []ports.Socket{tcpListen(3000, 101, at(260))}
	}
	s.snapFn = func(*simOS) []Proc {
		return []Proc{
			{ID: ID{PID: 10, Created: at(0)}, Name: "nodemon.exe", Parent: 500},
			{ID: ID{PID: 101, Created: at(260)}, Name: "node.exe", Parent: 10},
		}
	}
	rep, _ := exec(s, before, req, pl, cfg, opts)
	pr := assertPort(t, rep, PortRespawned)
	if pr.SupervisorName != "nodemon.exe" {
		t.Fatalf("supervisor = %q, esperaba nodemon.exe", pr.SupervisorName)
	}
	if s.sleeps == 0 {
		t.Fatal("con Settle debió confirmar releyendo (durmió 0 veces)")
	}
}

// Regla: con Settle > 0 pero sin respawn, el puerto libre se confirma tras la
// ventana y se declara libre (no se cuelga esperando de más).
func TestInvSettleConfirmaLibre(t *testing.T) {
	s, cfg, opts := escenario()
	opts.Settle = 300 * time.Millisecond
	n := nodeTarget(s)
	n.console = []uint32{100}
	n.ctrlCReacts, n.ctrlCAfter = true, 100*time.Millisecond
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	rep, _ := exec(s, before, req, pl, cfg, opts)
	assertPort(t, rep, PortFree)
	// Cerró a los 100 ms; la confirmación agrega ~300 ms de relecturas.
	if s.now.Before(at(400)) {
		t.Fatalf("terminó a las %v; esperaba confirmar ~300 ms tras el cierre", s.now.Sub(epoch))
	}
}

// Regla (regresión del cuelgue e2e): si el adaptador devuelve la identidad de
// la salida en OTRA representación del mismo instante (zona local, como el
// FILETIME→time.Unix real), el motor igual la reencuentra en su tabla y la
// espera termina. Sin canonizar la clave, giraría en falso hasta el plazo.
func TestInvIdentidadNoCanonicaEnWait(t *testing.T) {
	s, cfg, opts := escenario()
	s.skewWait = true
	n := nodeTarget(s)
	n.console = []uint32{100}
	n.ctrlCReacts, n.ctrlCAfter = true, 420*time.Millisecond
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	rep, _ := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeClosed, MethodCtrlC)
	if !s.now.Equal(at(420)) {
		t.Fatalf("cerró a las %v, esperaba +420 ms (no debió girar hasta el plazo)", s.now.Sub(epoch))
	}
	assertPort(t, rep, PortFree)
}

func exitTime(rec *recorder, id ID) (time.Time, bool) {
	for _, e := range rec.evs {
		if e.Kind == EvExited && e.Victim == id {
			return e.Time, true
		}
	}
	return time.Time{}, false
}
