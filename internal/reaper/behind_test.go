package reaper

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Lo que hay detrás de los dos intermediarios de Windows mismo: http.sys (el
// kernel escucha en nombre del proceso que registró la URL) y el IP Helper
// (escucha por una regla de portproxy).

var systemID = ID{PID: 4, Created: at(0)}

func refusalOf(t *testing.T, pl Plan, port uint16) Refusal {
	t.Helper()
	for _, r := range pl.Refusals {
		if r.Ports.Contains(port) {
			return r
		}
	}
	t.Fatalf("no hay negativa para el %d: %+v", port, pl.Refusals)
	return Refusal{}
}

func targetOf(t *testing.T, pl Plan, port uint16) Target {
	t.Helper()
	for _, tg := range pl.Targets {
		if tg.Ports.Contains(port) {
			return tg
		}
	}
	t.Fatalf("no hay objetivo para el %d: %+v", port, pl.Targets)
	return Target{}
}

// El kernel: compartir archivos es suyo y no se desenmascara; lo demás que
// escucha por TCP puede ser http.sys, y eso sí. Nunca se aconseja detener ni
// deshabilitar el controlador HTTP.
func TestKernelSMBNoEsHTTP(t *testing.T) {
	f := NewForest(windows())
	socks := []ports.Socket{
		tcpListen(445, 4, at(5)), tcp6Listen(445, 4, at(5)), tcpListen(139, 4, at(5)),
		udpBound(137, 4, at(5)), udpBound(138, 4, at(5)),
		tcpListen(80, 4, at(5)), tcp6Listen(80, 4, at(5)), tcpListen(47880, 4, at(5)),
		udpBound(4500, 4, at(5)),
	}
	pl := PlanKill(ports.Of(80, 137, 138, 139, 445, 4500, 47880), socks, f, nil, Config{SystemRoot: winRoot})
	if len(pl.Targets) != 0 {
		t.Fatalf("el kernel nunca es objetivo: %+v", pl.Targets)
	}
	for _, p := range []uint16{445, 139} {
		r := refusalOf(t, pl, p)
		if !strings.Contains(r.Reason, "compartir archivos") || !strings.Contains(r.Reason, "SMB") || r.Relay != RelayNone {
			t.Errorf("%d = %+v: es compartir archivos, y no se desenmascara", p, r)
		}
	}
	for _, p := range []uint16{137, 138} {
		if r := refusalOf(t, pl, p); !strings.Contains(r.Reason, "NetBIOS") || r.Relay != RelayNone {
			t.Errorf("%d = %+v", p, r)
		}
	}
	for _, p := range []uint16{80, 47880} {
		if r := refusalOf(t, pl, p); r.Relay != RelayHTTP || !strings.Contains(r.Advice, "requestq") {
			t.Errorf("%d = %+v: puede ser http.sys", p, r)
		}
	}
	if r := refusalOf(t, pl, 4500); r.Relay != RelayNone {
		t.Errorf("un UDP del kernel no es http.sys: %+v", r)
	}
	for _, r := range pl.Refusals {
		for _, malo := range []string{"net stop http", "sc config http", "start= disabled", "start=disabled"} {
			if strings.Contains(strings.ToLower(r.Reason+r.Advice), malo) {
				t.Fatalf("la negativa aconseja %q: %+v", malo, r)
			}
		}
	}

	var got []string
	for _, m := range pl.Masked() {
		got = append(got, m.Relay.String()+" "+m.Proto.String()+"/"+itoa(uint32(m.Port)))
		if m.Via != systemID {
			t.Errorf("Via = %v", m.Via)
		}
	}
	want := "http.sys, el servidor HTTP de Windows TCP/80|http.sys, el servidor HTTP de Windows TCP/47880"
	if strings.Join(got, "|") != want {
		t.Fatalf("enmascarados = %q", strings.Join(got, "|"))
	}
}

// El svchost que aloja varios servicios cuenta una historia por servicio; la
// del IP Helper es una regla de portproxy (solo TCP) y nunca aconseja
// detenerlo.
func TestSvchostUnaNegativaPorServicio(t *testing.T) {
	f := NewForest(con(svchostReal(800, 5)))
	svc := map[uint32][]Service{800: {{Name: "iphlpsvc"}, {Name: "Schedule"}, {Name: "Winmgmt"}}}
	mk := func(s ports.Socket, service string) ports.Socket { s.Service = service; return s }
	socks := []ports.Socket{
		mk(tcpListen(3000, 800, at(10)), "iphlpsvc"), mk(tcpListen(8080, 800, at(10)), "IPHLPSVC"),
		mk(udpBound(3544, 800, at(10)), "iphlpsvc"),
		mk(tcpListen(49665, 800, at(10)), "Schedule"),
		mk(tcpListen(49700, 800, at(10)), ""),
	}
	pl := PlanKill(ports.Of(3000, 3544, 8080, 49665, 49700), socks, f, svc, Config{SystemRoot: winRoot})
	if len(pl.Targets) != 0 || len(pl.Refusals) != 4 {
		t.Fatalf("plan = %+v", pl)
	}
	regla := refusalOf(t, pl, 3000)
	if regla.Relay != RelayPortProxy || !regla.Ports.Equal(ports.Of(3000, 8080)) || !strings.Contains(regla.Reason, "portproxy") {
		t.Fatalf("la negativa del IP Helper = %+v", regla)
	}
	if tunel := refusalOf(t, pl, 3544); tunel.Relay != RelayNone || !strings.Contains(tunel.Reason, "IPv6") {
		t.Fatalf("el UDP del IP Helper no es una regla: %+v", tunel)
	}
	if r := refusalOf(t, pl, 49665); r.Relay != RelayNone || r.Service != "Schedule" || !strings.Contains(r.Advice, "net stop Schedule") {
		t.Fatalf("Schedule = %+v", r)
	}
	if r := refusalOf(t, pl, 49700); !strings.Contains(r.Reason, "Schedule, Winmgmt, iphlpsvc") {
		t.Fatalf("sin el dato del socket se nombran todos: %+v", r)
	}
	for _, r := range pl.Refusals {
		if strings.Contains(strings.ToLower(r.Advice), "net stop iphlpsvc") {
			t.Fatalf("aconseja detener el IP Helper: %+v", r)
		}
	}
	if m := pl.Masked(); len(m) != 2 || m[0].Relay != RelayPortProxy || m[0].Port != 3000 || m[1].Port != 8080 {
		t.Fatalf("enmascarados = %+v", m)
	}
}

// http.sys de punta a punta en el plan: detrás del 80 hay un proceso común,
// que pasa a ser el objetivo con el kernel como Via.
func TestUnmaskProcesoDetrasDeHTTP(t *testing.T) {
	node := Proc{ID: ID{PID: 900, Created: at(20)}, Name: "node.exe", Parent: 500, Path: `C:\proy\node.exe`}
	f := NewForest(con(proc(500, 4, "explorer.exe", 0), node, proc(901, 25, "worker.exe", 900)))
	socks := []ports.Socket{tcpListen(80, 4, at(30)), tcp6Listen(80, 4, at(30)), tcpListen(445, 4, at(5))}
	for _, tree := range []bool{false, true} {
		pl := PlanKill(ports.Of(80, 445), socks, f, nil, Config{SystemRoot: winRoot, Tree: tree})
		got := Unmask(pl, []Unmasking{{Via: systemID, Port: 80, Proto: ports.TCP, Verdict: UnmaskFound, Procs: []ID{node.ID, node.ID}}})
		if len(got.Targets) != 1 || len(got.Refusals) != 1 {
			t.Fatalf("tree=%v: plan = %+v", tree, got)
		}
		tg := got.Targets[0]
		if tg.Kind != KillProcess || tg.Root != node.ID || tg.Via != systemID || tg.ViaName != "System" || tg.ViaClass != RelayHTTP ||
			!tg.Ports.Equal(ports.Of(80)) || len(tg.Sockets) != 2 {
			t.Fatalf("tree=%v: objetivo = %+v", tree, tg)
		}
		wantVictims := 1
		if tree {
			wantVictims = 2
		}
		if len(tg.Victims) != wantVictims || tg.Victims[0] != node.ID {
			t.Fatalf("tree=%v: víctimas = %v", tree, tg.Victims)
		}
		if r := got.Refusals[0]; !r.Ports.Equal(ports.Of(445)) {
			t.Fatalf("el 445 sigue negado: %+v", r)
		}
		// El plan de entrada no cambió.
		if len(pl.Targets) != 0 || len(pl.Refusals) != 2 {
			t.Fatalf("Unmask tocó el plan que recibió: %+v", pl)
		}
	}
}

// Detrás de http.sys valen TODAS las reglas de la planificación: un svchost
// genuino queda negado (con el kernel anotado como intermediario), la
// terminal de killport también, un servicio de terceros se detiene, y un
// proceso que ya escuchaba otro puerto por su cuenta es UN solo objetivo.
func TestUnmaskDetrasDeHTTPValenLasReglas(t *testing.T) {
	self := ID{PID: 600, Created: at(12)}
	ssrs := Proc{ID: ID{PID: 700, Created: at(8)}, Name: "ReportingServicesService.exe", Parent: pidServices, Path: `C:\Program Files\SSRS\ReportingServicesService.exe`}
	node := Proc{ID: ID{PID: 900, Created: at(20)}, Name: "node.exe", Parent: 500, Path: `C:\proy\node.exe`}
	f := NewForest(con(proc(500, 4, "pwsh.exe", 0), Proc{ID: self, Name: "killport.exe", Parent: 500}, svchostReal(800, 6), ssrs, node))
	svc := map[uint32][]Service{800: {{Name: "WinRM"}}, 700: {{Name: "SQLServerReportingServices"}}}
	socks := []ports.Socket{
		tcpListen(80, 4, at(30)), tcpListen(443, 4, at(30)), tcpListen(5985, 4, at(30)), tcpListen(8000, 4, at(30)), tcpListen(8001, 4, at(30)),
		tcpListen(3000, 900, at(30)),
	}
	pl := PlanKill(ports.Of(80, 443, 3000, 5985, 8000, 8001), socks, f, svc, Config{Self: self, SystemRoot: winRoot})
	answer := func(port uint16, ids ...ID) Unmasking {
		return Unmasking{Via: systemID, Port: port, Proto: ports.TCP, Verdict: UnmaskFound, Procs: ids}
	}
	got := Unmask(pl, []Unmasking{
		answer(80, ssrs.ID), answer(443, ssrs.ID),
		answer(5985, ID{PID: 800, Created: at(6)}),
		answer(8000, ID{PID: 500, Created: at(4)}),
		answer(8001, node.ID),
	})

	svcT := targetOf(t, got, 80)
	if svcT.Kind != StopThirdSvc || svcT.Service != "SQLServerReportingServices" || !svcT.Ports.Equal(ports.Of(80, 443)) || svcT.Via != systemID {
		t.Fatalf("SSRS = %+v", svcT)
	}
	nodeT := targetOf(t, got, 3000)
	if nodeT.Root != node.ID || !nodeT.Ports.Equal(ports.Of(3000, 8001)) || len(nodeT.Victims) != 1 || nodeT.Via != systemID {
		t.Fatalf("node (puerto propio + URL en http.sys) = %+v", nodeT)
	}
	if len(got.Targets) != 2 {
		t.Fatalf("objetivos = %+v", got.Targets)
	}
	winrm := refusalOf(t, got, 5985)
	if winrm.PID != 800 || winrm.Via != systemID || winrm.ViaName != "System" || winrm.Relay != RelayNone || !strings.Contains(winrm.Reason, "WinRM") {
		t.Fatalf("WinRM = %+v", winrm)
	}
	if shell := refusalOf(t, got, 8000); shell.PID != 500 || !strings.Contains(shell.Reason, "terminal") || shell.Via != systemID {
		t.Fatalf("la terminal = %+v", shell)
	}
	// Una negativa firme no se vuelve a desenmascarar.
	if m := got.Masked(); len(m) != 0 {
		t.Fatalf("quedó algo por desenmascarar: %+v", m)
	}
	again := Unmask(got, []Unmasking{{Via: ID{PID: 800, Created: at(6)}, Port: 5985, Proto: ports.TCP, Verdict: UnmaskEmpty, KillVia: true, Reason: "x"}})
	if len(again.Targets) != 2 {
		t.Fatalf("una segunda pasada volvió objetivo a un svchost: %+v", again.Targets)
	}
}

// Identidades que no están en la instantánea (el proceso salió, o el PID es
// de otro), el kernel detrás de sí mismo, y un plan armado a mano: nada de
// eso entra al plan. El puerto queda negado como estaba.
func TestUnmaskProcsQueNoEntran(t *testing.T) {
	node := Proc{ID: ID{PID: 900, Created: at(20)}, Name: "node.exe", Parent: 500, Path: `C:\proy\node.exe`}
	f := NewForest(con(proc(500, 4, "explorer.exe", 0), node))
	socks := []ports.Socket{tcpListen(80, 4, at(30))}
	pl := PlanKill(ports.Of(80), socks, f, nil, Config{SystemRoot: winRoot})
	for nombre, ids := range map[string][]ID{
		"otra hora de creación": {{PID: 900, Created: at(21)}},
		"PID que no existe":     {{PID: 12345, Created: at(1)}},
		"el kernel":             {systemID},
		"sin identidad":         {{}},
	} {
		got := Unmask(pl, []Unmasking{{Via: systemID, Port: 80, Proto: ports.TCP, Verdict: UnmaskFound, Procs: ids}})
		if len(got.Targets) != 0 || len(got.Refusals) != 1 || got.Refusals[0].Reason != pl.Refusals[0].Reason || got.Refusals[0].Relay != RelayHTTP {
			t.Errorf("%s: plan = %+v", nombre, got)
		}
	}
	// Un plan que no salió de PlanKill no recuerda su mundo.
	mano := Plan{Refusals: pl.Refusals}
	got := Unmask(mano, []Unmasking{{Via: systemID, Port: 80, Proto: ports.TCP, Verdict: UnmaskFound, Procs: []ID{node.ID}}})
	if len(got.Targets) != 0 || len(got.Refusals) != 1 {
		t.Fatalf("sin mundo no se clasifica: %+v", got)
	}
}

// Con --tree el antichain se rehace al desenmascarar: si el proceso que está
// detrás de http.sys desciende de otro objetivo, se absorbe; y si es ancestro
// de uno, lo absorbe él.
func TestUnmaskAntichain(t *testing.T) {
	f := NewForest(con(
		proc(500, 4, "explorer.exe", 0),
		proc(20, 10, "npm.exe", 500),
		proc(21, 11, "node.exe", 20),
		proc(22, 12, "api.exe", 21),
	))
	// node (21) escucha el 3000; api (22), su hijo, tiene una URL en el 8080.
	socks := []ports.Socket{tcpListen(3000, 21, at(30)), tcpListen(8080, 4, at(30)), tcpListen(9090, 4, at(30))}
	pl := PlanKill(ports.Of(3000, 8080, 9090), socks, f, nil, Config{SystemRoot: winRoot, Tree: true})
	got := Unmask(pl, []Unmasking{
		{Via: systemID, Port: 8080, Proto: ports.TCP, Verdict: UnmaskFound, Procs: []ID{{PID: 22, Created: at(12)}}},
		{Via: systemID, Port: 9090, Proto: ports.TCP, Verdict: UnmaskFound, Procs: []ID{{PID: 20, Created: at(10)}}},
	})
	if len(got.Targets) != 1 || len(got.Refusals) != 0 {
		t.Fatalf("plan = %+v", got)
	}
	tg := got.Targets[0]
	if tg.Root.PID != 20 || !tg.Ports.Equal(ports.Of(3000, 8080, 9090)) || len(tg.Victims) != 3 {
		t.Fatalf("objetivo = %+v", tg)
	}
	seen := map[ID]bool{}
	for _, v := range got.Victims() {
		if seen[v] {
			t.Fatalf("la víctima %v aparece dos veces", v)
		}
		seen[v] = true
	}
}

// La regla de portproxy es un externo como cualquier otro: objetivo
// StopExternal con el svchost del IP Helper como Via.
func TestUnmaskReglaDePortproxy(t *testing.T) {
	f := NewForest(con(svchostReal(800, 5)))
	sk := tcpListen(3000, 800, at(10))
	sk.Service = "iphlpsvc"
	pl := PlanKill(ports.Of(3000), []ports.Socket{sk}, f, map[uint32][]Service{800: {{Name: "iphlpsvc"}}}, Config{SystemRoot: winRoot})
	regla := External{Kind: ExtRule, Where: "portproxy", Key: "v4tov4/tcp/0.0.0.0/3000", Name: "0.0.0.0:3000 → 172.24.1.5:3000", Ports: ports.Of(3000)}
	got := Unmask(pl, []Unmasking{{Via: ID{PID: 800, Created: at(5)}, Port: 3000, Proto: ports.TCP, Verdict: UnmaskFound, Behind: []External{regla}}})
	if len(got.Targets) != 1 || len(got.Refusals) != 0 {
		t.Fatalf("plan = %+v", got)
	}
	tg := got.Targets[0]
	if tg.Kind != StopExternal || tg.External.Key != regla.Key || tg.ViaName != "svchost.exe" || tg.ViaClass != RelayPortProxy || len(tg.Victims) != 0 {
		t.Fatalf("objetivo = %+v", tg)
	}
	if got := regla.Label(); got != "la regla de portproxy 0.0.0.0:3000 → 172.24.1.5:3000" {
		t.Fatalf("Label = %q", got)
	}
	if ExtRule.home() != RelayPortProxy || ExtRule.String() == "" {
		t.Fatal("el dominio de la regla es el portproxy")
	}
}

// escenarioHTTP: node (PID 900) tiene una URL en el 8080 de http.sys, y el
// kernel tiene además el 445, que también se pidió.
func escenarioHTTP(t *testing.T) (*simOS, Config, Options, *Forest, Plan, ports.Set, *simProc) {
	t.Helper()
	s, cfg, opts := escenario()
	for _, p := range windows() {
		s.add(&simProc{id: p.ID, name: p.Name, parent: p.Parent, path: p.Path})
	}
	node := s.add(&simProc{id: ID{PID: 900, Created: at(50)}, name: "node.exe", parent: 500, path: `C:\proy\node.exe`})
	s.sockets = append(s.sockets, tcpListen(8080, 4, at(60)), tcpListen(445, 4, at(5)))
	requested := ports.Of(445, 8080)
	before, pl := buildPlan(s, cfg, requested, opts.Filter)
	pl = Unmask(pl, []Unmasking{{Via: systemID, Port: 8080, Proto: ports.TCP, Verdict: UnmaskFound, Procs: []ID{node.id}}})
	if len(pl.Targets) != 1 || len(pl.Refusals) != 1 {
		t.Fatalf("plan = %+v", pl)
	}
	return s, cfg, opts, before, pl, requested, node
}

// El proceso de atrás cierra y http.sys suelta el puerto un instante después:
// la verificación lo espera (sin esperar al 445, que el kernel no va a soltar
// nunca) y el puerto queda libre.
func TestEscHTTPSueltaDespues(t *testing.T) {
	s, cfg, opts, before, pl, requested, node := escenarioHTTP(t)
	node.console, node.ctrlCReacts, node.ctrlCAfter = []uint32{900}, true, 200*time.Millisecond
	suelta := 120 * time.Millisecond
	s.afterFn = func(s *simOS) []ports.Socket {
		out := []ports.Socket{tcpListen(445, 4, at(5))}
		if !node.exited || s.now.Before(node.exitAt.Add(suelta)) {
			out = append(out, tcpListen(8080, 4, at(60)))
		}
		return out
	}
	start := s.now
	rep, _ := exec(s, before, requested, pl, cfg, opts)
	assertVictim(t, rep, OutcomeClosed, MethodCtrlC)
	if p := portOf(rep, 8080); p.Status != PortFree {
		t.Fatalf("8080 = %+v", p)
	}
	if p := portOf(rep, 445); p.Status != PortRefused || !strings.Contains(p.Reason, "compartir archivos") {
		t.Fatalf("445 = %+v", p)
	}
	if tardo := s.now.Sub(start); tardo < 200*time.Millisecond+suelta || tardo > time.Second {
		t.Fatalf("tardó %v: tiene que esperar a http.sys y no al 445", tardo)
	}
}

// El proceso de atrás NO se pudo cerrar (acceso denegado): http.sys no va a
// soltar nada, y esperarlo sería regalar los dos segundos del tope. El puerto
// sigue ocupado y se dice por qué, sin dormir ni una vez.
func TestEscHTTPDenegadoNoEspera(t *testing.T) {
	s, cfg, opts, before, pl, requested, node := escenarioHTTP(t)
	node.openErr = ErrAccessDenied
	s.afterFn = func(*simOS) []ports.Socket {
		return []ports.Socket{tcpListen(445, 4, at(5)), tcpListen(8080, 4, at(60))}
	}
	start := s.now
	rep, _ := exec(s, before, requested, pl, cfg, opts)
	assertVictim(t, rep, OutcomeDenied, MethodNone)
	p := portOf(rep, 8080)
	if p.Status != PortBusy || p.Reason != "acceso denegado" || p.Owner.PID != 4 {
		t.Fatalf("8080 = %+v", p)
	}
	if s.sleeps != 0 || !s.now.Equal(start) {
		t.Fatalf("durmió %d veces (%v) esperando a http.sys, que no tiene por qué soltar el puerto", s.sleeps, s.now.Sub(start))
	}
}

// Solo mirar: el puerto lo tiene el proceso de atrás, a través del kernel.
func TestVerifyNombraAlQueEstaDetrasDeHTTP(t *testing.T) {
	s, _, opts, before, pl, requested, _ := escenarioHTTP(t)
	res := Verify(ctx, s, before, requested, pl, Report{}, opts)
	var p8080 PortResult
	for _, r := range res {
		if r.Port == 8080 {
			p8080 = r
		}
	}
	if p8080.Status != PortBusy || !strings.Contains(p8080.Reason, "node.exe (PID 900)") || !strings.Contains(p8080.Reason, "System") {
		t.Fatalf("8080 = %+v", p8080)
	}
}

// El proceso cerró pero en ese puerto hay OTRA URL registrada: http.sys sigue
// escuchando, y se dice por qué.
func TestEscHTTPOtraURLEnElMismoPuerto(t *testing.T) {
	s, cfg, opts, before, pl, requested, node := escenarioHTTP(t)
	node.console, node.ctrlCReacts, node.ctrlCAfter = []uint32{900}, true, 100*time.Millisecond
	s.afterFn = func(*simOS) []ports.Socket {
		return []ports.Socket{tcpListen(445, 4, at(5)), tcpListen(8080, 4, at(60))}
	}
	rep, _ := exec(s, before, requested, pl, cfg, opts)
	assertVictim(t, rep, OutcomeClosed, MethodCtrlC)
	p := portOf(rep, 8080)
	if p.Status != PortBusy || !strings.Contains(p.Reason, "node.exe cerró") || !strings.Contains(p.Reason, "otro proceso") || p.Advice == "" {
		t.Fatalf("8080 = %+v", p)
	}
}

// Detrás del mismo puerto hay un objetivo y un servicio que no se toca: se
// cierra el primero, y el veredicto del puerto es la negativa del segundo.
func TestEscHTTPObjetivoYNegativaEnElMismoPuerto(t *testing.T) {
	s, cfg, opts := escenario()
	for _, p := range con(svchostReal(800, 6)) {
		s.add(&simProc{id: p.ID, name: p.Name, parent: p.Parent, path: p.Path})
	}
	node := s.add(&simProc{id: ID{PID: 900, Created: at(50)}, name: "node.exe", parent: 500, path: `C:\proy\node.exe`})
	s.svc["WinRM"] = &simSvc{name: "WinRM", ownerPID: 800}
	s.sockets = append(s.sockets, tcpListen(5985, 4, at(60)))
	requested := ports.Of(5985)
	before, pl := buildPlan(s, cfg, requested, opts.Filter)
	pl = Unmask(pl, []Unmasking{{Via: systemID, Port: 5985, Proto: ports.TCP, Verdict: UnmaskFound, Procs: []ID{node.id, {PID: 800, Created: at(6)}}}})
	if len(pl.Targets) != 1 || len(pl.Refusals) != 1 {
		t.Fatalf("plan = %+v", pl)
	}
	s.afterFn = func(*simOS) []ports.Socket { return []ports.Socket{tcpListen(5985, 4, at(60))} }
	rep, _ := exec(s, before, requested, pl, cfg, opts)
	if len(rep.Victims) != 1 || !rep.Victims[0].Outcome.Done() {
		t.Fatalf("víctimas = %+v", rep.Victims)
	}
	p := portOf(rep, 5985)
	if p.Status != PortRefused || !strings.Contains(p.Reason, "svchost.exe") || !strings.Contains(p.Reason, "WinRM") || !strings.Contains(p.Reason, "a través de System") {
		t.Fatalf("5985 = %+v", p)
	}
}

// Propiedad: detrás de http.sys, sobre bosques al azar, lo que Unmask vuelve
// objetivo es EXACTAMENTE lo que PlanKill habría vuelto objetivo si ese
// proceso fuera el dueño directo del socket. No hay una segunda vara.
func TestUnmaskMismaVaraQuePlanKill(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 33))
	nombres := []string{"app.exe", "svchost.exe", "lsass.exe", "com.docker.backend.exe", "Code.exe", "wslrelay.exe", "w3wp.exe"}
	for iter := 0; iter < 400; iter++ {
		self := ID{PID: 600, Created: at(10)}
		procs := con(proc(500, 4, "pwsh.exe", 0), Proc{ID: self, Name: "killport.exe", Parent: 500})
		svc := map[uint32][]Service{}
		n := 4 + rng.IntN(10)
		for i := 0; i < n; i++ {
			pid := uint32(1000 + i)
			p := proc(pid, 100+i, nombres[rng.IntN(len(nombres))], procs[rng.IntN(len(procs))].PID)
			switch rng.IntN(3) {
			case 0:
				p.Path = winRoot + `\System32\` + p.Name
			case 1:
				p.Path = `C:\Program Files\x\` + p.Name
			}
			if rng.IntN(3) == 0 {
				svc[pid] = []Service{{Name: "Svc" + itoa(pid)}}
			}
			procs = append(procs, p)
		}
		f := NewForest(procs)
		cfg := Config{Self: self, SystemRoot: winRoot, Tree: rng.IntN(2) == 0}

		// Directo: cada proceso es dueño de su puerto. Detrás de http.sys: el
		// kernel es dueño de todos, y cada proceso registró el suyo.
		var directo, kernel []ports.Socket
		var answers []Unmasking
		pedidos := ports.Of()
		for i, p := range procs {
			if p.PID <= 4 {
				continue
			}
			port := uint16(2000 + i)
			pedidos = pedidos.Union(ports.Of(port))
			directo = append(directo, tcpListen(port, p.PID, at(5000)))
			kernel = append(kernel, tcpListen(port, 4, at(5000)))
			answers = append(answers, Unmasking{Via: systemID, Port: port, Proto: ports.TCP, Verdict: UnmaskFound, Procs: []ID{p.ID}})
		}
		a := PlanKill(pedidos, directo, f, svc, cfg)
		b := Unmask(PlanKill(pedidos, kernel, f, svc, cfg), answers)

		firma := func(pl Plan) string {
			var out []string
			for _, tg := range pl.Targets {
				var vs []string
				for _, v := range tg.Victims {
					vs = append(vs, itoa(v.PID))
				}
				out = append(out, "T "+itoa(tg.Root.PID)+" k"+itoa(uint32(tg.Kind))+" "+tg.Service+" "+tg.Ports.String()+" ["+strings.Join(vs, ",")+"]")
			}
			for _, r := range pl.Refusals {
				out = append(out, "R "+itoa(r.PID)+" "+r.Ports.String())
			}
			return strings.Join(out, "\n")
		}
		if firma(a) != firma(b) {
			t.Fatalf("iter %d (tree=%v): directo\n%s\n\ndetrás de http.sys\n%s", iter, cfg.Tree, firma(a), firma(b))
		}
		for _, tg := range b.Targets {
			if tg.Via != systemID || tg.ViaClass != RelayHTTP {
				t.Fatalf("iter %d: el objetivo %s no anota al kernel como intermediario: %+v", iter, tg.Name, tg)
			}
		}
		for _, r := range b.Refusals {
			if r.Via != systemID || r.Relay.Unmaskable() {
				t.Fatalf("iter %d: la negativa de %s = %+v", iter, r.Name, r)
			}
		}
	}
}

// Propiedad: RelayFor —la vara del listado general— dice de cada socket lo
// mismo que la planificación al negarlo. Si PlanKill marca un puerto como
// desenmascarable, el listado mira detrás de esa fila, y si no, no.
func TestRelayForCoincideConElPlan(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 77))
	nombres := []string{"app.exe", "svchost.exe", "lsass.exe", "com.docker.backend.exe", "Code.exe", "wslrelay.exe", "System"}
	servicios := []string{"iphlpsvc", "IPHLPSVC", "Schedule", "", "svchost.exe"}
	for iter := 0; iter < 400; iter++ {
		procs := con(svchostReal(800, 5), svchostReal(801, 6))
		svc := map[uint32][]Service{800: {{Name: "iphlpsvc"}, {Name: "Schedule"}}, 801: {{Name: "Dnscache"}}}
		for i := 0; i < 6; i++ {
			p := proc(uint32(1000+i), 100+i, nombres[rng.IntN(len(nombres))], 45)
			if rng.IntN(2) == 0 {
				p.Path = winRoot + `\System32\` + p.Name
			}
			procs = append(procs, p)
		}
		f := NewForest(procs)
		cfg := Config{SystemRoot: winRoot}

		var socks []ports.Socket
		pedidos := ports.Of()
		for i, p := range f.All() {
			port := uint16(130 + i*7 + rng.IntN(5)) // cae cerca del 137–139 y, con i grande, lejos
			if rng.IntN(4) == 0 {
				port = []uint16{80, 137, 138, 139, 445}[rng.IntN(5)]
			}
			s := tcpListen(port, p.PID, at(9000))
			if rng.IntN(3) == 0 {
				s = udpBound(port, p.PID, at(9000))
			}
			s.Service = servicios[rng.IntN(len(servicios))]
			socks = append(socks, s)
			pedidos = pedidos.Union(ports.Of(port))
		}
		pl := PlanKill(pedidos, socks, f, svc, cfg)

		type clave struct {
			pid   uint32
			port  uint16
			proto ports.Proto
		}
		delPlan := map[clave]Relay{}
		for _, r := range pl.Refusals {
			for _, s := range r.Sockets {
				delPlan[clave{s.PID, s.Port(), s.Proto}] = r.Relay
			}
		}
		for _, s := range socks {
			owner, _ := f.LivePID(s.PID)
			got := RelayFor(owner, s, f, svc, cfg)
			if want := delPlan[clave{s.PID, s.Port(), s.Proto}]; got != want {
				t.Fatalf("iter %d: %s (PID %d) %s/%d servicio %q: RelayFor = %v y el plan dice %v",
					iter, owner.Name, owner.PID, s.Proto, s.Port(), s.Service, got, want)
			}
		}
	}
}
