package reaper

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// El diagnóstico del puerto sin dueño (C3 y el de WinNAT): primero se sondea,
// después se lee la tabla de exclusiones; y la tabla de sockets explica un
// bind que falló.

func winnatBlock(lo, hi uint16) ports.Exclusions {
	return ports.Exclusions{
		{Proto: ports.TCP, Family: ports.IPv4, Range: ports.Range{Lo: lo, Hi: hi}},
		{Proto: ports.TCP, Family: ports.IPv6, Range: ports.Range{Lo: lo, Hi: hi}},
	}
}

func administered(lo, hi uint16) ports.Exclusions {
	return ports.Exclusions{
		{Proto: ports.TCP, Family: ports.IPv4, Range: ports.Range{Lo: lo, Hi: hi}, Administered: true},
		{Proto: ports.TCP, Family: ports.IPv6, Range: ports.Range{Lo: lo, Hi: hi}, Administered: true},
	}
}

func dynamic(start uint16, count int) ports.DynamicRanges {
	var out ports.DynamicRanges
	for _, proto := range []ports.Proto{ports.TCP, ports.UDP} {
		for _, fam := range []ports.Family{ports.IPv4, ports.IPv6} {
			out = append(out, ports.DynamicRange{Proto: proto, Family: fam, Start: start, Count: count})
		}
	}
	return out
}

func conn(local string, pid uint32, state ports.State) ports.Socket {
	return ports.Socket{Proto: ports.TCP, Local: netip.MustParseAddrPort(local), Remote: netip.MustParseAddrPort("93.184.216.34:443"), State: state, PID: pid, Since: at(1)}
}

func diagnose(s *simOS, p uint16, socks ...ports.Socket) PortResult {
	procs, _ := s.Snapshot(ctx)
	return NewDiagnoser(s, ports.Filter{}, true).Seen(socks, NewForest(procs)).Port(p)
}

// C3: una exclusión administrada ("*") no le niega el puerto a quien lo pide
// por número. Si el bind anda, está LIBRE —y se dice la salvedad—; jamás se
// aconseja borrar la exclusión.
func TestDiagExclusionAdministradaSeSondeaPrimero(t *testing.T) {
	s := newSim(epoch)
	s.excluded = administered(3000, 3000)
	got := diagnose(s, 3000)
	if got.Status != PortFree || !got.Reserved.Administered || !strings.Contains(got.Reason, "exclusión administrada 3000") || len(got.Remedies) != 0 {
		t.Fatalf("3000 = %+v", got)
	}
	if s.probes == 0 {
		t.Fatal("no sondeó")
	}

	// El bind falla aunque la exclusión es administrada: es de otro.
	s.binds[3000] = ports.BindForbidden
	got = diagnose(s, 3000)
	if got.Status != PortReserved || !strings.Contains(got.Advice, "no borres") {
		t.Fatalf("3000 con el bind negado = %+v", got)
	}
	for _, malo := range []string{"delete excludedportrange", "--unreserve"} {
		if strings.Contains(got.Reason+got.Advice, malo) {
			t.Fatalf("aconseja %q sobre una exclusión ajena: %+v", malo, got)
		}
		for _, r := range got.Remedies {
			if strings.Contains(remedio(r), malo) {
				t.Fatalf("ofrece %q sobre una exclusión ajena", remedio(r))
			}
		}
	}
}

// Un bloque de WinNAT con el bind negado: reservado, con el rango y las
// salidas en orden (reservarlo, y reiniciar winnat como lo más barato).
func TestDiagBloqueDeWinNAT(t *testing.T) {
	s := newSim(epoch)
	s.excluded = winnatBlock(2980, 3079)
	s.dynamic = dynamic(49152, 16384)
	s.binds[3000] = ports.BindForbidden
	got := diagnose(s, 3000)
	if got.Status != PortReserved || got.Proto != ports.TCP || got.Reserved.Lo != 2980 || got.Reserved.Administered {
		t.Fatalf("3000 = %+v", got)
	}
	if !strings.Contains(got.Reason, "2980–3079") || !strings.Contains(got.Reason, "WinNAT") || got.Dynamic != (ports.DynamicRange{}) {
		t.Fatalf("motivo = %q (dinámico %+v)", got.Reason, got.Dynamic)
	}
	var cmds []string
	for _, r := range got.Remedies {
		cmds = append(cmds, remedio(r))
		if r.Effect == "" {
			t.Errorf("la salida %q no dice qué cuesta", remedio(r))
		}
	}
	if want := "reservar 3000/TCP|net stop winnat + net start winnat"; strings.Join(cmds, "|") != want {
		t.Fatalf("salidas = %q", strings.Join(cmds, "|"))
	}
	// Cada paso es un comando solo (se pega en cualquier consola), y detener
	// WinNAT va SIEMPRE con volver a arrancarlo, en la misma salida: antes el
	// arranque iba en el texto del efecto, y quien ejecutaba la columna de
	// comandos del JSON dejaba sin red a WSL, Docker y Hyper-V (X-26).
	for _, r := range got.Remedies {
		for _, st := range r.Steps {
			if strings.ContainsAny(st, "&;|") {
				t.Errorf("el paso %q no se puede pegar en cualquier consola", st)
			}
		}
		if r.Kind == RemedySteps && slices.Contains(r.Steps, "net stop winnat") && !slices.Contains(r.Steps, "net start winnat") {
			t.Errorf("detener WinNAT sin volver a arrancarlo: %+v", r)
		}
	}

	// Con el rango dinámico corrido, se dice y se ofrece arreglarlo.
	s.dynamic = dynamic(1025, 3976)
	got = diagnose(s, 3000)
	if !got.Dynamic.Low() || got.Dynamic.Start != 1025 || !strings.Contains(got.Reason, "1025–5000") || !strings.Contains(got.Reason, "49152–65535") {
		t.Fatalf("con el dinámico corrido = %+v", got)
	}
	if len(got.Remedies) != 3 || got.Remedies[1].Kind != RemedyFixDynamic {
		t.Fatalf("salidas = %+v", got.Remedies)
	}

	// Un rango dinámico corrido que no tiene que ver con ese bloque no se culpa.
	s.excluded = winnatBlock(50000, 50099)
	s.dynamic = dynamic(1025, 3976)
	s.binds[50000] = ports.BindForbidden
	if got = diagnose(s, 50000); got.Dynamic != (ports.DynamicRange{}) || len(got.Remedies) != 2 {
		t.Fatalf("50000 = %+v", got)
	}
}

// WinNAT también reserva bloques de UDP (esta máquina tiene seis). La salida
// que reserva el puerto tiene que ser la del protocolo tapado: --reserve solo
// reserva TCP, así que para un bloque de UDP el comando lleva --udp, o el
// usuario reservaría el protocolo que no es y el puerto seguiría tapado. Visto
// con el binario real: `killport 63300 --udp` ofrecía `killport 63300 --reserve`.
func TestDiagBloqueDeWinNATEnUDP(t *testing.T) {
	for _, tc := range []struct {
		proto ports.Proto
		want  string
	}{
		{ports.UDP, "reservar 63300/UDP"},
		{ports.TCP, "reservar 63300/TCP"},
	} {
		s := newSim(epoch)
		s.excluded = ports.Exclusions{{Proto: tc.proto, Family: ports.IPv4, Range: ports.Range{Lo: 63264, Hi: 63363}}}
		s.bindsBy[bindKey{tc.proto, ports.IPv4, 63300}] = ports.BindForbidden
		// Sin filtro de protocolo: se sondean los dos, y el veredicto es el del
		// protocolo tapado.
		got := diagnose(s, 63300)
		if got.Status != PortReserved || got.Proto != tc.proto || len(got.Remedies) == 0 {
			t.Fatalf("%v: 63300 = %+v", tc.proto, got)
		}
		if remedio(got.Remedies[0]) != tc.want {
			t.Errorf("%v: la salida que reserva es %q, y tendría que ser %q", tc.proto, remedio(got.Remedies[0]), tc.want)
		}
	}
}

// Una exclusión sin "*" que NO es un bloque de WinNAT —el puerto que atiende
// http.sys, la reserva de un programa— se dice por lo que es, y no lleva
// ninguna salida de WinNAT: ni reservarlo (que detiene WinNAT) ni reiniciarlo
// la sacan, y cortarles la red a WSL y Docker para nada no es una salida.
// Antes toda exclusión sin "*" era "el bloque que WinNAT reservó": con el
// 2869 de SSDP y con una reserva de un programa se aconsejaba --reserve.
func TestDiagExclusionQueNoEsDeWinNAT(t *testing.T) {
	ssdp := ports.HTTPHold{Held: true, URL: "http://*:2869/", Account: "servicio local", What: "SSDP (UPnP), el descubrimiento de dispositivos de la red"}
	casos := []struct {
		nombre     string
		fila       ports.ExcludedRange
		puerto     uint16
		dice, pide []string
	}{
		{"http.sys con lo que se sabe", ports.ExcludedRange{Range: ports.Range{Lo: 2869, Hi: 2869}, HTTP: ssdp}, 2869,
			[]string{"cae en la exclusión 2869, que Windows pone mientras http.sys atiende ese puerto: es SSDP (UPnP), el descubrimiento de dispositivos de la red (http://*:2869/, reservada a servicio local)"},
			[]string{"no es de WinNAT", "Windows la quita sola"}},
		{"http.sys con su servicio", ports.ExcludedRange{Range: ports.Range{Lo: 5985, Hi: 5985},
			HTTP: ports.HTTPHold{Held: true, URL: "http://+:5985/wsman/", Service: "WinRM", What: "la administración remota de Windows (WinRM)"}}, 5985,
			[]string{"(http://+:5985/wsman/, servicio WinRM)"}, []string{"no es de WinNAT"}},
		{"http.sys sin más detalle", ports.ExcludedRange{Range: ports.Range{Lo: 47230, Hi: 47230}, HTTP: ports.HTTPHold{Held: true}}, 47230,
			[]string{"cae en la exclusión 47230, que Windows pone mientras http.sys atiende ese puerto"}, []string{"no es de WinNAT"}},
		{"un programa", ports.ExcludedRange{Range: ports.Range{Lo: 47111, Hi: 47117}}, 47113,
			[]string{"cae en la exclusión 47111–47117, que no es de WinNAT (sus bloques son de 100 puertos) ni la agregó un administrador", "Windows no dice cuál"},
			[]string{"no hay proceso a la vista", "ese programa la suelta o termina"}},
		{"un programa, de un puerto", ports.ExcludedRange{Range: ports.Range{Lo: 3000, Hi: 3000}}, 3000,
			[]string{"cae en la exclusión 3000, que no es de WinNAT"}, []string{"ni reiniciar WinNAT la sacan"}},
	}
	for _, c := range casos {
		for _, probe := range []bool{true, false} {
			s := newSim(epoch)
			for _, fam := range []ports.Family{ports.IPv4, ports.IPv6} {
				x := c.fila
				x.Proto, x.Family = ports.TCP, fam
				s.excluded = append(s.excluded, x)
			}
			s.dynamic = dynamic(1025, 3976) // corrido: a WinNAT se lo culparía; acá no
			s.binds[c.puerto] = ports.BindForbidden
			procs, _ := s.Snapshot(ctx)
			got := NewDiagnoser(s, ports.Filter{}, probe).Seen(nil, NewForest(procs)).Port(c.puerto)
			if got.Status != PortReserved || got.Proto != ports.TCP || got.Reserved.Range != c.fila.Range || got.Reserved.Administered {
				t.Fatalf("%s (sondeo %v): %+v", c.nombre, probe, got)
			}
			for _, d := range c.dice {
				if !strings.Contains(got.Reason, d) {
					t.Errorf("%s (sondeo %v): el motivo no dice %q:\n  %s", c.nombre, probe, d, got.Reason)
				}
			}
			for _, d := range c.pide {
				if !strings.Contains(got.Advice, d) {
					t.Errorf("%s (sondeo %v): el consejo no dice %q:\n  %s", c.nombre, probe, d, got.Advice)
				}
			}
			if len(got.Remedies) != 0 || got.Dynamic != (ports.DynamicRange{}) || strings.Contains(got.Reason, "reservó para Hyper-V") ||
				strings.Contains(got.Reason+got.Advice, "net stop") || strings.Contains(got.Reason+got.Advice, "--reserve") {
				t.Errorf("%s (sondeo %v): le da salidas de WinNAT a lo que no es de WinNAT: %+v", c.nombre, probe, got)
			}
		}
	}
}

// La tabla de exclusiones y el sondeo se leen en instantes distintos: si el
// bind anda, el puerto está libre aunque la tabla diga que WinNAT lo tiene.
func TestDiagElSondeoLeGanaALaTabla(t *testing.T) {
	s := newSim(epoch)
	s.excluded = winnatBlock(2980, 3079)
	if got := diagnose(s, 3000); got.Status != PortFree || got.Reason != "" {
		t.Fatalf("3000 = %+v", got)
	}
}

// Con uso exclusivo, un bind que falla puede deberse a CONEXIONES sobre ese
// puerto local. Eso no es ocupar el puerto: queda libre, con la salvedad y
// con el nombre de quien las tiene.
func TestDiagConexionesNoOcupan(t *testing.T) {
	s := newSim(epoch)
	s.add(&simProc{id: ID{PID: 812, Created: at(1)}, name: "chrome.exe"})
	tcp := func(r ports.BindResult) {
		s.bindsBy[bindKey{ports.TCP, ports.IPv4, 3000}] = r
		s.bindsBy[bindKey{ports.TCP, ports.IPv6, 3000}] = r
	}
	tcp(ports.BindInUse)
	got := diagnose(s, 3000, conn("192.0.2.68:3000", 812, ports.Established), conn("192.0.2.68:3001", 812, ports.Established))
	if got.Status != PortFree || !strings.Contains(got.Reason, "chrome.exe (PID 812)") || !strings.Contains(got.Reason, "uso exclusivo") {
		t.Fatalf("3000 = %+v", got)
	}
	if len(got.Remedies) != 0 {
		t.Fatalf("sin el dinámico corrido no hay nada que arreglar: %+v", got.Remedies)
	}

	// Con el rango dinámico corrido, Windows reparte ese puerto como puerto de salida.
	s.dynamic = dynamic(1025, 3976)
	got = diagnose(s, 3000, conn("192.0.2.68:3000", 812, ports.Established))
	if got.Status != PortFree || !got.Dynamic.Low() || len(got.Remedies) != 1 || got.Remedies[0].Kind != RemedyFixDynamic {
		t.Fatalf("3000 con el dinámico corrido = %+v", got)
	}

	// Restos de conexiones que se cierran (sin dueño).
	s.dynamic = nil
	got = diagnose(s, 3000, conn("127.0.0.1:3000", 0, ports.TimeWait))
	if got.Status != PortFree || !strings.Contains(got.Reason, "TIME_WAIT") {
		t.Fatalf("3000 con restos = %+v", got)
	}

	// WSAEACCES por lo mismo (el otro socket no comparte): igual.
	tcp(ports.BindForbidden)
	got = diagnose(s, 3000, conn("192.0.2.68:3000", 812, ports.Established))
	if got.Status != PortFree || !strings.Contains(got.Reason, "chrome.exe") {
		t.Fatalf("3000 con WSAEACCES y una conexión = %+v", got)
	}
}

// El bind falla y en las tablas no hay NADA sobre ese puerto: alguien lo ató
// sin escuchar (en uso), o Windows lo niega sin que figure en ningún rango.
func TestDiagSinNadaEnLasTablas(t *testing.T) {
	s := newSim(epoch)
	s.binds[3000] = ports.BindInUse
	got := diagnose(s, 3000, conn("192.0.2.68:3001", 812, ports.Established))
	if got.Status != PortBusy || !strings.Contains(got.Advice, "netstat -anoq") {
		t.Fatalf("en uso = %+v", got)
	}
	s.binds[3000] = ports.BindForbidden
	if got = diagnose(s, 3000); got.Status != PortReserved || got.Reserved != (ports.ExcludedRange{}) || got.Advice == "" {
		t.Fatalf("negado = %+v", got)
	}
}

// Sin sondeo (un pedido de miles de puertos) manda la tabla: el bloque de
// WinNAT es reservado; la exclusión administrada, libre con la salvedad de
// que no se sondeó. Y un sondeo que no pudo decir nada vale lo mismo.
func TestDiagSinSondeo(t *testing.T) {
	s := newSim(epoch)
	s.excluded = append(winnatBlock(2980, 3079), administered(8080, 8080)...)
	d := NewDiagnoser(s, ports.Filter{}, false)
	if got := d.Port(3000); got.Status != PortReserved || got.Reserved.Lo != 2980 {
		t.Fatalf("3000 = %+v", got)
	}
	if got := d.Port(8080); got.Status != PortFree || !strings.Contains(got.Reason, "no sondeé") {
		t.Fatalf("8080 = %+v", got)
	}
	if got := d.Port(9000); got.Status != PortFree || got.Reason != "" {
		t.Fatalf("9000 = %+v", got)
	}
	if s.probes != 0 || s.exclReads != 1 || s.dynReads != 1 {
		t.Fatalf("sondeos %d, lecturas de exclusiones %d y del dinámico %d", s.probes, s.exclReads, s.dynReads)
	}

	s.binds[3000] = ports.BindError
	if got := diagnose(s, 3000); got.Status != PortReserved {
		t.Fatalf("con el sondeo mudo manda la tabla: %+v", got)
	}
}

// El filtro de protocolo acota el diagnóstico, y la familia se sondea por
// separado: el primer veredicto que no es "libre" gana.
func TestDiagProtocoloYFamilia(t *testing.T) {
	s := newSim(epoch)
	s.bindsBy[bindKey{ports.UDP, ports.IPv6, 5353}] = ports.BindInUse
	d := NewDiagnoser(s, ports.Filter{TCP: true}, true)
	if got := d.Port(5353); got.Status != PortFree {
		t.Fatalf("con --tcp el UDP no cuenta: %+v", got)
	}
	d = NewDiagnoser(s, ports.Filter{}, true)
	if got := d.Port(5353); got.Status != PortBusy || got.Proto != ports.UDP {
		t.Fatalf("5353 = %+v", got)
	}
}

// Un proceso MARCADO crítico no se toca, se llame como se llame: no se le
// manda ninguna señal, no se lo termina, y el veredicto del puerto lo dice.
func TestEscCritico(t *testing.T) {
	s, cfg, opts := escenario()
	node := nodeTarget(s)
	node.console, node.ctrlCReacts, node.windows = []uint32{100}, true, 1
	s.critical[node.id] = true
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	rep, rec := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeCritical, MethodNone)
	for _, k := range []EventKind{EvCtrlC, EvClose, EvTerminated, EvWaiting} {
		if rec.has(k) {
			t.Fatalf("a un proceso crítico no le llega %v", k)
		}
	}
	if node.exited {
		t.Fatal("el proceso crítico murió")
	}
	p := assertPort(t, rep, PortBusy)
	if !strings.Contains(p.Reason, "crítico") || p.Advice == "" {
		t.Fatalf("puerto = %+v", p)
	}
	if OutcomeCritical.Done() || OutcomeCritical.String() != "crítico" {
		t.Fatal("crítico no es un final que libere el puerto")
	}
}

// Con --tree, un descendiente marcado crítico que comparte la consola de la
// raíz impide el Ctrl+C (lo bajaría también a él): la raíz se termina en seco
// y el crítico queda como estaba.
func TestEscCriticoEnLaConsolaDeLaRaiz(t *testing.T) {
	s, cfg, opts := escenario()
	cfg.Tree = true
	node := nodeTarget(s)
	hijo := s.add(&simProc{id: ID{PID: 101, Created: at(70)}, name: "vigia.exe", parent: 100, console: []uint32{100, 101}, ctrlCReacts: true})
	node.console, node.ctrlCReacts, node.ctrlCAfter = []uint32{100, 101}, true, 100*time.Millisecond
	s.critical[hijo.id] = true
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	rep, rec := exec(s, before, req, pl, cfg, opts)
	if rec.has(EvCtrlC) {
		t.Fatal("se mandó Ctrl+C a una consola con un proceso crítico adentro")
	}
	if hijo.exited {
		t.Fatal("el proceso crítico murió")
	}
	var raiz, critico VictimResult
	for _, v := range rep.Victims {
		if v.ID == node.id {
			raiz = v
		} else {
			critico = v
		}
	}
	if raiz.Outcome != OutcomeTerminated || critico.Outcome != OutcomeCritical {
		t.Fatalf("raíz %v, crítico %v", raiz.Outcome, critico.Outcome)
	}
}

// remedio escribe una salida para compararla: "reservar 3000/TCP",
// "rango dinámico", "net stop winnat + net start winnat". El comando de
// verdad lo arma la herramienta (ver RemedyKind).
func remedio(r Remedy) string {
	switch r.Kind {
	case RemedyReserve:
		return "reservar " + itoa(uint32(r.Port)) + "/" + r.Proto.String()
	case RemedyFixDynamic:
		return "rango dinámico"
	}
	return strings.Join(r.Steps, " + ")
}
