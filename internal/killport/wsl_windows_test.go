//go:build windows

package killport

// El cliente de WSL con un wsl.exe de mentira que contesta salidas grabadas
// (esta máquina tiene WSL instalado pero ninguna distro: verificado con
// `wsl -l --running -q`, que sale 0 y vacío), y una prueba contra el wsl.exe
// REAL que confirma que desenmascarar un relay sin distros corriendo no
// nombra ninguna distro (no arranca nada).

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/win"
)

// wslFalso contesta como wsl.exe: la lista de distros y, por distro, la
// salida del sondeo y la del cierre. Registra cada llamada.
type wslFalso struct {
	mu       sync.Mutex
	running  []string
	listErr  error
	sondeo   map[string]string // distro → salida del sondeo
	procesos map[string]string // distro → salida de la segunda ida (/proc)
	cierre   string            // salida del script de cierre
	llamadas [][]string
	scripts  []string
}

func (f *wslFalso) run(ctx context.Context, args []string, stdin []byte, timeout time.Duration) ([]byte, uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.llamadas = append(f.llamadas, args)
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if strings.Join(args, " ") == "-l --running -q" {
		if f.listErr != nil {
			return nil, 0, f.listErr
		}
		return utf16le(strings.Join(f.running, "\r\n")+"\r\n", false), 0, nil
	}
	if len(args) != 7 || args[0] != "-d" || strings.Join(args[2:], " ") != "-u root -e /bin/sh -s" {
		return nil, 1, errors.New("llamada inesperada: " + strings.Join(args, " "))
	}
	distro, script := args[1], string(stdin)
	f.scripts = append(f.scripts, script)
	switch {
	case script == scriptSondeo:
		return []byte(f.sondeo[distro]), 0, nil
	case strings.Contains(script, "kill -INT $P"):
		return []byte(f.cierre), 0, nil
	case strings.HasPrefix(script, scriptEncabezado+"pids="):
		return []byte(f.procesos[distro]), 0, nil
	}
	return nil, 1, errors.New("script inesperado")
}

func (f *wslFalso) distrosNombradas() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.llamadas {
		if len(a) > 1 && a[0] == "-d" {
			out = append(out, a[1])
		}
	}
	return out
}

var viaWSL = reaper.ID{PID: 9120, Created: time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)}

func mascaraWSL(port uint16) reaper.MaskedPort {
	return reaper.MaskedPort{Via: viaWSL, Name: "wslrelay.exe", Relay: reaper.RelayWSL, Port: port, Proto: ports.TCP}
}

// socketsDelRelay: lo que la tabla de Windows le atribuye a wslrelay.
func socketsDelRelay(ps ...uint16) []ports.Socket {
	var out []ports.Socket
	for _, p := range ps {
		out = append(out, sock(ports.TCP, "127.0.0.1", p, ports.Listen, viaWSL.PID, ""))
	}
	return out
}

func TestWSLDesenmascara(t *testing.T) {
	f := &wslFalso{running: []string{"Ubuntu"}, sondeo: map[string]string{"Ubuntu": string(grabado(t, "wsl_sondeo_ss.txt"))}}
	w := &wsl{run: f.run}
	res := w.desenmascarar(context.Background(),
		[]reaper.MaskedPort{mascaraWSL(3000), mascaraWSL(8000), mascaraWSL(8080), mascaraWSL(6000), mascaraWSL(5555)},
		socketsDelRelay(3000, 8000, 8080, 6000, 5555), reaper.NewForest(nil), nil, true)
	if len(res) != 5 {
		t.Fatalf("respuestas = %+v", res)
	}
	node := res[0]
	if node.Verdict != reaper.UnmaskFound || len(node.Behind) != 1 {
		t.Fatalf("3000 = %+v", node)
	}
	if x := node.Behind[0]; x.Kind != reaper.ExtLinux || x.Key != "Ubuntu/812@523400" || x.Name != "node" || x.Where != "Ubuntu" || x.Ref.(*procesoLinux).Cmdline != "node server.js --port 3000" {
		t.Fatalf("externo = %+v", x)
	}
	if res[1].Verdict != reaper.UnmaskFound || res[1].Behind[0].Name != "python3" {
		t.Fatalf("8000 = %+v", res[1])
	}
	// docker-proxy: el reenvío de un Docker DENTRO de Ubuntu; no se toca.
	if p := res[2]; p.Verdict != reaper.UnmaskUnavailable || !strings.Contains(p.Reason, "docker-proxy") || !strings.Contains(p.Advice, "wsl -d Ubuntu docker ps") {
		t.Fatalf("8080 = %+v", p)
	}
	// Hay socket pero ninguna distro corriendo dice de quién es.
	if p := res[3]; p.Verdict != reaper.UnmaskUnavailable || !strings.Contains(p.Reason, "ninguna distro") {
		t.Fatalf("6000 = %+v", p)
	}
	// Nadie escucha el 5555, pero el MISMO relay reenvía cosas vivas: no se
	// ofrece cerrarlo (cortaría el 3000, el 8000…).
	if p := res[4]; p.Verdict != reaper.UnmaskEmpty || p.KillVia || !strings.Contains(p.Advice, "3000") {
		t.Fatalf("5555 = %+v", p)
	}
	// Un solo sondeo por distro y por corrida.
	if n := len(f.distrosNombradas()); n != 1 {
		t.Fatalf("sondeó %d veces", n)
	}
}

// Sin distros corriendo, todo lo que sostiene el relay es un relay colgado,
// y es seguro cerrarlo; y NO se nombra ninguna distro (nombrarla la arrancaría).
func TestWSLZombiSinDistros(t *testing.T) {
	f := &wslFalso{}
	w := &wsl{run: f.run}
	res := w.desenmascarar(context.Background(), []reaper.MaskedPort{mascaraWSL(3000)}, socketsDelRelay(3000, 3001), reaper.NewForest(nil), nil, true)
	if res[0].Verdict != reaper.UnmaskEmpty || !res[0].KillVia || res[0].Advice == "" {
		t.Fatalf("3000 = %+v", res[0])
	}
	if d := f.distrosNombradas(); len(d) != 0 {
		t.Fatalf("nombró distros sin que corrieran: %v", d)
	}
}

// Dos distros: comparten el espacio de red (las dos ven el socket del 3000),
// pero solo Debian lo resuelve a un proceso: el dueño es el de Debian.
func TestWSLDosDistrosComparteRed(t *testing.T) {
	ubuntu := "@navaja 1\n@ss\ntcp LISTEN 0 511 0.0.0.0:3000 0.0.0.0:*\n@clock 1790400000 100\n@fin\n"
	debian := strings.Replace(string(grabado(t, "wsl_sondeo_ss.txt")), "wsl: Se detectó", "aviso", 1)
	f := &wslFalso{running: []string{"Ubuntu", "Debian"}, sondeo: map[string]string{"Ubuntu": ubuntu, "Debian": debian}}
	w := &wsl{run: f.run}
	res := w.desenmascarar(context.Background(), []reaper.MaskedPort{mascaraWSL(3000)}, socketsDelRelay(3000), reaper.NewForest(nil), nil, true)
	if res[0].Verdict != reaper.UnmaskFound || res[0].Behind[0].Where != "Debian" {
		t.Fatalf("3000 = %+v", res[0])
	}
	if d := f.distrosNombradas(); len(d) != 2 {
		t.Fatalf("tenía que sondear las dos: %v", d)
	}
}

// El respaldo /proc (Alpine sin iproute2): dos idas y el mismo resultado.
func TestWSLRespaldoProc(t *testing.T) {
	f := &wslFalso{running: []string{"Alpine"},
		sondeo:   map[string]string{"Alpine": string(grabado(t, "wsl_sondeo_proc1.txt"))},
		procesos: map[string]string{"Alpine": string(grabado(t, "wsl_sondeo_proc2.txt"))}}
	w := &wsl{run: f.run}
	res := w.desenmascarar(context.Background(), []reaper.MaskedPort{mascaraWSL(3000)}, socketsDelRelay(3000), reaper.NewForest(nil), nil, true)
	if res[0].Verdict != reaper.UnmaskFound || res[0].Behind[0].Key != "Alpine/812@523400" {
		t.Fatalf("3000 = %+v", res[0])
	}
	if n := len(f.scripts); n != 2 || !strings.Contains(f.scripts[1], "pids='812'") {
		t.Fatalf("idas = %d: %q", n, f.scripts)
	}
}

// Si WSL no contesta, o una distro falla el sondeo, no se inventa un relay
// colgado: se dice que no se pudo mirar.
func TestWSLNoSePudoMirar(t *testing.T) {
	f := &wslFalso{listErr: errors.New("wsl.exe no está")}
	res := (&wsl{run: f.run}).desenmascarar(context.Background(), []reaper.MaskedPort{mascaraWSL(3000)}, socketsDelRelay(3000), reaper.NewForest(nil), nil, true)
	if res[0].Verdict != reaper.UnmaskUnavailable || res[0].KillVia {
		t.Fatalf("sin lista = %+v", res[0])
	}
	f = &wslFalso{running: []string{"Rota"}, sondeo: map[string]string{"Rota": "sh: not found\n"}}
	res = (&wsl{run: f.run}).desenmascarar(context.Background(), []reaper.MaskedPort{mascaraWSL(3000)}, socketsDelRelay(3000), reaper.NewForest(nil), nil, true)
	if res[0].Verdict != reaper.UnmaskUnavailable || res[0].KillVia || !strings.Contains(res[0].Reason, "Rota") {
		t.Fatalf("sondeo roto = %+v", res[0])
	}
}

// Cerrar: vuelve a listar antes (si la distro se apagó, el proceso ya no
// existe y nombrarla la arrancaría), y traduce la marca del script.
func TestWSLDetener(t *testing.T) {
	p := &procesoLinux{Distro: "Ubuntu", PID: 812, Arranque: 523400, Comm: "node"}
	for _, tc := range []struct {
		running []string
		cierre  string
		want    reaper.Outcome
		method  reaper.Method
		execs   int
	}{
		{[]string{"Ubuntu"}, "@fin INT\n", reaper.OutcomeClosed, reaper.MethodSigInt, 1},
		{[]string{"Ubuntu"}, "@fin TERM\n", reaper.OutcomeClosed, reaper.MethodSigTerm, 1},
		{[]string{"Ubuntu"}, "@fin KILL\n", reaper.OutcomeTerminated, reaper.MethodTerminate, 1},
		{[]string{"Ubuntu"}, "@fin GONE\n", reaper.OutcomeGone, reaper.MethodNone, 1},
		{[]string{"Ubuntu"}, "@fin DENIED\n", reaper.OutcomeDenied, reaper.MethodNone, 1},
		{[]string{"Ubuntu"}, "@fin STUCK\n", reaper.OutcomeError, reaper.MethodNone, 1},
		{[]string{"Ubuntu"}, "basura\n", reaper.OutcomeError, reaper.MethodNone, 1},
		{[]string{"Debian"}, "@fin INT\n", reaper.OutcomeGone, reaper.MethodNone, 0}, // Ubuntu se apagó: no se la nombra
	} {
		f := &wslFalso{running: tc.running, cierre: tc.cierre}
		r, ok := (&wsl{run: f.run}).detener(context.Background(), p, time.Now().Add(3*time.Second), false)
		if !ok || r.Outcome != tc.want || r.Method != tc.method {
			t.Errorf("%v/%q: %v/%v (%v) ok=%v", tc.running, tc.cierre, r.Outcome, r.Method, r.Err, ok)
		}
		if n := len(f.distrosNombradas()); n != tc.execs {
			t.Errorf("%v/%q: %d ejecuciones en la distro, esperaba %d", tc.running, tc.cierre, n, tc.execs)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := (&wsl{run: (&wslFalso{running: []string{"Ubuntu"}}).run}).detener(ctx, p, time.Now(), false); ok {
		t.Fatal("cancelado: el motor tiene que dejarlo pendiente")
	}
}

// Contra el wsl.exe REAL de esta máquina: listar no falla, y desenmascarar un
// relay solo nombra (-d) distros que YA están corriendo: nombrar una detenida
// la arrancaría. La prueba no ejecuta nada adentro de ninguna distro (corta
// las llamadas con -d y las anota): mira QUÉ pidió killport, no lo corre.
//   - sin distros corriendo: una sola llamada (la lista), ninguna con -d, y el
//     relay sin nadie detrás es un relay colgado;
//   - con distros corriendo: exactamente esas, cada una una vez, y ninguna de
//     las instaladas que están detenidas; como la prueba no deja mirar
//     adentro, el veredicto es "no se pudo mirar", nunca "relay colgado".
func TestWSLRealNoArrancaDistros(t *testing.T) {
	if _, err := os.Stat(win.SystemExe("wsl.exe")); err != nil {
		t.Skip("no hay wsl.exe")
	}
	real := nuevoWSL()
	var mu sync.Mutex
	var llamadas [][]string
	w := &wsl{run: func(ctx context.Context, args []string, stdin []byte, timeout time.Duration) ([]byte, uint32, error) {
		mu.Lock()
		llamadas = append(llamadas, args)
		mu.Unlock()
		if len(args) > 0 && args[0] == "-d" {
			return nil, 1, errors.New("la prueba no deja nombrar distros")
		}
		return real.run(ctx, args, stdin, timeout)
	}}
	distros, err := w.corriendo(context.Background())
	if err != nil {
		t.Skipf("WSL no responde en esta máquina: %v", err)
	}
	// Las instaladas (corran o no), para saber cuáles están detenidas.
	out, _, err := real.run(context.Background(), []string{"-l", "-q"}, nil, wslListTimeout)
	if err != nil {
		t.Skipf("WSL no lista las distros instaladas: %v", err)
	}
	var detenidas []string
	for _, d := range distrosCorriendo(out) { // mismo formato: un nombre por línea
		if !slices.Contains(distros, d) {
			detenidas = append(detenidas, d)
		}
	}
	t.Logf("corriendo: %v · detenidas: %v", distros, detenidas)

	llamadas = nil
	res := w.desenmascarar(context.Background(), []reaper.MaskedPort{mascaraWSL(3000)}, socketsDelRelay(3000), reaper.NewForest(nil), nil, true)
	var nombradas []string
	listas := 0
	for _, a := range llamadas {
		switch {
		case strings.Join(a, " ") == "-l --running -q":
			listas++
		case len(a) > 1 && a[0] == "-d":
			nombradas = append(nombradas, a[1])
		default:
			t.Fatalf("llamada inesperada a wsl.exe: %q", a)
		}
	}
	if listas != 1 {
		t.Fatalf("esperaba UNA consulta de qué distros corren, hubo %d: %q", listas, llamadas)
	}
	for _, d := range nombradas {
		if slices.Contains(detenidas, d) || !slices.Contains(distros, d) {
			t.Fatalf("killport nombró a %q, que no estaba corriendo: la habría arrancado (llamadas: %q)", d, llamadas)
		}
	}
	slices.Sort(nombradas)
	esperadas := slices.Clone(distros)
	slices.Sort(esperadas)
	if !slices.Equal(nombradas, esperadas) {
		t.Fatalf("distros nombradas = %v, esperaba exactamente las que corren, una vez cada una: %v", nombradas, esperadas)
	}
	if len(distros) == 0 {
		if res[0].Verdict != reaper.UnmaskEmpty || !res[0].KillVia {
			t.Fatalf("sin distros, el relay está colgado: %+v", res[0])
		}
		return
	}
	if res[0].Verdict != reaper.UnmaskUnavailable || res[0].KillVia {
		t.Fatalf("no se pudo mirar adentro: no se puede declarar colgado al relay: %+v", res[0])
	}
}
