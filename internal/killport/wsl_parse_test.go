package killport

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/agustinyarrus/killport/internal/ports"
)

// utf16le codifica como lo hace wsl.exe (sin BOM, CRLF).
func utf16le(s string, bom bool) []byte {
	var b []byte
	if bom {
		b = append(b, 0xFF, 0xFE)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}

// La salida de `wsl -l --running -q` en todas sus formas: UTF-16LE con y sin
// BOM, UTF-8 (WSL_UTF8=1), vacía (sin distros: capturado en esta máquina) y
// con nombres que wsl.exe tomaría como opción (descartados).
func TestDistrosCorriendo(t *testing.T) {
	for _, tc := range []struct {
		nombre string
		in     []byte
		want   []string
	}{
		{"vacía (sin distros)", nil, nil},
		{"utf16", utf16le("Ubuntu-24.04\r\ndocker-desktop\r\n", false), []string{"Ubuntu-24.04", "docker-desktop"}},
		{"utf16 con bom", utf16le("Debian\r\n", true), []string{"Debian"}},
		{"utf8", []byte("Ubuntu\r\nAlpine\n"), []string{"Ubuntu", "Alpine"}},
		{"nombre raro", []byte("--shutdown\r\nOK\r\n\x01mal\r\n  \r\n"), []string{"OK"}},
		{"una sola letra utf16", utf16le("A\r\n", false), []string{"A"}},
	} {
		got := distrosCorriendo(tc.in)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: %q, esperaba %q", tc.nombre, got, tc.want)
		}
	}
	// Grabado real (sin -q, en español): UTF-16LE sin BOM.
	if got := strings.TrimSpace(decodificarWSL(grabado(t, "wsl_l_running_es_utf16.bin"))); got != "No hay distribuciones en ejecución." {
		t.Fatalf("decodificado = %q", got)
	}
}

// El sondeo con ss: los avisos de wsl.exe antes de la marca se ignoran, cada
// dueño queda con sus sockets e identidad, el nombre con paréntesis se lee
// hasta el ÚLTIMO ')', y el socket sin dueño (otra distro) queda en Sockets.
func TestParseSondeoSS(t *testing.T) {
	sd, faltan, err := parseSondeo("Ubuntu", string(grabado(t, "wsl_sondeo_ss.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if sd.Fuente != "ss" || len(faltan) != 0 || len(sd.Sockets) != 8 {
		t.Fatalf("fuente %q, faltan %v, sockets %d", sd.Fuente, faltan, len(sd.Sockets))
	}
	by := map[uint32]procesoLinux{}
	for _, p := range sd.Procesos {
		by[p.PID] = p
	}
	if len(by) != 6 {
		t.Fatalf("procesos = %+v", sd.Procesos)
	}
	node := by[812]
	if node.Comm != "node" || node.Arranque != 523400 || node.Usuario != "agus" || node.UID != 1000 || node.Cmdline != "node server.js --port 3000" {
		t.Fatalf("node = %+v", node)
	}
	if !node.puertos().Equal(ports.Of(3000)) || len(node.Sockets) != 2 {
		t.Fatalf("node escucha %v (%d sockets)", node.puertos(), len(node.Sockets))
	}
	if want := time.Unix(1790400000+5234, 0); !node.Desde.Equal(want) {
		t.Fatalf("desde = %v, esperaba %v (btime + 523400/100 s)", node.Desde, want)
	}
	if node.clave() != "Ubuntu/812@523400" {
		t.Fatalf("clave = %q", node.clave())
	}
	if raro := by[1400]; raro.Comm != "(raro) x" || raro.Arranque != 720000 {
		t.Fatalf("raro = %+v", raro)
	}
	if !relaysDeLinux[by[2100].Comm] {
		t.Fatal("docker-proxy es un reenvío de contenedores dentro de Linux")
	}
	var sinDueno bool
	for _, s := range sd.Sockets {
		if s.Port() == 6000 && len(s.Users) == 0 {
			sinDueno = true
		}
	}
	if !sinDueno {
		t.Fatal("el socket del 6000 (sin dueño visible) tiene que quedar en Sockets")
	}
}

// El respaldo /proc: primera ida con tablas y mapa de inodos (los que
// escuchan llevan al PID 812; la conexión establecida del 813 no), segunda
// ida con la descripción.
func TestParseSondeoProc(t *testing.T) {
	sd, faltan, err := parseSondeo("Alpine", string(grabado(t, "wsl_sondeo_proc1.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if sd.Fuente != "/proc" || len(sd.Sockets) != 2 || fmt.Sprint(faltan) != "[812]" {
		t.Fatalf("fuente %q, sockets %d, faltan %v", sd.Fuente, len(sd.Sockets), faltan)
	}
	if !strings.Contains(scriptProcesos(faltan), "pids='812'") {
		t.Fatalf("segunda ida = %q", scriptProcesos(faltan))
	}
	sd, err = completarSondeo(sd, string(grabado(t, "wsl_sondeo_proc2.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if len(sd.Procesos) != 1 || sd.Procesos[0].PID != 812 || len(sd.Procesos[0].Sockets) != 2 || sd.Procesos[0].Distro != "Alpine" {
		t.Fatalf("procesos = %+v", sd.Procesos)
	}
}

func TestParseSondeoErrores(t *testing.T) {
	for nombre, out := range map[string]string{
		"sin marca":     "sh: not found\n",
		"cortado":       "@navaja 1\n@ss\ntcp LISTEN 0 1 0.0.0.0:1 0.0.0.0:*\n",
		"sin fuente":    "@navaja 1\n@clock 1 100\n@fin\n",
		"ss ilegible":   "@navaja 1\n@ss\nbasura total\n@fin\n",
		"proc ilegible": "@navaja 1\n@proc\n@net tcp\n  sl local\nbasura\n@fin\n",
	} {
		if _, _, err := parseSondeo("X", out); err == nil {
			t.Errorf("%s: esperaba error", nombre)
		} else if !errors.Is(err, ErrSondeo) && !errors.Is(err, ports.ErrLinuxFormat) {
			t.Errorf("%s: error inesperado %v", nombre, err)
		}
	}
	// Un proceso sin hora de arranque no tiene identidad: no se lo atribuye
	// (no se lo podría cerrar sin riesgo de pegarle a un PID reciclado).
	sd, _, err := parseSondeo("X", "@navaja 1\n@ss\ntcp LISTEN 0 1 0.0.0.0:3000 0.0.0.0:* users:((\"a\",pid=5,fd=1))\n@clock 1 100\n@pid 5\nS 5 (a) S\nC a\n@fin\n")
	if err != nil || len(sd.Procesos) != 0 || len(sd.Sockets) != 1 {
		t.Fatalf("sin identidad = %+v / %v", sd, err)
	}
}

func TestParseStat(t *testing.T) {
	tail := " S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 424242 20 21"
	for in, want := range map[string]string{
		"9 (node)" + tail:          "node",
		"9 (a b)" + tail:           "a b",
		"9 ((x) y)" + tail:         "(x) y",
		"9 (con ) adentro)" + tail: "con ) adentro",
	} {
		comm, start, ok := parseStat(in)
		if !ok || comm != want || start != 424242 {
			t.Errorf("%q → %q %d %v", in, comm, start, ok)
		}
	}
	for _, in := range []string{"", "9 node S 1", "9 (x) S 1 2 3", "9 (x" + tail} {
		if _, _, ok := parseStat(in); ok {
			t.Errorf("%q debió fallar", in)
		}
	}
}

// El script de cierre lleva la identidad y los plazos exactos, y con --force
// va directo a SIGKILL.
func TestScriptCierre(t *testing.T) {
	p := procesoLinux{Distro: "Ubuntu", PID: 812, Arranque: 523400}
	s := scriptCierre(p, 3*time.Second, false)
	for _, want := range []string{"P=812; T=523400; N=60; M=20; K=40; F=0", "kill -INT $P", "kill -TERM $P", "kill -KILL $P", `"${20}"`} {
		if !strings.Contains(s, want) {
			t.Errorf("falta %q en el script", want)
		}
	}
	if !strings.Contains(scriptCierre(p, 0, true), "F=1") || !strings.Contains(scriptCierre(p, 10*time.Millisecond, false), "N=1;") {
		t.Fatal("force / plazo mínimo de un paso")
	}
	if strings.ContainsAny(s, "\x00") {
		t.Fatal("el script no puede llevar bytes nulos")
	}
	if plazoCierre(3*time.Second) != 3*time.Second+graciaTerm+graciaKill+margenScript {
		t.Fatal("plazo del lado de Windows")
	}
}

func TestResultadoCierre(t *testing.T) {
	for in, want := range map[string]string{
		"@fin INT\n":             "INT",
		"aviso\r\n@fin TERM\r\n": "TERM",
		"@fin KILL":              "KILL",
		"  @fin GONE  ":          "GONE",
		"@fin DENIED\n":          "DENIED",
		"@fin STUCK\n":           "STUCK",
	} {
		if got, err := resultadoCierre(in); err != nil || got != want {
			t.Errorf("%q → %q/%v", in, got, err)
		}
	}
	for _, in := range []string{"", "@fin OTRA", "INT"} {
		if _, err := resultadoCierre(in); err == nil {
			t.Errorf("%q debió fallar", in)
		}
	}
}

// Qué reenvía el relay de localhost: todas las interfaces o loopback, en v4,
// v6 y v4 mapeada; no lo atado a la IP de la VM ni a una interfaz.
func TestLinuxForwarded(t *testing.T) {
	mk := func(addr string, iface string) ports.LinuxSocket {
		ss, err := ports.ParseSS("tcp LISTEN 0 1 " + addr + " 0.0.0.0:*")
		if err != nil || len(ss) != 1 {
			t.Fatalf("%s: %v", addr, err)
		}
		ss[0].Iface = iface
		return ss[0]
	}
	for addr, want := range map[string]bool{
		"0.0.0.0:3000": true, "127.0.0.1:3000": true, "[::]:3000": true, "[::1]:3000": true, "*:3000": true,
		"[::ffff:127.0.0.1]:3000": true, "172.25.160.10:3000": false, "[fe80::1%eth0]:3000": false,
	} {
		if got := linuxForwarded(mk(addr, "")); got != want {
			t.Errorf("%s → %v, esperaba %v", addr, got, want)
		}
	}
	if linuxForwarded(mk("127.0.0.53:53", "lo")) {
		t.Error("atado a una interfaz (%lo) no se reenvía")
	}
}
