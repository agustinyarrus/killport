package reaper

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Dueño enmascarado → objetivo real: el protocolo Masked/Unmask (puro) y la
// escalada de externos en el simulador (peldaño 1b + verificación).

const relayPID = 300

var relayID = ID{PID: relayPID, Created: at(1)}

func tcp6Listen(port uint16, pid uint32, since time.Time) ports.Socket {
	return ports.Socket{Proto: ports.TCP, Local: netip.AddrPortFrom(netip.IPv6Unspecified(), port), State: ports.Listen, PID: pid, Since: since}
}

func udpBound(port uint16, pid uint32, since time.Time) ports.Socket {
	return ports.Socket{Proto: ports.UDP, Local: netip.AddrPortFrom(netip.IPv4Unspecified(), port), PID: pid, Since: since}
}

// relayPlan planifica con com.docker.backend (PID 300) sosteniendo los sockets dados.
func relayPlan(t *testing.T, name string, requested ports.Set, socks ...ports.Socket) (*Forest, Plan) {
	t.Helper()
	f := NewForest([]Proc{{ID: relayID, Name: name, Parent: 1, Path: `C:\Program Files\X\` + name}})
	return f, PlanKill(requested, socks, f, nil, Config{SystemRoot: `C:\Windows`})
}

func contenedor(key, name string, ps ...uint16) External {
	return External{Kind: ExtContainer, Where: "Docker Desktop", Key: key, Name: name, Ports: ports.Of(ps...)}
}

// Masked agrupa por (puerto, protocolo): v4 y v6 del mismo servidor son UN par.
func TestMaskedAgrupaPorPuertoYProto(t *testing.T) {
	_, pl := relayPlan(t, "com.docker.backend.exe", ports.NewSet(ports.R(1, 65535)),
		tcpListen(8888, relayPID, at(5)), tcpListen(5432, relayPID, at(5)), tcp6Listen(5432, relayPID, at(5)), udpBound(53, relayPID, at(5)))
	m := pl.Masked()
	if len(m) != 3 {
		t.Fatalf("pares = %+v", m)
	}
	want := []struct {
		port  uint16
		proto ports.Proto
		n     int
	}{{53, ports.UDP, 1}, {5432, ports.TCP, 2}, {8888, ports.TCP, 1}}
	for i, w := range want {
		if m[i].Port != w.port || m[i].Proto != w.proto || len(m[i].Sockets) != w.n || m[i].Via != relayID || m[i].Relay != RelayContainer {
			t.Fatalf("par %d = %+v, esperaba %d/%v con %d sockets", i, m[i], w.port, w.proto, w.n)
		}
	}
	// VS Code no se desenmascara: no expone a quién reenvía.
	_, pl = relayPlan(t, "Code.exe", ports.Of(3000), tcpListen(3000, relayPID, at(5)))
	if len(pl.Masked()) != 0 || len(pl.Refusals) != 1 {
		t.Fatalf("VS Code no tiene nada que desenmascarar: %+v", pl)
	}
}

// Found: un contenedor que publica dos puertos pedidos es UN objetivo con los
// dos, el relay queda como Via y ya no hay negativa.
func TestUnmaskEncontrado(t *testing.T) {
	_, pl := relayPlan(t, "com.docker.backend.exe", ports.Of(5432, 5433),
		tcpListen(5432, relayPID, at(5)), tcp6Listen(5432, relayPID, at(5)), tcpListen(5433, relayPID, at(5)))
	db := contenedor("c0ffee", "db-1", 5432, 5433)
	db.Caution = "tiene restart=always"
	pl = Unmask(pl, []Unmasking{
		{Via: relayID, Port: 5432, Proto: ports.TCP, Verdict: UnmaskFound, Behind: []External{db}},
		{Via: relayID, Port: 5433, Proto: ports.TCP, Verdict: UnmaskFound, Behind: []External{db}},
	})
	if len(pl.Refusals) != 0 || len(pl.Targets) != 1 {
		t.Fatalf("plan = %+v", pl)
	}
	tg := pl.Targets[0]
	if tg.Kind != StopExternal || tg.External.Key != "c0ffee" || !tg.Ports.Equal(ports.Of(5432, 5433)) {
		t.Fatalf("objetivo = %+v", tg)
	}
	if tg.Via != relayID || tg.ViaName != "com.docker.backend.exe" || tg.ViaClass != RelayContainer || len(tg.Victims) != 0 {
		t.Fatalf("el relay tiene que quedar como Via, sin víctimas de Windows: %+v", tg)
	}
	if len(tg.Sockets) != 3 || tg.Caution != "tiene restart=always" {
		t.Fatalf("sockets/caution = %d/%q", len(tg.Sockets), tg.Caution)
	}
	if len(pl.Victims()) != 0 {
		t.Fatalf("un externo no aporta víctimas de Windows: %v", pl.Victims())
	}
}

// Cada puerto cuenta su historia: el 5432 lo publica db-1 (objetivo), el 8888
// es un puerto interno de Docker Desktop (negativa con ese motivo), el 9000 no
// se pudo mirar (negativa con ese motivo) y el 7000 nadie lo contestó
// (negativa genérica de relay).
func TestUnmaskMezcla(t *testing.T) {
	_, pl := relayPlan(t, "com.docker.backend.exe", ports.Of(5432, 7000, 8888, 9000),
		tcpListen(5432, relayPID, at(5)), tcpListen(7000, relayPID, at(5)), tcpListen(8888, relayPID, at(5)), tcpListen(9000, relayPID, at(5)))
	generic := pl.Refusals[0].Reason
	pl = Unmask(pl, []Unmasking{
		{Via: relayID, Port: 5432, Proto: ports.TCP, Verdict: UnmaskFound, Behind: []External{contenedor("c0ffee", "db-1", 5432)}},
		{Via: relayID, Port: 8888, Proto: ports.TCP, Verdict: UnmaskEmpty, Reason: "ningún contenedor publica el 8888: es un puerto interno de Docker Desktop", Advice: "desactivalo en Docker Desktop"},
		{Via: relayID, Port: 9000, Proto: ports.TCP, Verdict: UnmaskUnavailable, Reason: "Docker no responde", Advice: "¿está corriendo Docker Desktop?"},
	})
	if len(pl.Targets) != 1 || pl.Targets[0].External.Key != "c0ffee" {
		t.Fatalf("objetivos = %+v", pl.Targets)
	}
	if len(pl.Refusals) != 3 {
		t.Fatalf("esperaba 3 negativas (7000 genérica, 8888 interno, 9000 sin motor): %+v", pl.Refusals)
	}
	byPort := map[uint16]Refusal{}
	for _, r := range pl.Refusals {
		if r.Ports.Len() != 1 {
			t.Fatalf("cada historia es un puerto: %+v", r)
		}
		p, _ := r.Ports.Min()
		byPort[p] = r
		if r.Relay != RelayContainer || r.Owner != relayID {
			t.Fatalf("la negativa sigue siendo del relay: %+v", r)
		}
	}
	if byPort[7000].Reason != generic || byPort[8888].Advice != "desactivalo en Docker Desktop" || byPort[9000].Reason != "Docker no responde" {
		t.Fatalf("motivos = %+v", byPort)
	}
}

// Relay zombi (Empty + KillVia): el intermediario mismo pasa a ser el
// objetivo, con el motivo y la advertencia que dio el dominio.
func TestUnmaskRelayZombi(t *testing.T) {
	_, pl := relayPlan(t, "wslrelay.exe", ports.Of(3000), tcpListen(3000, relayPID, at(5)))
	pl = Unmask(pl, []Unmasking{{Via: relayID, Port: 3000, Proto: ports.TCP, Verdict: UnmaskEmpty, KillVia: true,
		Reason: "relay zombi: en ninguna distro escucha nadie el 3000", Advice: "cerrarlo corta los reenvíos de esa VM"}})
	if len(pl.Refusals) != 0 || len(pl.Targets) != 1 {
		t.Fatalf("plan = %+v", pl)
	}
	tg := pl.Targets[0]
	if tg.Kind != KillProcess || tg.Root != relayID || len(tg.Victims) != 1 || tg.Victims[0] != relayID {
		t.Fatalf("el relay colgado es el objetivo: %+v", tg)
	}
	if tg.Reason == "" || tg.Caution == "" || tg.ViaClass != RelayWSL {
		t.Fatalf("motivo/advertencia/clase = %q/%q/%v", tg.Reason, tg.Caution, tg.ViaClass)
	}
}

// Unmask no es una puerta trasera: una respuesta que apunte a una negativa
// que NO es de un intermediario desenmascarable (el kernel, la terminal de
// killport, svchost, VS Code) no la convierte en objetivo, ni con Found ni con
// KillVia. Solo Relay.Unmaskable decide qué se puede reescribir.
func TestUnmaskSoloLoDesenmascarable(t *testing.T) {
	self := ID{PID: 600, Created: at(3)}
	f := NewForest(con(
		proc(500, 1, "pwsh.exe", 0),
		Proc{ID: self, Name: "killport.exe", Parent: 500},
		proc(700, 2, "Code.exe", 1),
		svchostReal(800, 4),
	))
	socks := []ports.Socket{tcpListen(445, 4, at(5)), tcpListen(3000, 500, at(5)), tcpListen(5500, 700, at(5)),
		tcpListen(7680, 800, at(5)), tcpListen(9000, pidLsass, at(5))}
	pl := PlanKill(ports.Of(445, 3000, 5500, 7680, 9000), socks, f, nil, Config{Self: self, SystemRoot: `C:\Windows`})
	if len(pl.Targets) != 0 || len(pl.Refusals) != 5 {
		t.Fatalf("plan = %+v", pl)
	}
	if m := pl.Masked(); len(m) != 0 {
		t.Fatalf("nada de esto se desenmascara: %+v", m)
	}
	x := External{Kind: ExtLinux, Where: "Ubuntu", Key: "Ubuntu/1@1", Name: "node"}
	for _, verdict := range []Unmasking{
		{Verdict: UnmaskFound, Behind: []External{x}},
		{Verdict: UnmaskEmpty, KillVia: true, Reason: "colgado"},
		{Verdict: UnmaskEmpty, Reason: "otro motivo", Advice: "otro consejo"},
	} {
		var answers []Unmasking
		for _, r := range pl.Refusals {
			for p := range r.Ports.All() {
				u := verdict
				u.Via, u.Port, u.Proto = r.Owner, p, ports.TCP
				answers = append(answers, u)
			}
		}
		got := Unmask(pl, answers)
		if len(got.Targets) != 0 || len(got.Refusals) != len(pl.Refusals) {
			t.Fatalf("veredicto %v: una negativa firme pasó a objetivo: %+v", verdict.Verdict, got.Targets)
		}
		for i, r := range got.Refusals {
			if r.Reason != pl.Refusals[i].Reason || r.Advice != pl.Refusals[i].Advice || r.Owner != pl.Refusals[i].Owner {
				t.Fatalf("veredicto %v: la negativa de %s cambió: %+v", verdict.Verdict, r.Name, r)
			}
		}
	}
}

// Sin respuestas, Unmask no cambia nada; y dos procesos Linux que comparten
// el puerto (SO_REUSEPORT) son dos objetivos.
func TestUnmaskSinRespuestasYReusePort(t *testing.T) {
	_, pl := relayPlan(t, "wslrelay.exe", ports.Of(3000), tcpListen(3000, relayPID, at(5)))
	same := Unmask(pl, nil)
	if len(same.Targets) != 0 || len(same.Refusals) != 1 || same.Refusals[0].Reason != pl.Refusals[0].Reason {
		t.Fatalf("sin respuestas cambió el plan: %+v", same)
	}
	a := External{Kind: ExtLinux, Where: "Ubuntu", Key: "Ubuntu/812@100", Name: "node", Ports: ports.Of(3000)}
	b := External{Kind: ExtLinux, Where: "Ubuntu", Key: "Ubuntu/813@101", Name: "node", Ports: ports.Of(3000)}
	two := Unmask(pl, []Unmasking{{Via: relayID, Port: 3000, Proto: ports.TCP, Verdict: UnmaskFound, Behind: []External{a, b}}})
	if len(two.Targets) != 2 || len(two.Refusals) != 0 {
		t.Fatalf("plan = %+v", two)
	}
}

// Solo mirar (sin escalada): el puerto de un externo sigue ocupado por su
// intermediario, y el motivo nombra a los dos. Y cuando no se pudo detener, el
// motivo dice a quién y por qué.
func TestVerifyNombraAlExterno(t *testing.T) {
	s, _, opts, before, pl, req := escenarioRelay(t, &simExt{})
	ps := Verify(ctx, s, before, req, pl, Report{}, opts)
	if len(ps) != 1 || ps[0].Status != PortBusy || ps[0].Owner.PID != relayPID ||
		ps[0].Reason != "lo tiene el contenedor db-1 (a través de com.docker.backend.exe)" {
		t.Fatalf("solo mirar = %+v", ps)
	}
	boom := errors.New("el motor contestó 500")
	s, cfg, opts, before, pl, req := escenarioRelay(t, &simExt{result: ExternalStop{Outcome: OutcomeError, Err: boom}})
	rep, _ := exec(s, before, req, pl, cfg, opts)
	if p := portOf(rep, 5432); p.Status != PortBusy || p.Reason != "no se pudo detener el contenedor db-1: el motor contestó 500" {
		t.Fatalf("5432 = %+v", p)
	}
	for _, tc := range []struct {
		x    External
		want string
	}{
		{External{Kind: ExtContainer, Name: "db-1", Where: "Docker Desktop"}, "el contenedor db-1"},
		{External{Kind: ExtLinux, Name: "node", Where: "Ubuntu"}, "node en Ubuntu"},
		{External{Kind: ExtLinux, Name: "node"}, "node"},
		{External{Kind: ExtKind(200), Name: "0.0.0.0:8080", Where: "portproxy"}, "0.0.0.0:8080"},
	} {
		if got := tc.x.Label(); got != tc.want {
			t.Errorf("Label = %q, esperaba %q", got, tc.want)
		}
	}
}

// Dos intermediarios, un externo. Docker Desktop (con WSL 2) publica cada
// puerto con com.docker.backend en 0.0.0.0 y [::] y, además, con wslrelay en
// [::1]: tres sockets de dos dueños para el mismo contenedor. Es UN objetivo
// con los tres sockets, y su Via es el motor (el intermediario de su
// dominio), sea cual sea el orden en que la tabla los trajo.
func TestUnmaskDosRelaysUnExterno(t *testing.T) {
	const wslPID = 301
	wslID := ID{PID: wslPID, Created: at(2)}
	loop6 := ports.Socket{Proto: ports.TCP, Local: netip.AddrPortFrom(netip.IPv6Loopback(), 5432), State: ports.Listen, PID: wslPID, Since: at(6)}
	for nombre, socks := range map[string][]ports.Socket{
		"el motor primero":     {tcpListen(5432, relayPID, at(5)), tcp6Listen(5432, relayPID, at(5)), loop6},
		"wslrelay primero":     {loop6, tcpListen(5432, relayPID, at(5)), tcp6Listen(5432, relayPID, at(5))},
		"wslrelay en el medio": {tcpListen(5432, relayPID, at(5)), loop6, tcp6Listen(5432, relayPID, at(5))},
	} {
		f := NewForest([]Proc{
			{ID: relayID, Name: "com.docker.backend.exe", Parent: 1, Path: `C:\Program Files\Docker\Docker\resources\com.docker.backend.exe`},
			{ID: wslID, Name: "wslrelay.exe", Parent: 1, Path: `C:\Program Files\WSL\wslrelay.exe`},
		})
		pl := PlanKill(ports.Of(5432), socks, f, nil, Config{SystemRoot: `C:\Windows`})
		if len(pl.Refusals) != 2 || len(pl.Masked()) != 2 {
			t.Fatalf("%s: sin desenmascarar son dos negativas: %+v", nombre, pl)
		}
		db := contenedor("c0ffee", "db-1", 5432)
		var answers []Unmasking
		for _, m := range pl.Masked() {
			answers = append(answers, Unmasking{Via: m.Via, Port: m.Port, Proto: m.Proto, Verdict: UnmaskFound, Behind: []External{db}})
		}
		pl = Unmask(pl, answers)
		if len(pl.Targets) != 1 || len(pl.Refusals) != 0 {
			t.Fatalf("%s: plan = %+v", nombre, pl)
		}
		tg := pl.Targets[0]
		if tg.Kind != StopExternal || len(tg.Sockets) != 3 || !tg.Ports.Equal(ports.Of(5432)) || len(tg.Victims) != 0 {
			t.Fatalf("%s: objetivo = %+v", nombre, tg)
		}
		if tg.Via != relayID || tg.ViaName != "com.docker.backend.exe" || tg.ViaClass != RelayContainer {
			t.Fatalf("%s: el Via de un contenedor es el motor, no el reenvío de WSL: %v %s %v", nombre, tg.Via, tg.ViaName, tg.ViaClass)
		}
	}

	// Al revés: a un proceso Linux lo nombra su wslrelay, aunque otro
	// intermediario también tenga un socket suyo.
	f := NewForest([]Proc{
		{ID: relayID, Name: "gvproxy.exe", Parent: 1},
		{ID: wslID, Name: "wslrelay.exe", Parent: 1},
	})
	pl := PlanKill(ports.Of(3000), []ports.Socket{tcpListen(3000, relayPID, at(5)), tcpListen(3000, wslPID, at(5))}, f, nil, Config{SystemRoot: `C:\Windows`})
	node := External{Kind: ExtLinux, Where: "Ubuntu", Key: "Ubuntu/812@100", Name: "node", Ports: ports.Of(3000)}
	var answers []Unmasking
	for _, m := range pl.Masked() {
		answers = append(answers, Unmasking{Via: m.Via, Port: m.Port, Proto: m.Proto, Verdict: UnmaskFound, Behind: []External{node}})
	}
	if pl = Unmask(pl, answers); len(pl.Targets) != 1 || pl.Targets[0].Via != wslID || pl.Targets[0].ViaClass != RelayWSL {
		t.Fatalf("plan = %+v", pl)
	}
}

// escenarioDosRelays: el contenedor c0ffee publica el 5432 con el motor (300,
// v4 y v6) y con wslrelay (301, [::1]); cada uno suelta el puerto a su tiempo.
func escenarioDosRelays(t *testing.T, x *simExt) (*simOS, Config, Options, *Forest, Plan, ports.Set) {
	t.Helper()
	const wslPID = 301
	s, cfg, opts := escenario()
	s.add(&simProc{id: relayID, name: "com.docker.backend.exe", parent: 1, path: `C:\Program Files\Docker\Docker\resources\com.docker.backend.exe`})
	s.add(&simProc{id: ID{PID: wslPID, Created: at(2)}, name: "wslrelay.exe", parent: 1, path: `C:\Program Files\WSL\wslrelay.exe`})
	s.sockets = append(s.sockets, tcpListen(5432, relayPID, at(5)), tcp6Listen(5432, relayPID, at(5)),
		ports.Socket{Proto: ports.TCP, Local: netip.AddrPortFrom(netip.IPv6Loopback(), 5432), State: ports.Listen, PID: wslPID, Since: at(6)})
	x.relayPID = relayPID
	x.ports = []uint16{5432}
	s.ext["c0ffee"] = x
	requested := ports.Of(5432)
	before, pl := buildPlan(s, cfg, requested, opts.Filter)
	var answers []Unmasking
	for _, m := range pl.Masked() {
		answers = append(answers, Unmasking{Via: m.Via, Port: m.Port, Proto: m.Proto, Verdict: UnmaskFound, Behind: []External{contenedor("c0ffee", "db-1", 5432)}})
	}
	pl = Unmask(pl, answers)
	if len(pl.Targets) != 1 || len(pl.Refusals) != 0 || len(pl.Targets[0].Sockets) != 3 {
		t.Fatalf("plan = %+v", pl)
	}
	return s, cfg, opts, before, pl, requested
}

// Medido con Docker Desktop de verdad: al detener el contenedor, el motor
// suelta sus sockets enseguida y wslrelay suelta el [::1] unos 315 ms después.
// La verificación espera a los DOS: el puerto no se canta libre (ni
// "retomado por otro proceso") mientras wslrelay lo tenga, y se canta apenas
// lo suelta.
func TestEscDosRelaysSueltanADestiempo(t *testing.T) {
	s, cfg, opts, before, pl, req := escenarioDosRelays(t, &simExt{closeAfter: 400 * time.Millisecond, release: 20 * time.Millisecond,
		otros: []simRelay{{pid: 301, release: 315 * time.Millisecond}}})
	opts.Settle = 350 * time.Millisecond // la ventana de producción
	rep, rec := exec(s, before, req, pl, cfg, opts)
	if e := rep.Externals[0]; e.Outcome != OutcomeClosed {
		t.Fatalf("externo = %+v", e)
	}
	p := portOf(rep, 5432)
	if p.Status != PortFree {
		t.Fatalf("5432 = %v (%s): tenía que esperar a que wslrelay lo suelte", p.Status, p.Reason)
	}
	ev, _ := rec.first(EvPortFreed)
	if libre := ev.Time.Sub(epoch); libre < (400+315)*time.Millisecond {
		t.Fatalf("lo cantó libre a los %v, con wslrelay todavía escuchando (lo suelta a los 715 ms)", libre)
	} else if libre > (400+315+350+250)*time.Millisecond {
		t.Fatalf("lo cantó libre a los %v: esperó de más", libre)
	}
	for _, ev := range rec.evs {
		if ev.Victim.PID == relayPID || ev.Victim.PID == 301 {
			t.Fatalf("ningún intermediario se abre ni se toca: %v", ev.Kind)
		}
	}
}

// El segundo intermediario no suelta: el puerto sigue ocupado y el motivo
// nombra al que lo tiene (wslrelay), no al Via.
func TestEscDosRelaysUnoNoSuelta(t *testing.T) {
	s, cfg, opts, before, pl, req := escenarioDosRelays(t, &simExt{closeAfter: 200 * time.Millisecond, release: 20 * time.Millisecond,
		otros: []simRelay{{pid: 301, never: true}}})
	rep, _ := exec(s, before, req, pl, cfg, opts)
	p := portOf(rep, 5432)
	if p.Status != PortBusy || p.Owner.PID != 301 || p.Reason != "el contenedor db-1 se detuvo, pero wslrelay.exe no soltó el puerto" {
		t.Fatalf("5432 = %+v", p)
	}
	if !strings.Contains(p.Advice, "relay quedó colgado") {
		t.Fatalf("el consejo es el del que lo tiene (el reenvío de WSL): %q", p.Advice)
	}
}

// --- escalada de externos en el simulador ----------------------------------

// escenarioRelay: shell + killport + com.docker.backend (300) publicando el
// 5432 (v4+v6) del contenedor c0ffee, ya desenmascarado.
func escenarioRelay(t *testing.T, x *simExt, extra ...ports.Socket) (*simOS, Config, Options, *Forest, Plan, ports.Set) {
	t.Helper()
	s, cfg, opts := escenario()
	s.add(&simProc{id: relayID, name: "com.docker.backend.exe", parent: 1, path: `C:\Program Files\Docker\Docker\resources\com.docker.backend.exe`})
	s.sockets = append(s.sockets, tcpListen(5432, relayPID, at(5)), tcp6Listen(5432, relayPID, at(5)))
	s.sockets = append(s.sockets, extra...)
	x.relayPID = relayPID
	x.ports = append(x.ports, 5432)
	s.ext["c0ffee"] = x
	requested := ports.Of(5432)
	for _, e := range extra {
		requested = requested.Union(ports.Of(e.Port()))
	}
	before, pl := buildPlan(s, cfg, requested, opts.Filter)
	pl = Unmask(pl, []Unmasking{{Via: relayID, Port: 5432, Proto: ports.TCP, Verdict: UnmaskFound, Behind: []External{contenedor("c0ffee", "db-1", 5432)}}})
	if len(pl.Targets) != 1 || pl.Targets[0].Kind != StopExternal {
		t.Fatalf("plan = %+v", pl)
	}
	return s, cfg, opts, before, pl, requested
}

func portOf(rep Report, p uint16) PortResult {
	for _, r := range rep.Ports {
		if r.Port == p {
			return r
		}
	}
	return PortResult{}
}

// Docker stop prolijo: el contenedor cede a los 1,2 s, el relay suelta el
// puerto 50 ms después, y el relay jamás se abre ni se toca.
func TestEscContenedorProlijo(t *testing.T) {
	s, cfg, opts, before, pl, req := escenarioRelay(t, &simExt{closeAfter: 1200 * time.Millisecond, release: 50 * time.Millisecond})
	rep, rec := exec(s, before, req, pl, cfg, opts)
	if len(rep.Externals) != 1 || len(rep.Victims) != 0 {
		t.Fatalf("reporte = %+v", rep)
	}
	e := rep.Externals[0]
	if e.Outcome != OutcomeClosed || e.Method != MethodEngine || e.Elapsed != 1200*time.Millisecond {
		t.Fatalf("externo = %v/%v en %v", e.Outcome, e.Method, e.Elapsed)
	}
	if ev, ok := rec.first(EvExternalStopped); !ok || !ev.Time.Equal(at(1200)) || ev.External.Key != "c0ffee" {
		t.Fatalf("evento de detención = %+v", ev)
	}
	if ev, _ := rec.first(EvExternalStopping); ev.Method != MethodNone || ev.External.Key != "c0ffee" {
		t.Fatalf("pedido de detención = %+v: sin --force es la escalera del dominio", ev)
	}
	if ev, _ := rec.first(EvWaiting); ev.Method != MethodEngine || !ev.Deadline.Equal(at(3000)) {
		t.Fatalf("espera = %+v", ev)
	}
	for _, ev := range rec.evs {
		if ev.Victim.PID == relayPID {
			t.Fatalf("el relay no se abre ni se toca: %v", ev.Kind)
		}
	}
	if len(s.extCalls) != 1 || s.extCalls[0].force || !s.extCalls[0].deadline.Equal(at(3000)) {
		t.Fatalf("pedido al dominio = %+v", s.extCalls)
	}
	if p := portOf(rep, 5432); p.Status != PortFree {
		t.Fatalf("5432 = %v (%s)", p.Status, p.Reason)
	}
	if s.now.After(at(1400)) {
		t.Fatalf("terminó a las %v: esperó de más", s.now.Sub(epoch))
	}
}

// El contenedor no cede: el dominio lo fuerza al vencer el plazo (3 s).
func TestEscContenedorSordo(t *testing.T) {
	s, cfg, opts, before, pl, req := escenarioRelay(t, &simExt{deaf: true, release: 10 * time.Millisecond})
	rep, rec := exec(s, before, req, pl, cfg, opts)
	if e := rep.Externals[0]; e.Outcome != OutcomeTerminated || e.Method != MethodTerminate || e.Elapsed != 3*time.Second {
		t.Fatalf("externo = %+v", e)
	}
	if ev, _ := rec.first(EvExternalStopped); !ev.Time.Equal(at(3000)) {
		t.Fatalf("forzado a las %v, esperaba +3 s", ev.Time.Sub(epoch))
	}
	if portOf(rep, 5432).Status != PortFree {
		t.Fatal("el puerto debía quedar libre")
	}
}

// --force llega al dominio: sin cierre prolijo (t=0 en docker stop).
func TestEscContenedorForce(t *testing.T) {
	s, cfg, opts, before, pl, req := escenarioRelay(t, &simExt{closeAfter: 100 * time.Millisecond})
	cfg.Force = true
	rep, rec := exec(s, before, req, pl, cfg, opts)
	if !s.extCalls[0].force {
		t.Fatal("--force no llegó al dominio")
	}
	if e := rep.Externals[0]; e.Outcome != OutcomeTerminated || e.Elapsed != 0 {
		t.Fatalf("externo = %+v", e)
	}
	// Los eventos dicen que va en seco: la interfaz no promete un cierre
	// prolijo (un SIGINT) que con --force nadie va a mandar.
	if ev, _ := rec.first(EvExternalStopping); ev.Method != MethodTerminate {
		t.Fatalf("EvExternalStopping con --force = %v, esperaba terminación", ev.Method)
	}
	if ev, _ := rec.first(EvWaiting); ev.Method != MethodTerminate {
		t.Fatalf("EvWaiting con --force = %v, esperaba terminación", ev.Method)
	}
}

// Sin permiso (fuera de docker-users): el puerto sigue ocupado con el motivo
// del dominio, y NO se espera a que el relay suelte algo que nadie detuvo.
func TestEscContenedorDenegado(t *testing.T) {
	deny := errors.New("tenés que estar en el grupo docker-users")
	s, cfg, opts, before, pl, req := escenarioRelay(t, &simExt{result: ExternalStop{Outcome: OutcomeDenied, Err: deny}})
	rep, rec := exec(s, before, req, pl, cfg, opts)
	if e := rep.Externals[0]; e.Outcome != OutcomeDenied {
		t.Fatalf("externo = %+v", e)
	}
	if ev, ok := rec.first(EvExternalFailed); !ok || ev.Detail != deny.Error() {
		t.Fatalf("evento de falla = %+v", ev)
	}
	p := portOf(rep, 5432)
	if p.Status != PortBusy || p.Reason != "acceso denegado" || p.Advice != deny.Error() || p.Owner.PID != relayPID {
		t.Fatalf("5432 = %+v", p)
	}
	if s.sleeps != 0 {
		t.Fatalf("durmió %d veces esperando un puerto que nadie liberó", s.sleeps)
	}
}

// Ya no existía (404/304): el relay tampoco lo sostiene y el puerto queda libre.
func TestEscContenedorYaNoEstaba(t *testing.T) {
	s, cfg, opts, before, pl, req := escenarioRelay(t, &simExt{result: ExternalStop{Outcome: OutcomeGone}})
	rep, _ := exec(s, before, req, pl, cfg, opts)
	if rep.Externals[0].Outcome != OutcomeGone || portOf(rep, 5432).Status != PortFree {
		t.Fatalf("reporte = %+v", rep)
	}
}

// El contenedor se detuvo pero el relay no suelta el puerto: se espera el
// tope (~2 s) y se informa con el nombre del relay y un consejo.
func TestEscRelayNoSuelta(t *testing.T) {
	s, cfg, opts, before, pl, req := escenarioRelay(t, &simExt{closeAfter: 200 * time.Millisecond, never: true})
	rep, _ := exec(s, before, req, pl, cfg, opts)
	p := portOf(rep, 5432)
	if p.Status != PortBusy || p.Reason != "el contenedor db-1 se detuvo, pero com.docker.backend.exe no soltó el puerto" || p.Advice == "" {
		t.Fatalf("5432 = %+v", p)
	}
	if lim := at(200).Add(2*time.Second + 200*time.Millisecond); s.now.After(lim) {
		t.Fatalf("la espera no respetó el tope: terminó a las %v", s.now.Sub(epoch))
	}
}

// Regresión de la espera por puerto: el mismo relay sostiene el 5432 (de
// db-1, que se detiene) y el 8888 (interno, negado). Apenas suelta el 5432 se
// declara; el 8888, que nadie va a soltar, no hace esperar el tope entero.
func TestEscRelayOtroPuertoNoHaceEsperar(t *testing.T) {
	s, cfg, opts, before, pl, req := escenarioRelay(t, &simExt{closeAfter: 1200 * time.Millisecond, release: 50 * time.Millisecond},
		tcpListen(8888, relayPID, at(5)))
	pl = Unmask(pl, []Unmasking{{Via: relayID, Port: 8888, Proto: ports.TCP, Verdict: UnmaskEmpty, Reason: "puerto interno de Docker Desktop"}})
	rep, _ := exec(s, before, req, pl, cfg, opts)
	if p := portOf(rep, 5432); p.Status != PortFree {
		t.Fatalf("5432 = %+v", p)
	}
	if p := portOf(rep, 8888); p.Status != PortRefused || p.Reason != "puerto interno de Docker Desktop" {
		t.Fatalf("8888 = %+v", p)
	}
	if s.now.After(at(1200 + 50 + 200)) {
		t.Fatalf("terminó a las %v: esperó por un puerto negado", s.now.Sub(epoch))
	}
}

// Ctrl+C del usuario mientras el motor detiene el contenedor: se corta, el
// externo queda pendiente y el reporte lo marca.
func TestEscCancelaDuranteExterno(t *testing.T) {
	s, cfg, opts, before, pl, req := escenarioRelay(t, &simExt{closeAfter: 1200 * time.Millisecond})
	s.cancelAt = at(500)
	rep, rec := exec(s, before, req, pl, cfg, opts)
	if !rep.Canceled || rep.Externals[0].Outcome != OutcomePending {
		t.Fatalf("reporte = %+v", rep)
	}
	if ev, _ := rec.first(EvCanceled); !ev.Time.Equal(at(500)) {
		t.Fatalf("canceló a las %v", ev.Time.Sub(epoch))
	}
	if rec.has(EvExternalStopped) {
		t.Fatal("no debía informar una detención después de cancelar")
	}
}

// Relay zombi: el dominio lo declaró colgado y seguro; se lo termina (no tiene
// consola ni ventanas: directo, sin esperar) y el puerto queda libre.
func TestEscRelayZombi(t *testing.T) {
	s, cfg, opts := escenario()
	s.add(&simProc{id: relayID, name: "wslrelay.exe", parent: 1})
	s.sockets = append(s.sockets, tcpListen(3000, relayPID, at(5)))
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	if len(pl.Targets) != 0 || len(pl.Refusals) != 1 {
		t.Fatalf("sin desenmascarar, el relay es negativa: %+v", pl)
	}
	pl = Unmask(pl, []Unmasking{{Via: relayID, Port: 3000, Proto: ports.TCP, Verdict: UnmaskEmpty, KillVia: true, Reason: "relay zombi"}})
	rep, rec := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeTerminated, MethodTerminate)
	if ev, _ := rec.first(EvTerminated); ev.Victim != relayID || !ev.Time.Equal(at(0)) {
		t.Fatalf("terminación = %+v", ev)
	}
	assertPort(t, rep, PortFree)
}

// Un adaptador que vuelve sin veredicto (contrato roto) no deja al externo
// "detenido": queda como error.
func TestEscExternoSinVeredicto(t *testing.T) {
	s, cfg, opts, before, pl, req := escenarioRelay(t, &simExt{closeAfter: 100 * time.Millisecond})
	mute := &muteExt{simOS: s}
	rec := &recorder{}
	opts.Emit = rec.emit
	rep := Run(ctx, mute, before, req, pl, cfg, opts)
	if e := rep.Externals[0]; e.Outcome != OutcomeError || !errors.Is(e.Err, errNoVerdict) {
		t.Fatalf("externo = %+v", e)
	}
}

// muteExt es un OS cuyo StopExternal vuelve sin llamar a done.
type muteExt struct{ *simOS }

func (m *muteExt) StopExternal(context.Context, []External, time.Time, bool, func(int, ExternalStop)) {
}

// C-10: dos contenedores publican el MISMO puerto en direcciones distintas del
// host (A en 127.0.0.1:8080, B en 192.168.1.5:8080); el motor abre un socket
// por dirección. El dominio contesta por parte (Unmasking.Sockets) y cada
// contenedor se lleva solo su socket: `killport 127.0.0.1:8080` apunta a A y
// B queda al margen.
func TestUnmaskRepartePorSocket(t *testing.T) {
	sA := ports.Socket{Proto: ports.TCP, Local: netip.MustParseAddrPort("127.0.0.1:8080"), State: ports.Listen, PID: relayPID, Since: at(5)}
	sB := ports.Socket{Proto: ports.TCP, Local: netip.MustParseAddrPort("192.168.1.5:8080"), State: ports.Listen, PID: relayPID, Since: at(6)}
	_, pl := relayPlan(t, "com.docker.backend.exe", ports.Of(8080), sA, sB)
	a, b := contenedor("aaa", "web-a", 8080), contenedor("bbb", "web-b", 8080)
	pl = Unmask(pl, []Unmasking{
		{Via: relayID, Port: 8080, Proto: ports.TCP, Verdict: UnmaskFound, Behind: []External{a}, Sockets: []ports.Socket{sA}},
		{Via: relayID, Port: 8080, Proto: ports.TCP, Verdict: UnmaskFound, Behind: []External{b}, Sockets: []ports.Socket{sB}},
	})
	if len(pl.Targets) != 2 || len(pl.Refusals) != 0 {
		t.Fatalf("plan = %+v", pl)
	}
	for _, tg := range pl.Targets {
		if len(tg.Sockets) != 1 || (tg.External.Key == "aaa") != (tg.Sockets[0] == sA) {
			t.Fatalf("%s se llevó %+v", tg.External.Name, tg.Sockets)
		}
	}
	req, err := ports.ParseRequest([]string{"127.0.0.1:8080"})
	if err != nil {
		t.Fatal(err)
	}
	fp := Focus(pl, req, nil)
	if len(fp.Targets) != 1 || fp.Targets[0].External.Name != "web-a" || len(fp.Bystanders) != 1 || fp.Bystanders[0].Name != "el contenedor web-b" {
		t.Fatalf("Focus(127.0.0.1:8080) = %+v / al margen %+v", fp.Targets, fp.Bystanders)
	}

	// Un socket que ninguna respuesta nombra, sin respuesta general: queda
	// negado como estaba (no se lo lleva nadie por descarte).
	_, pl = relayPlan(t, "com.docker.backend.exe", ports.Of(8080), sA, sB)
	pl = Unmask(pl, []Unmasking{{Via: relayID, Port: 8080, Proto: ports.TCP, Verdict: UnmaskFound, Behind: []External{a}, Sockets: []ports.Socket{sA}}})
	if len(pl.Targets) != 1 || len(pl.Targets[0].Sockets) != 1 || len(pl.Refusals) != 1 || pl.Refusals[0].Sockets[0] != sB {
		t.Fatalf("plan = %+v", pl)
	}
}
