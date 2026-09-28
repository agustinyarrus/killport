//go:build windows

package killport

import (
	"math/rand"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/agustinyarrus/killport/internal/tui"
)

// argvWindows es el oráculo: CommandLineToArgvW de shell32.
func argvWindows(t *testing.T, cmd string) []string {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(cmd)
	if err != nil {
		t.Fatal(err)
	}
	var argc int32
	argv, err := syscall.CommandLineToArgv(p, &argc)
	if err != nil {
		t.Fatalf("CommandLineToArgvW(%q): %v", cmd, err)
	}
	defer syscall.LocalFree(syscall.Handle(unsafe.Pointer(argv)))
	out := make([]string, argc)
	for i := range out {
		out[i] = syscall.UTF16ToString((*argv[i])[:])
	}
	return out
}

// argvDe parte igual que Windows: casos elegidos y 30.000 líneas al azar
// hechas de lo que complica (comillas, barras, blancos) contra
// CommandLineToArgvW.
func TestArgvComoWindows(t *testing.T) {
	casos := []string{
		`"C:\Program Files\nodejs\node.exe" server.js 3000`,
		`"node"   "C:\Users\mauro\AppData\Roaming\npm\\node_modules\@ejemplo\notas\bin\notas.js" `,
		`C:\Python310\python.exe -m http.server 8123 --bind 127.0.0.1`,
		`a "b c" d`, `a "b""c" d`, `a \"b`, `a \\"b c"`, `a \\\"b`, `a b\\`, `"a b"c d`, `"abc`, `a "b`,
		"a\tb  \t c", `a ""`, `a "" b`, `x "a"""b"`, `x """"`, `ñandú "ñ ú" \\ñ`,
	}
	for _, c := range casos {
		if got, want := argvDe(c), argvWindows(t, c); !reflect.DeepEqual(got, want) {
			t.Errorf("argvDe(%q) = %q, Windows = %q", c, got, want)
		}
	}
	rng := rand.New(rand.NewSource(20260928))
	piezas := []string{"a", "b", " ", "\t", `"`, `\`, `\\`, `""`, "ñ", "x y"}
	for i := 0; i < 30000; i++ {
		var b strings.Builder
		for j := 0; j < 1+rng.Intn(12); j++ {
			b.WriteString(piezas[rng.Intn(len(piezas))])
		}
		c := b.String()
		if strings.TrimLeft(c, " \t") == "" {
			continue // una línea en blanco: Windows devuelve la ruta del que llama
		}
		if got, want := argvDe(c), argvWindows(t, c); !reflect.DeepEqual(got, want) {
			t.Fatalf("argvDe(%q) = %q, Windows = %q", c, got, want)
		}
	}
}

// Líneas de comandos como las de una máquina de desarrollo, legibles.
func TestComandoLegible(t *testing.T) {
	home := `C:\Users\mauro`
	casos := []struct {
		cmd, cwd, want string
	}{
		{`"C:\Program Files\nodejs\node.exe" server.js`, `C:\Users\mauro\dev\sitios\tienda`, "node server.js"},
		{`"C:\Program Files\nodejs\node.exe" C:\Users\mauro\dev\web\server.js 3000`, `C:\Users\mauro\dev\web`, "node server.js 3000"},
		{`"node"   "C:\Users\mauro\AppData\Roaming\npm\\node_modules\@ejemplo\notas\bin\notas.js" `, home, `node @ejemplo\notas\bin\notas.js`},
		{`node  C:/Users/mauro/dev/scripts/reloj-cli/reloj.js`, `C:\`, `node ~\dev\scripts\reloj-cli\reloj.js`},
		{`C:\Python310\python.exe -m http.server 8123 --bind 127.0.0.1`, `C:\Users\mauro`, `python -m http.server 8123 --bind 127.0.0.1`},
		{`"C:\Program Files\NVIDIA Corporation\NVIDIA App\CEF\NVIDIA Overlay.exe"`, ``, `NVIDIA Overlay`},
		{`adb -L tcp:5037 fork-server server --reply-fd 784`, ``, `adb -L tcp:5037 fork-server server --reply-fd 784`},
		{`C:\Users\mauro\llama\llama-server.exe -m C:\Users\mauro\models\q.gguf --config=C:\Users\mauro\llama\c.json --jinja`, `C:\Users\mauro\llama`,
			`llama-server -m ~\models\q.gguf --config=c.json --jinja`},
		{`"C:\Program Files\Go\bin\go.exe"`, ``, `go`},
		// La carpeta del proceso es el perfil: "relativo" no aclararía nada.
		{`node C:\Users\mauro\scripts\x.js`, home, `node ~\scripts\x.js`},
		{`relayfalso.EXE -modo wsl`, ``, `relayfalso -modo wsl`},
		{``, ``, ``},
	}
	for _, c := range casos {
		got, keep := comandoLegible(c.cmd, rutasDe{cwd: c.cwd, home: home})
		if got != c.want {
			t.Errorf("comandoLegible(%q) = %q, esperaba %q", c.cmd, got, c.want)
		}
		// keep cubre exactamente el programa (y el espacio, si hay argumentos),
		// o el programa y el módulo de "-m".
		if got != "" && (keep > len(got) || strings.TrimRight(got[:keep], " ") != programa(argvDe(c.cmd)[0])) &&
			!strings.Contains(got[:keep], " -m ") {
			t.Errorf("%q: keep = %d", got, keep)
		}
	}
	// El recorte respeta el programa: el de C4 era `"C:\Pro…rver.js`.
	txt, keep := comandoLegible(`"C:\Program Files\nodejs\node.exe" C:\otro\lado\con\muchas\carpetas\server.js 3000`, rutasDe{home: home})
	if got := tui.TruncateMiddleKeep(txt, keep, 24); !strings.HasPrefix(got, "node ") || !strings.HasSuffix(got, "3000") || tui.Width(got) != 24 {
		t.Errorf("recortado: %q", got)
	}
}

// X-11 / X-03: la forma corta deja el programa y el SCRIPT (o el módulo) a
// la vista: dos servidores del mismo programa no quedan iguales salvo las
// últimas letras.
func TestComandoCompacto(t *testing.T) {
	for _, c := range []struct{ cmd, want, keep string }{
		{`"C:\Program Files\nodejs\node.exe" C:\Users\mauro\dev\_pruebas\killport\revision\x\srv.js normal 0.0.0.0 0 A C:\Users\mauro\dev\_pruebas\killport\revision\x\m-L4.txt`,
			`node srv.js normal 0.0.0.0 0 A m-L4.txt`, `node srv.js `},
		{`C:\Python310\python.exe -m http.server 8123 --bind 127.0.0.1`, `python -m http.server 8123 --bind 127.0.0.1`, `python -m http.server `},
		{`llama-server.exe -m C:\Users\a\models\modelo.gguf --port 8099`, `llama-server -m modelo.gguf --port 8099`, `llama-server -m modelo.gguf `},
		{`adb.exe -L tcp:5037 fork-server server --reply-fd 784`, `adb -L tcp:5037 fork-server server --reply-fd 784`, `adb `},
		{`node --config=C:\dev\x\cfg.json http://localhost:3000/a/b`, `node --config=cfg.json http://localhost:3000/a/b`, `node `},
		{`app.exe`, `app`, `app`},
	} {
		got, keep := comandoCompacto(c.cmd)
		if got != c.want || got[:keep] != c.keep {
			t.Errorf("comandoCompacto(%q) = %q (respeta %q), esperaba %q (respeta %q)", c.cmd, got, got[:keep], c.want, c.keep)
		}
	}
	// A 30 columnas, dos servidores de srv.js se siguen distinguiendo.
	a, ka := comandoCompacto(`node C:\x\srv.js normal 0.0.0.0 0 A C:\x\m-L4.txt`)
	b, kb := comandoCompacto(`node C:\x\otro.js normal 0.0.0.0 0 A C:\x\m-L6.txt`)
	if ra, rb := tui.TruncateMiddleKeep(a, ka, 20), tui.TruncateMiddleKeep(b, kb, 20); !strings.HasPrefix(ra, "node srv.js") || !strings.HasPrefix(rb, "node otro.js") {
		t.Errorf("recortados: %q / %q", ra, rb)
	}
}

func TestUnaLinea(t *testing.T) {
	if got := unaLinea("a\r\nb\tc\x00d"); got != "a  b cd" {
		t.Errorf("unaLinea = %q", got)
	}
}
