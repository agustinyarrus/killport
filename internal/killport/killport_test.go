//go:build windows

package killport

import (
	"encoding/json"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

var t0 = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func sock(proto ports.Proto, addr string, port uint16, state ports.State, pid uint32, svc string) ports.Socket {
	return ports.Socket{Proto: proto, Local: netip.AddrPortFrom(netip.MustParseAddr(addr), port), State: state, PID: pid, Service: svc, Since: t0}
}

// --- fusión v4+v6 en el listado --------------------------------------------

// El mismo servidor escuchando en 0.0.0.0 y en [::] es UNA fila "v4+v6".
func TestListadoFusionaFamilias(t *testing.T) {
	f := reaper.NewForest([]reaper.Proc{{ID: reaper.ID{PID: 100, Created: t0}, Name: "node.exe", Path: `C:\proy\node.exe`}})
	socks := []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 3000, ports.Listen, 100, ""),
		sock(ports.TCP, "::", 3000, ports.Listen, 100, ""),
	}
	filas := filasDeListado(socks, f, nil, reaper.Config{SystemRoot: `C:\Windows`})
	if len(filas) != 1 {
		t.Fatalf("esperaba 1 fila fusionada, hubo %d: %+v", len(filas), filas)
	}
	if !filas[0].Familias.v4 || !filas[0].Familias.v6 {
		t.Fatalf("la fila debería tener v4 y v6: %+v", filas[0].Familias)
	}
	if filas[0].Familias.etiqueta() != "∗ v4+v6" {
		t.Fatalf("etiqueta = %q", filas[0].Familias.etiqueta())
	}
}

// Dos procesos distintos en el mismo puerto NO se fusionan.
func TestListadoNoFusionaDistintoDueño(t *testing.T) {
	f := reaper.NewForest([]reaper.Proc{
		{ID: reaper.ID{PID: 100, Created: t0}, Name: "node.exe", Path: `C:\a\node.exe`},
		{ID: reaper.ID{PID: 200, Created: t0}, Name: "python.exe", Path: `C:\b\python.exe`},
	})
	socks := []ports.Socket{
		sock(ports.TCP, "127.0.0.1", 3000, ports.Listen, 100, ""),
		sock(ports.TCP, "0.0.0.0", 3000, ports.Listen, 200, ""),
	}
	filas := filasDeListado(socks, f, nil, reaper.Config{SystemRoot: `C:\Windows`})
	if len(filas) != 2 {
		t.Fatalf("dueños distintos: esperaba 2 filas, hubo %d", len(filas))
	}
}

// Lo de Windows se oculta salvo --system; un servicio de terceros no.
func TestVisiblesOcultaWindows(t *testing.T) {
	f := reaper.NewForest(conWindows(
		reaper.Proc{ID: reaper.ID{PID: 100, Created: t0}, Name: "node.exe", Path: `C:\proy\node.exe`},
		svchostGenuino(900),
	))
	socks := []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 3000, ports.Listen, 100, ""),
		sock(ports.TCP, "0.0.0.0", 445, ports.Listen, 4, ""),
		sock(ports.TCP, "0.0.0.0", 135, ports.Listen, 900, "RpcSs"),
	}
	filas := filasDeListado(socks, f, nil, reaper.Config{SystemRoot: `C:\Windows`})
	muestra, ocultas := visibles(filas, mostrar{})
	if len(muestra) != 1 || muestra[0].Port != 3000 {
		t.Fatalf("sin --system solo debería verse el 3000: %+v", muestra)
	}
	if ocultas.filas != 2 {
		t.Fatalf("esperaba 2 ocultas, hubo %d", ocultas.filas)
	}
	todo, _ := visibles(filas, mostrar{system: true, efimeros: true})
	if len(todo) != 3 {
		t.Fatalf("con --system deberían verse las 3: %+v", todo)
	}
}

// Un cliente UDP efímero (puerto elegido por el sistema) es ruido: se oculta.
func TestVisiblesOcultaEfimero(t *testing.T) {
	f := reaper.NewForest([]reaper.Proc{{ID: reaper.ID{PID: 100, Created: t0}, Name: "app.exe", Path: `C:\a\app.exe`}})
	s := sock(ports.UDP, "0.0.0.0", 55123, ports.StateNone, 100, "")
	s.Ephemeral = true
	filas := filasDeListado([]ports.Socket{s}, f, nil, reaper.Config{SystemRoot: `C:\Windows`})
	if m, _ := visibles(filas, mostrar{}); len(m) != 0 {
		t.Fatalf("un cliente UDP efímero debería ocultarse: %+v", m)
	}
	// No es de Windows: --system no lo trae (X-01); --all-states sí (son
	// sockets de clientes, como las conexiones).
	if m, _ := visibles(filas, mostrar{system: true}); len(m) != 0 {
		t.Fatalf("con --system solo, un cliente UDP no es lo de Windows")
	}
	if m, _ := visibles(filas, mostrar{efimeros: true}); len(m) != 1 {
		t.Fatalf("con --all-states debería verse")
	}
}

// X-01: el pie cuenta PROCESOS de Windows (un svchost con tres puertos es
// uno) y, aparte, los sockets de clientes UDP (que no son de Windows).
func TestPieOcultasCuentaProcesos(t *testing.T) {
	f := reaper.NewForest(conWindows(
		reaper.Proc{ID: reaper.ID{PID: 100, Created: t0}, Name: "chrome.exe", Path: `C:\Program Files\Google\Chrome\chrome.exe`},
		svchostGenuino(900),
	))
	efimero := func(p uint16) ports.Socket {
		s := sock(ports.UDP, "0.0.0.0", p, ports.StateNone, 100, "")
		s.Ephemeral = true
		return s
	}
	socks := []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 135, ports.Listen, 900, "RpcSs"),
		sock(ports.TCP, "0.0.0.0", 5040, ports.Listen, 900, "RpcSs"),
		sock(ports.UDP, "0.0.0.0", 5050, ports.StateNone, 900, "RpcSs"),
		efimero(55001), efimero(55002),
	}
	_, oc := visibles(filasDeListado(socks, f, nil, reaper.Config{SystemRoot: `C:\Windows`}), mostrar{})
	if oc.filas != 5 || oc.windows != 1 || oc.efimeros != 2 {
		t.Fatalf("ocultas = %+v", oc)
	}
	if got := pieOcultas(oc); got != "1 proceso de Windows oculto · --system los muestra; 2 sockets UDP de clientes ocultos · --all-states los muestra" {
		t.Fatalf("pie = %q", got)
	}
}

// --- JSON estable -----------------------------------------------------------

func TestJSONReporteEstable(t *testing.T) {
	rep := reaper.Report{
		Victims: []reaper.VictimResult{
			{ID: reaper.ID{PID: 100, Created: t0}, Name: "node.exe", Outcome: reaper.OutcomeClosed, Method: reaper.MethodCtrlC, Elapsed: 420 * time.Millisecond},
		},
		Ports: []reaper.PortResult{
			{Port: 3000, Proto: ports.TCP, Status: reaper.PortFree},
			{Port: 445, Proto: ports.TCP, Status: reaper.PortRefused, Owner: reaper.ID{PID: 4}, OwnerName: "System", Reason: "es el kernel de Windows", Advice: "no hay proceso que cerrar"},
		},
	}
	doc := construirJSONReporte(rep, reaper.Plan{}, false)
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	// Re-parsea (como haría ConvertFrom-Json) y se comprueban claves estables.
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("no re-parsea: %v", err)
	}
	if back["libres"].(float64) != 1 || back["total"].(float64) != 2 {
		t.Fatalf("libres/total = %v/%v", back["libres"], back["total"])
	}
	puertos := back["puertos"].([]any)
	p0 := puertos[0].(map[string]any)
	if p0["estado"] != "libre" {
		t.Fatalf("estado 3000 = %v", p0["estado"])
	}
	p1 := puertos[1].(map[string]any)
	if p1["estado"] != "no_se_toca" || p1["motivo"] == "" {
		t.Fatalf("445 = %+v", p1)
	}
	v0 := back["victimas"].([]any)[0].(map[string]any)
	if v0["destino"] != "cerrado" || v0["metodo"] != "Ctrl+C" || v0["ms"].(float64) != 420 {
		t.Fatalf("víctima = %+v", v0)
	}
}

// Lo que sale por --json se lee y se copia: los comandos que lleva adentro no
// pueden salir con el escapado para HTML (el "&" como \u0026, el "<" como
// \u003c) que encoding/json aplica de fábrica.
func TestJSONSinEscapadoHTML(t *testing.T) {
	rep := reaper.Report{Ports: []reaper.PortResult{{Port: 3000, Proto: ports.TCP, Status: reaper.PortReserved,
		Reason: "cae en un bloque de WinNAT", Advice: "killport <puerto> --reserve",
		Remedies: []reaper.Remedy{{Steps: []string{"net stop winnat", "net start winnat"}, Effect: "a > b"}}}}}
	b, err := textoJSON(construirJSONReporte(rep, reaper.Plan{}, true))
	if err != nil {
		t.Fatal(err)
	}
	texto := string(b)
	for _, dice := range []string{`"consejo": "killport <puerto> --reserve"`, `net stop winnat && net start winnat`, `"efecto": "a > b"`} {
		if !strings.Contains(texto, dice) {
			t.Errorf("el JSON no dice %s:\n%s", dice, texto)
		}
	}
	if strings.Contains(texto, `\u00`) {
		t.Errorf("el JSON salió escapado para HTML:\n%s", texto)
	}
	if !strings.HasSuffix(texto, "}\n") || strings.HasSuffix(texto, "\n\n") {
		t.Errorf("el documento tiene que terminar en UN salto de línea: %q", texto[len(texto)-4:])
	}
	var back jsonReporte
	if err := json.Unmarshal(b, &back); err != nil || back.Puertos[0].Remedios[0].Comando != comandoRemedio(rep.Ports[0].Remedies[0]) {
		t.Errorf("no re-parsea igual: %v", err)
	}
}

// --- códigos de salida ------------------------------------------------------

func TestCodigoDeSalida(t *testing.T) {
	libre := reaper.PortResult{Port: 1, Status: reaper.PortFree}
	ocupado := reaper.PortResult{Port: 2, Status: reaper.PortBusy}
	for _, tc := range []struct {
		nombre string
		rep    reaper.Report
		quiero int
	}{
		{"todo libre", reaper.Report{Ports: []reaper.PortResult{libre, libre}}, 0},
		{"sin puertos", reaper.Report{}, 0},
		{"parcial", reaper.Report{Ports: []reaper.PortResult{libre, ocupado}}, 1},
		{"ninguno", reaper.Report{Ports: []reaper.PortResult{ocupado, ocupado}}, 3},
		{"cancelado", reaper.Report{Canceled: true, Ports: []reaper.PortResult{libre}}, 130},
	} {
		if got := codigoDeSalida(tc.rep); got != tc.quiero {
			t.Errorf("%s: código %d, esperaba %d", tc.nombre, got, tc.quiero)
		}
	}
}

// --- confirmación -----------------------------------------------------------

func TestEsSi(t *testing.T) {
	for _, s := range []string{"s", "S", "si", "sí", "y", "yes"} {
		if !esSi(s) {
			t.Errorf("%q debería ser sí", s)
		}
	}
	for _, s := range []string{"", "n", "no", "x", "0"} {
		if esSi(s) {
			t.Errorf("%q NO debería ser sí", s)
		}
	}
}

// (La ida y vuelta del trabajo elevado está en trabajo_test.go.)

func TestCodificarPuertos(t *testing.T) {
	s := ports.NewSet(ports.R(3000, 3000), ports.R(5000, 5003))
	if got := specDe(s); got != "3000,5000-5003" {
		t.Fatalf("specDe = %q", got)
	}
	// Re-parsea por el camino real del usuario.
	back, err := ports.ParseSpec("3000,5000-5003")
	if err != nil || !back.Equal(s) {
		t.Fatalf("no re-parsea: %v / %v", back, err)
	}
}

// --- identidades por texto (ayudante y trabajo) ----------------------------

func TestIDsRoundTrip(t *testing.T) {
	ids := []reaper.ID{{PID: 100, Created: t0}, {PID: 7, Created: time.Time{}}}
	got := decodeIDs(idsEncoded(ids))
	if len(got) != 2 || got[0].PID != 100 || !got[0].Created.Equal(t0) || got[1].PID != 7 || !got[1].Created.IsZero() {
		t.Fatalf("ida y vuelta de IDs: %+v", got)
	}
}

// --- ayuda: los flags se registran sin pánico y la ayuda sale --------------

func TestAyudaSale(t *testing.T) {
	term := tui.Open()
	if code := Main(term, "test", []string{"--help"}); code != 0 {
		t.Fatalf("--help debería salir 0, dio %d", code)
	}
}

// La ayuda entra en una consola de 80, 100 y 120 columnas: ningún renglón se
// sale del borde (los flags, los ejemplos y las notas se parten).
func TestAyudaEntraEnElAncho(t *testing.T) {
	for _, w := range []int{80, 100, 120} {
		out, code := capturarAncho(t, w, func(term *tui.Term) int { return Main(term, "1.0.0", []string{"--help"}) })
		if code != 0 {
			t.Fatalf("--help: código %d", code)
		}
		for _, l := range strings.Split(out, "\n") {
			if tui.Width(l) > w {
				t.Errorf("ancho %d: renglón de %d columnas: %q", w, tui.Width(l), l)
			}
		}
		for _, secc := range []string{"qué hacer", "elegir sin nombrar puertos", "un puerto libre", "esperar", "puertos que reserva Windows", "qué mirar", "salida", "ejemplos", "notas"} {
			if !strings.Contains(out, "\n  "+secc) {
				t.Errorf("ancho %d: falta la sección %q", w, secc)
			}
		}
	}
}

// --- flags: --json sin --yes no debe requerir consola ----------------------

func TestFiltroDeFlags(t *testing.T) {
	o := opciones{tcp: true}
	if !o.filtro().TCP || o.filtro().UDP {
		t.Fatal("filtro TCP")
	}
	o = opciones{allStates: true}
	if !o.filtro().AllStates {
		t.Fatal("filtro all-states")
	}
	if strings.Contains("x", "y") {
		t.Fatal("imposible")
	}
}

// X-33: el orden de las filas de un mismo puerto no depende del orden de la
// tabla de Windows (que cambia entre corridas): puerto, protocolo, PID.
func TestListadoOrdenDeterminista(t *testing.T) {
	var procs []reaper.Proc
	var socks []ports.Socket
	for _, pid := range []uint32{35068, 9148, 33492, 3700} {
		procs = append(procs, reaper.Proc{ID: reaper.ID{PID: pid, Created: t0.Add(-time.Hour)}, Name: "app.exe", Path: `C:\a\app.exe`})
		socks = append(socks, sock(ports.UDP, "0.0.0.0", 5353, ports.StateNone, pid, ""), sock(ports.TCP, "127.0.0.1", 3000+uint16(pid%7), ports.Listen, pid, ""))
	}
	f := reaper.NewForest(procs)
	cfg := reaper.Config{SystemRoot: `C:\Windows`}
	orden := func(ss []ports.Socket) string {
		var out []string
		for _, fl := range filasDeListado(ss, f, nil, cfg) {
			out = append(out, fl.Proto.String()+"/"+u32(uint32(fl.Port))+"/"+u32(fl.PID))
		}
		return strings.Join(out, " ")
	}
	want := orden(socks)
	if !strings.Contains(want, "UDP/5353/3700 UDP/5353/9148 UDP/5353/33492 UDP/5353/35068") {
		t.Fatalf("orden = %s", want)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 200 {
		ss := slices.Clone(socks)
		rng.Shuffle(len(ss), func(a, b int) { ss[a], ss[b] = ss[b], ss[a] })
		if got := orden(ss); got != want {
			t.Fatalf("vuelta %d: %s, esperaba %s", i, got, want)
		}
	}
}

// X-15 y X-31: la ayuda de una herramienta de Windows habla PowerShell (bash
// al lado), --timeout está con lo que hace, el listado dice que pregunta, y
// un flag desconocido da UNA pista.
func TestAyudaX15X31(t *testing.T) {
	out, _ := capturar(t, func(term *tui.Term) int { return Main(term, "test", []string{"--help"}) })
	i, j := strings.Index(out, "qué hacer"), strings.Index(out, "elegir sin nombrar")
	k := strings.Index(out, "--timeout")
	if !strings.Contains(out, "$port = killport --free 5173") || !strings.Contains(out, "pregunta cuál liberar") ||
		i < 0 || j < 0 || k < i || k > j || strings.Count(out, "killport --free 3000") > 1 {
		t.Fatalf("ayuda:\n%s", out)
	}
	out, code := capturar(t, func(term *tui.Term) int { return Main(term, "test", []string{"--bogus"}) })
	if code != 2 || strings.Count(out, "--help") != 1 {
		t.Fatalf("flag desconocido (código %d):\n%s", code, out)
	}
	libre := pistaScript([]string{"3000"}, opciones{})
	if !strings.Contains(libre, "$port = killport --free 3000") || !strings.Contains(libre, "en bash, PORT=$(killport --free 3000)") {
		t.Fatalf("pista de --free: %q", libre)
	}
}
