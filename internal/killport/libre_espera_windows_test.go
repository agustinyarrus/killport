//go:build windows

package killport

// Pruebas de --free y --wait contra el sistema SIMULADO (sistemaDeRed: netsh,
// tablas y sondeo de mentira, coherentes entre sí): qué flags van con qué
// modo, --free sin consola y en JSON, la sugerencia del próximo libre, los
// códigos de --json, y la espera (--wait / --wait-free) con una tabla que
// cambia con el tiempo.

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

// capturarAncho es capturar con la terminal a un ancho fijo.
func capturarAncho(t *testing.T, w int, f func(term *tui.Term) int) (string, int) {
	t.Helper()
	return capturar(t, func(term *tui.Term) int {
		term.SetWidth(w)
		return f(term)
	})
}

// --- modos ---------------------------------------------------------------------

func TestValidarModos(t *testing.T) {
	w := func(o opciones) opciones { o.wait, o.waitDado = 5*time.Second, true; return o }
	wf := func(o opciones) opciones { o.waitFree, o.waitFreeDado = 5*time.Second, true; return o }
	casos := []struct {
		nombre string
		pos    []string
		o      opciones
		err    string // "" = vale
	}{
		{"cerrar, lo de siempre", []string{"3000"}, opciones{yes: true, tree: true}, ""},
		{"--free solo", nil, opciones{free: true}, ""},
		{"--free con --count y --udp y --json", []string{"3000"}, opciones{free: true, countDado: true, count: 3, udp: true, json: true}, ""},
		{"--count sin --free", []string{"3000"}, opciones{countDado: true, count: 3}, "--count va con --free"},
		{"--free con -y", nil, opciones{free: true, yes: true}, "no va con --yes"},
		{"--free con --tree", nil, opciones{free: true, tree: true}, "no va con --tree"},
		{"--free con --list", nil, opciones{free: true, list: true}, "no va con --list"},
		{"--free con --timeout", nil, opciones{free: true, timeoutDado: true}, "no va con --timeout"},
		{"--free con --all-states", nil, opciones{free: true, allStates: true}, "no va con --all-states"},
		{"--free con --orphans", nil, opciones{free: true, sel: seleccion{huerfanos: true}}, "no va con --orphans"},
		{"--free con --wait", []string{"3000"}, w(opciones{free: true}), "--free y --wait no van juntos"},
		{"--free con --reserve", []string{"3000"}, opciones{free: true, reserve: true}, "--free y --reserve no van juntos"},
		{"--wait con puertos", []string{"5173"}, w(opciones{any: true, stableDado: true, nuevos: true, udp: true, json: true}), ""},
		{"--wait sin puertos", nil, w(opciones{}), "--wait necesita los puertos"},
		{"--wait con -y", []string{"5173"}, w(opciones{yes: true}), "--wait espera sin cerrar nada: no va con --yes"},
		{"--wait con --name", []string{"5173"}, w(opciones{sel: seleccion{nombres: []string{"node"}}}), "no va con --name"},
		{"--wait con --all-states", []string{"5173"}, w(opciones{allStates: true}), "--all-states va con --wait-free"},
		{"--wait-free con --all-states", []string{"3000"}, wf(opciones{allStates: true}), ""},
		{"--wait y --wait-free", []string{"3000"}, wf(w(opciones{})), "no van juntos"},
		{"--wait-free con --new", []string{"3000"}, wf(opciones{nuevos: true}), "--new va con --wait"},
		{"--wait con --unreserve", []string{"3000"}, w(opciones{unreserve: true}), "no va con --unreserve"},
		{"--any sin espera", []string{"3000"}, opciones{any: true}, "--any va con --wait o --wait-free (ej.: killport 3000 8080 --wait 60s --any)"},
		{"--stable sin espera", []string{"3000"}, opciones{stableDado: true}, "--stable va con --wait"},
		{"--new sin espera", []string{"3000"}, opciones{nuevos: true}, "--new va con --wait"},
		{"--stable más largo que --wait-free", []string{"3000"}, wf(opciones{stable: 10 * time.Second, stableDado: true}), "--stable 10 s no entra en --wait-free 5 s"},
		{"--stable igual a --wait-free", []string{"3000"}, wf(opciones{stable: 5 * time.Second, stableDado: true}), ""},
	}
	for _, c := range casos {
		err := validarModos(c.pos, c.o)
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%s: %v", c.nombre, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: error %v, esperaba %q", c.nombre, err, c.err)
		}
	}
}

func TestPlazosYCantidades(t *testing.T) {
	for v, want := range map[string]time.Duration{"60": time.Minute, "60s": time.Minute, "750ms": 750 * time.Millisecond, "1:30": 90 * time.Second, "30m": 30 * time.Minute, "0": 0} {
		if d, err := plazoEspera(v); err != nil || d != want {
			t.Errorf("plazo %q = %v, %v", v, d, err)
		}
	}
	for _, v := range []string{"31m", "2h", "abc", "-5s"} {
		if _, err := plazoEspera(v); err == nil {
			t.Errorf("plazo %q debería fallar", v)
		}
	}
	for v, want := range map[string]int{"1": 1, "100": 100, " 7 ": 7} {
		if n, err := cantidadLibres(v); err != nil || n != want {
			t.Errorf("cantidad %q = %d, %v", v, n, err)
		}
	}
	for _, v := range []string{"0", "101", "-1", "x"} {
		if _, err := cantidadLibres(v); err == nil {
			t.Errorf("cantidad %q debería fallar", v)
		}
	}
	for d, want := range map[time.Duration]string{5 * time.Second: "5 s", 750 * time.Millisecond: "750 ms", 42500 * time.Millisecond: "42,5 s", 90 * time.Second: "1 min 30 s"} {
		if got := textoPlazo(d); got != want {
			t.Errorf("textoPlazo(%v) = %q", d, got)
		}
	}
}

// Los errores de modo salen por Main con el código de línea de comandos.
func TestModosPorMain(t *testing.T) {
	for _, args := range [][]string{
		{"3000", "--count", "3"},
		{"--free", "-y"},
		{"--wait", "5s"},
		{"5173", "--wait", "31m"},
		{"5173", "--wait", "5s", "--wait-free", "5s"},
		{"3000", "--any"},
		{"--free", "3000", "4000"},
		{"--free", "--count", "0"},
	} {
		out, code := capturar(t, func(term *tui.Term) int { return Main(term, "test", args) })
		if code != cli.ExitUsage || !strings.Contains(out, "✗") {
			t.Errorf("%q: código %d\n%s", args, code, out)
		}
	}
}

// --- --free --------------------------------------------------------------------

// redConBloque: el 3000 cae en un bloque de WinNAT (2980–3079), el rango
// dinámico está corrido (1025–5000) y el 8080 está tomado.
func TestLibreSinConsolaSoloNumeros(t *testing.T) {
	casos := []struct {
		pos   []string
		o     opciones
		out   string
		code  int
		stder string
	}{
		{[]string{"3000"}, opciones{}, "5001\n", cli.ExitOK, ""},
		{[]string{"3000"}, opciones{count: 3}, "5001\n5002\n5003\n", cli.ExitOK, ""},
		{[]string{"8080-8081"}, opciones{}, "8081\n", cli.ExitOK, ""},
		{[]string{"2980-3079"}, opciones{}, "", cli.ExitFailure, ""},
		// Un rango explícito vale aunque caiga en el dinámico (lo pidió): del
		// 3075 al 3079 tapa WinNAT; quedan dos de los cinco pedidos.
		{[]string{"3075-3081"}, opciones{count: 5}, "3080\n3081\n", cli.ExitPartial, ""},
		// UDP: el bloque de WinNAT es de TCP; en UDP el 3000 está libre, pero
		// el rango dinámico corrido lo tapa igual.
		{[]string{"3000"}, opciones{udp: true}, "5001\n", cli.ExitOK, ""},
		{[]string{"50000-50100"}, opciones{udp: true}, "50060\n", cli.ExitOK, ""},
	}
	for _, c := range casos {
		sys := sistemaDeRed(redConBloque(), false)
		out, code := capturar(t, func(term *tui.Term) int { return correrLibre(term, sys, c.pos, c.o) })
		if out != c.out || code != c.code {
			t.Errorf("--free %q %+v: código %d, salida %q; esperaba %d, %q", c.pos, c.o, code, out, c.code, c.out)
		}
	}
}

func TestLibreJSON(t *testing.T) {
	sys := sistemaDeRed(redConBloque(), false)
	out, code := capturar(t, func(term *tui.Term) int { return correrLibre(term, sys, []string{"3000"}, opciones{json: true}) })
	var doc jsonLibre
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != cli.ExitOK {
		t.Fatalf("código %d, %v\n%s", code, err, out)
	}
	if len(doc.Encontrados) != 1 || doc.Encontrados[0] != 5001 || doc.Inicio != 3000 || doc.Fin != 65535 || doc.Explicito || doc.Protos[0] != "TCP" {
		t.Fatalf("doc: %+v", doc)
	}
	if len(doc.Salteados) != 2 || doc.Salteados[0].Motivo != "winnat" || doc.Salteados[0].Inicio != 3000 || doc.Salteados[0].Fin != 3079 ||
		doc.Salteados[0].Rango.Inicio != 2980 || doc.Salteados[1].Motivo != "dinamico" || doc.Salteados[1].Fin != 5000 || doc.Salteados[1].Dinamico.DeFabrica {
		t.Fatalf("salteados: %+v", doc.Salteados)
	}
	// Sin lugar: sale 3 igual, con el documento.
	out, code = capturar(t, func(term *tui.Term) int {
		return correrLibre(term, sys, []string{"2980-3079"}, opciones{json: true})
	})
	if json.Unmarshal([]byte(out), &doc) != nil || code != cli.ExitFailure || len(doc.Encontrados) != 0 {
		t.Fatalf("sin lugar: código %d\n%s", code, out)
	}
}

// Lo que se cuenta de lo salteado en la consola, con el tope.
func TestLineasSalteos(t *testing.T) {
	term := tui.Open()
	var skips []ports.Skip
	for i := range 11 {
		skips = append(skips, ports.Skip{Range: ports.Range{Lo: uint16(3000 + 2*i), Hi: uint16(3000 + 2*i)}, Why: ports.SkipTaken,
			Socket: ports.Socket{Proto: ports.TCP, State: ports.Listen, PID: 812}})
	}
	skips[1] = ports.Skip{Range: ports.Range{Lo: 3002, Hi: 3079}, Why: ports.SkipWinNAT, Excluded: ports.ExcludedRange{Range: ports.Range{Lo: 2980, Hi: 3079}}}
	texto := strings.Join(lineasSalteos(term, skips, func(uint32) string { return "node.exe (PID 812)" }, 8), "\n")
	for _, want := range []string{"salteé", "3000        lo escucha node.exe (PID 812)", "3002–3079   lo reservó WinNAT", "y 3 tramos más"} {
		if !strings.Contains(texto, want) {
			t.Errorf("falta %q en:\n%s", want, texto)
		}
	}
}

// --- la sugerencia ---------------------------------------------------------------

func TestAlternativas(t *testing.T) {
	sys := sistemaDeRed(redConBloque(), false)
	ctx := context.Background()
	tcp := func(uint16) []ports.Proto { return []ports.Proto{ports.TCP} }
	got := alternativas(ctx, sys, []uint16{3000, 8079, 65535}, tcp)
	// 3000: el bloque de WinNAT y el dinámico corrido lo empujan al 5001;
	// 8079: el 8080 está tomado; 65535: no hay próximo.
	if got[3000] != 5001 || got[8079] != 8081 || len(got) != 2 {
		t.Fatalf("alternativas: %v", got)
	}
	// El protocolo: lo que pidió el usuario, o lo que tapa el puerto.
	casos := []struct {
		o    opciones
		tapa ports.Proto
		want []ports.Proto
	}{
		{opciones{}, ports.UDP, []ports.Proto{ports.UDP}},
		{opciones{}, 0, []ports.Proto{ports.TCP}},
		{opciones{tcp: true}, ports.UDP, []ports.Proto{ports.TCP}},
		{opciones{udp: true}, ports.TCP, []ports.Proto{ports.UDP}},
		{opciones{tcp: true, udp: true}, ports.TCP, []ports.Proto{ports.TCP, ports.UDP}},
	}
	for _, c := range casos {
		if got := protosAlternativa(c.o, c.tapa); !slicesEqualProto(got, c.want) {
			t.Errorf("%+v %v: %v", c.o, c.tapa, got)
		}
	}
	// Solo para lo que no quedó libre, y no si se canceló la escalada.
	rep := reaper.Report{Ports: []reaper.PortResult{{Port: 3000, Status: reaper.PortReserved, Proto: ports.TCP}, {Port: 8079, Status: reaper.PortFree}}}
	if got := alternativasDe(ctx, sys, rep, opciones{}); len(got) != 1 || got[3000] != 5001 {
		t.Fatalf("alternativasDe: %v", got)
	}
	rep.Canceled = true
	if got := alternativasDe(ctx, sys, rep, opciones{}); len(got) != 0 {
		t.Fatalf("cancelado: %v", got)
	}
	// En el texto y en el JSON.
	term := tui.Open()
	texto := strings.Join(lineasPuertosAlt(term, reaper.Report{Ports: rep.Ports}, reaper.Plan{}, map[uint16]uint16{3000: 5001}), "\n")
	if !strings.Contains(texto, "el 5001 está libre, si te sirve otro puerto") {
		t.Fatalf("texto:\n%s", texto)
	}
	doc := construirJSONReporte(reaper.Report{Ports: rep.Ports}, reaper.Plan{}, false)
	doc.anotarAlternativas(map[uint16]uint16{3000: 5001})
	if doc.Puertos[0].Alternativa != 5001 || doc.Puertos[1].Alternativa != 0 {
		t.Fatalf("JSON: %+v", doc.Puertos)
	}
}

func slicesEqualProto(a, b []ports.Proto) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --json sale con el mismo código que el texto en la misma situación.
func TestCodigoJSON(t *testing.T) {
	libre := reaper.Report{Ports: []reaper.PortResult{{Port: 3000, Status: reaper.PortFree}}}
	negado := reaper.Report{Ports: []reaper.PortResult{{Port: 445, Status: reaper.PortRefused}}}
	ocupado := reaper.Report{Ports: []reaper.PortResult{{Port: 3000, Status: reaper.PortBusy}}}
	conObjetivo := reaper.Plan{Targets: []reaper.Target{{Kind: reaper.KillProcess}}}
	soloNegativa := reaper.Plan{Refusals: []reaper.Refusal{{PID: 4}}}
	casos := []struct {
		nombre string
		rep    reaper.Report
		plan   reaper.Plan
		o      opciones
		want   int
	}{
		{"ya estaba libre", libre, reaper.Plan{}, opciones{}, cli.ExitOK},
		{"reservado, nadie que cerrar", ocupado, reaper.Plan{}, opciones{}, cli.ExitFailure},
		{"describir lo que se cerraría", ocupado, conObjetivo, opciones{}, cli.ExitOK},
		{"--list", ocupado, conObjetivo, opciones{list: true, yes: true}, cli.ExitOK},
		{"solo una negativa (445)", negado, soloNegativa, opciones{}, cli.ExitFailure},
		{"--yes y quedó ocupado", ocupado, conObjetivo, opciones{yes: true}, cli.ExitFailure},
		{"--yes y quedó libre", libre, conObjetivo, opciones{yes: true}, cli.ExitOK},
		{"--yes y se canceló", reaper.Report{Canceled: true, Ports: ocupado.Ports}, conObjetivo, opciones{yes: true}, cli.ExitInterrupted},
	}
	for _, c := range casos {
		if got := codigoJSON(c.rep, c.plan, c.o); got != c.want {
			t.Errorf("%s: %d, esperaba %d", c.nombre, got, c.want)
		}
	}
}

// --- la espera -----------------------------------------------------------------

// sistemaQueEscucha es un sistema cuya tabla liviana es una historia: el
// socket de este proceso aparece en `desde` y se va en `hasta` (cero =
// nunca), contados desde que se armó.
func sistemaQueEscucha(t *testing.T, local string, desde, hasta time.Duration) *sistema {
	sys := sistemaDeRed(redConBloque(), false)
	yo := win.CurrentProcessID()
	var creado time.Time
	procs, _ := win.AllProcesses()
	for _, p := range procs {
		if p.PID == yo {
			creado = p.Created
		}
	}
	t0 := time.Now()
	s := ports.Socket{Proto: ports.TCP, Local: netip.MustParseAddrPort(local), State: ports.Listen, PID: yo, Since: t0.Add(desde)}
	_ = creado
	sys.escuchando = func(context.Context) ([]ports.Socket, error) {
		d := time.Since(t0)
		if d >= desde && (hasta == 0 || d < hasta) {
			return []ports.Socket{s}, nil
		}
		return nil, nil
	}
	sys.tablas = sys.escuchando
	return sys
}

func TestEsperaEscucha(t *testing.T) {
	sys := sistemaQueEscucha(t, "[::1]:5173", 150*time.Millisecond, 0)
	o := opciones{wait: 5 * time.Second, waitDado: true}
	t0 := time.Now()
	out, code := capturar(t, func(term *tui.Term) int { return correrEspera(term, sys, []string{"5173"}, o) })
	if code != cli.ExitOK || time.Since(t0) > 2*time.Second {
		t.Fatalf("código %d en %v\n%s", code, time.Since(t0), out)
	}
	t.Logf("salida (sin consola):\n%s", out)
	yo := u32(win.CurrentProcessID())
	for _, want := range []string{"Espero a que escuche el 5173", "hasta 5 s", "5173 escucha", "(PID " + yo, "en [::1]", "a los "} {
		if !strings.Contains(out, want) {
			t.Errorf("falta %q en:\n%s", want, out)
		}
	}
}

func TestEsperaVence(t *testing.T) {
	sys := sistemaQueEscucha(t, "[::1]:5173", time.Hour, 0)
	out, code := capturar(t, func(term *tui.Term) int {
		return correrEspera(term, sys, []string{"5173"}, opciones{wait: 300 * time.Millisecond, waitDado: true})
	})
	if code != cli.ExitFailure || !strings.Contains(out, "Venció el plazo") || !strings.Contains(out, "nadie escucha el 5173") {
		t.Fatalf("código %d\n%s", code, out)
	}
}

// Un puerto que tapa WinNAT: --wait no espera en vano, y dice cómo salir.
func TestEsperaImposible(t *testing.T) {
	sys := sistemaQueEscucha(t, "[::1]:5173", time.Hour, 0)
	t0 := time.Now()
	out, code := capturar(t, func(term *tui.Term) int {
		return correrEspera(term, sys, []string{"3000"}, opciones{wait: 30 * time.Second, waitDado: true})
	})
	if code != cli.ExitFailure || time.Since(t0) > 5*time.Second {
		t.Fatalf("código %d en %v\n%s", code, time.Since(t0), out)
	}
	t.Logf("salida (sin consola):\n%s", out)
	for _, want := range []string{"3000 no va a poder escuchar", "bloque 2980–3079", "killport 3000 --reserve", "No espero"} {
		if !strings.Contains(out, want) {
			t.Errorf("falta %q en:\n%s", want, out)
		}
	}
}

func TestEsperaLibre(t *testing.T) {
	sys := sistemaQueEscucha(t, "0.0.0.0:3000", 0, 200*time.Millisecond)
	out, code := capturar(t, func(term *tui.Term) int {
		return correrEspera(term, sys, []string{"3000"}, opciones{waitFree: 5 * time.Second, waitFreeDado: true})
	})
	if code != cli.ExitOK || !strings.Contains(out, "3000 lo ocupa") || !strings.Contains(out, "3000 libre") {
		t.Fatalf("código %d\n%s", code, out)
	}
}

func TestEsperaJSON(t *testing.T) {
	sys := sistemaQueEscucha(t, "127.0.0.1:5173", 100*time.Millisecond, 0)
	out, code := capturar(t, func(term *tui.Term) int {
		return correrEspera(term, sys, []string{"localhost:5173"}, opciones{wait: 5 * time.Second, waitDado: true, json: true})
	})
	var doc jsonEspera
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != cli.ExitOK {
		t.Fatalf("código %d, %v\n%s", code, err, out)
	}
	p := doc.Puertos[0]
	// MS se mide desde que arranca la espera, y el reloj del sistema falso
	// arrancó antes (al armarlo): puede dar menos de los 100 ms del retraso.
	// Lo que importa es que no estuviera escuchando ya y que se haya medido.
	if doc.Espera != "escuchar" || doc.Resultado != "cumplido" || doc.PlazoMS != 5000 || doc.Lecturas < 2 || p.Puerto != 5173 ||
		p.Estado != "cumplido" || p.YaEstaba || p.MS <= 0 || len(p.Destinos) != 1 || p.Destinos[0] != "localhost" ||
		len(p.Quienes) != 1 || p.Quienes[0].PID != win.CurrentProcessID() || p.Quienes[0].Direccion != "127.0.0.1" {
		t.Fatalf("doc: %+v", doc)
	}
	// Vencido: el mismo documento, con código 3.
	sys = sistemaQueEscucha(t, "127.0.0.1:5173", time.Hour, 0)
	out, code = capturar(t, func(term *tui.Term) int {
		return correrEspera(term, sys, []string{"5173"}, opciones{wait: 100 * time.Millisecond, waitDado: true, json: true})
	})
	if json.Unmarshal([]byte(out), &doc) != nil || code != cli.ExitFailure || doc.Resultado != "vencio" || doc.Puertos[0].Estado != "pendiente" {
		t.Fatalf("vencido: código %d\n%s", code, out)
	}
}

// El filtro de la espera: --wait es TCP salvo que se pida otra cosa.
func TestFiltroEspera(t *testing.T) {
	if f := filtroEspera(opciones{}, reaper.WatchListen); !f.TCP || f.UDP {
		t.Fatalf("--wait sin flags: %+v", f)
	}
	if f := filtroEspera(opciones{udp: true}, reaper.WatchListen); f.TCP || !f.UDP {
		t.Fatalf("--wait --udp: %+v", f)
	}
	if f := filtroEspera(opciones{allStates: true}, reaper.WatchFree); f.TCP || f.UDP || !f.AllStates {
		t.Fatalf("--wait-free: %+v", f)
	}
	_ = os.Getpid
}
