//go:build windows

package killport

// El JSON del listado lleva DATOS; la tabla, lo armado para leer (X-06, X-20,
// X-21, X-32). Antes el JSON tenía "direccion": "∗ v4+v6", "comando":
// "servicio: X" y la línea de comandos acortada con ~, y la fila de un
// contenedor decía "proceso": "docker · nombre" con el PID del motor.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/win"
	"github.com/agustinyarrus/killport/internal/win/pipetest"
)

// jsonDe es una fila del JSON del listado como mapa (para ver qué claves hay).
func jsonDe(t *testing.T, filas []fila) []map[string]any {
	t.Helper()
	b, err := json.Marshal(construirJSONListado(filas, sinMostrar{}))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Escuchando []map[string]any `json:"escuchando"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Escuchando
}

// Un proceso de verdad (este mismo) y un servicio: el JSON trae la línea de
// comandos y la ruta tal cual, y del servicio solo el servicio.
func TestListadoJSONSoloDatos(t *testing.T) {
	yo := win.CurrentProcessID()
	procs, err := win.AllProcesses()
	if err != nil {
		t.Fatal(err)
	}
	var mio reaper.Proc
	for _, p := range procs {
		if p.PID == yo {
			exe, _ := os.Executable()
			mio = reaper.Proc{ID: reaper.ID{PID: p.PID, Created: p.Created}, Name: p.Image, Path: exe, Parent: p.PPID}
		}
	}
	dns := svchostGenuino(3224)
	forest := reaper.NewForest(conWindows(mio, dns))
	svc := map[uint32][]reaper.Service{3224: {{Name: "Dnscache", Shared: true}}}
	s := sock(ports.TCP, "0.0.0.0", 47000, ports.Listen, yo, "")
	s.Since = mio.Created.Add(1)
	socks := []ports.Socket{s, sock(ports.TCP, "::", 47000, ports.Listen, yo, ""), sock(ports.UDP, "0.0.0.0", 5353, ports.StateNone, 3224, "Dnscache")}
	socks[1].Since = s.Since
	filas := filasDeListado(socks, forest, svc, reaper.Config{SystemRoot: raizWindows})
	anotarProcesos(&sistema{}, forest, filas)

	d, err := win.QueryDetail(yo, mio.Created)
	if err != nil {
		t.Fatal(err)
	}
	var mia, dnsc fila
	for _, f := range filas {
		switch f.Port {
		case 47000:
			mia = f
		case 5353:
			dnsc = f
		}
	}
	if mia.Linea != unaLinea(d.CommandLine) || mia.Ruta != mio.Path || mia.Comando == "" {
		t.Fatalf("fila propia: línea %q (esperaba %q), ruta %q", mia.Linea, d.CommandLine, mia.Ruta)
	}
	// X-32: el nombre del servicio es lo que se ve (el recorte se come el
	// rótulo, no el nombre).
	if dnsc.Comando != "Dnscache · servicio" || dnsc.keep != len("Dnscache") {
		t.Fatalf("fila del servicio: %q (keep %d)", dnsc.Comando, dnsc.keep)
	}
	for _, e := range jsonDe(t, []fila{mia, dnsc}) {
		if _, hay := e["direccion"]; hay {
			t.Fatalf("el JSON trae 'direccion' (texto de pantalla): %v", e)
		}
		switch e["puerto"].(float64) {
		case 47000:
			if e["comando"] != unaLinea(d.CommandLine) || e["ruta"] != mio.Path || strings.Join(aStrings(e["direcciones"]), ",") != "0.0.0.0,::" {
				t.Fatalf("fila propia en el JSON: %v", e)
			}
		case 5353:
			if _, hay := e["comando"]; hay || e["servicio"] != "Dnscache" {
				t.Fatalf("fila del servicio en el JSON: %v", e)
			}
		}
	}
}

// Un contenedor y un proceso Linux: la tabla muestra lo de atrás; el JSON,
// el proceso de Windows que sostiene el socket, y lo de atrás en su objeto
// (con la política de reinicio del contenedor, que antes solo tenía el
// bloque de `killport P`).
func TestListadoJSONIntermediarios(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{})
	f := &wslFalso{running: []string{"Ubuntu"}, sondeo: map[string]string{"Ubuntu": string(grabado(t, "wsl_sondeo_ss.txt"))}}
	forest := reaper.NewForest([]reaper.Proc{
		{ID: reaper.ID{PID: 8124, Created: t0}, Name: "com.docker.backend.exe", Path: `C:\Program Files\Docker\Docker\resources\com.docker.backend.exe`},
		{ID: reaper.ID{PID: 9120, Created: t0}, Name: "wslrelay.exe", Path: `C:\Program Files\WSL\wslrelay.exe`},
	})
	socks := []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 5432, ports.Listen, 8124, "com.docker.backend.exe"),
		sock(ports.TCP, "127.0.0.1", 3000, ports.Listen, 9120, "wslrelay.exe"),
	}
	sys := sistemaFalso(t, m, f)
	filas, _, _ := anotarListado(context.Background(), sys, filasDeListado(socks, forest, nil, reaper.Config{SystemRoot: raizWindows}), forest, opciones{wsl: true})
	for _, e := range jsonDe(t, filas) {
		switch e["puerto"].(float64) {
		case 5432:
			c, _ := e["contenedor"].(map[string]any)
			if e["proceso"] != "com.docker.backend.exe" || e["pid"].(float64) != 8124 || c == nil || c["reinicio"] != "always" {
				t.Fatalf("fila del contenedor en el JSON: %v", e)
			}
			if _, hay := e["comando"]; hay {
				t.Fatalf("la fila del contenedor trae un comando de pantalla: %v", e)
			}
		case 3000:
			w, _ := e["wsl"].(map[string]any)
			if e["proceso"] != "wslrelay.exe" || e["pid"].(float64) != 9120 || w == nil || w["pid"].(float64) != 812 {
				t.Fatalf("fila del proceso Linux en el JSON: %v", e)
			}
		}
	}
}

func aStrings(v any) []string {
	var out []string
	xs, _ := v.([]any)
	for _, x := range xs {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}
