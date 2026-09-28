//go:build windows

package killport

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

// filasDeMuestra son filas como las de una máquina de desarrollo real (las
// de esta), con un puerto compartido.
func filasDeMuestra() []fila {
	now := time.Now()
	s := func(pid uint32, dir string, port uint16) ports.Socket {
		return ports.Socket{Proto: ports.TCP, Local: netip.AddrPortFrom(netip.MustParseAddr(dir), port), State: ports.Listen, PID: pid, Since: now.Add(-3 * time.Hour)}
	}
	shop := &proyecto{Nombre: "fixture-kp", Rama: "prueba"}
	return []fila{
		{Port: 4173, Proto: ports.TCP, PID: 4088, Owner: reaper.ID{PID: 4088}, Name: "node.exe", Since: now.Add(-28 * time.Hour),
			Comando: "node server.js", keep: 5, Proyecto: &proyecto{Nombre: "landing"}, socks: []ports.Socket{s(4088, "127.0.0.1", 4173)}},
		{Port: 5173, Proto: ports.TCP, PID: 812, Owner: reaper.ID{PID: 812}, Name: "node.exe", Since: now.Add(-2 * time.Hour), Compartido: true,
			Comando: `node node_modules\vite\bin\vite.js --port 5173 --strictPort`, keep: 5, Proyecto: shop, socks: []ports.Socket{s(812, "::1", 5173)}},
		{Port: 5173, Proto: ports.TCP, PID: 990, Owner: reaper.ID{PID: 990}, Name: "python.exe", Since: now.Add(-5 * time.Minute), Compartido: true,
			Comando: "python -m http.server 5173", keep: len("python -m http.server "), socks: []ports.Socket{s(990, "0.0.0.0", 5173)}},
		{Port: 31100, Proto: ports.TCP, PID: 5528, Owner: reaper.ID{PID: 5528}, Name: "CoordinatorService.exe", Since: now.Add(-35 * time.Hour),
			Comando: "Contoso_CoordinatorService · servicio", keep: len("Contoso_CoordinatorService"), socks: []ports.Socket{s(5528, "0.0.0.0", 31100), s(5528, "::", 31100)}},
		{Port: 8099, Proto: ports.TCP, PID: 37880, Owner: reaper.ID{PID: 37880}, Name: "llama-server.exe", Since: now.Add(-16 * time.Hour),
			Comando: `llama-server -m ~\models\modelo-local-30b.gguf --host 127.0.0.1 --port 8099 --ctx-size 65536 --jinja`, keep: 13,
			Proyecto: &proyecto{Nombre: "chat-local"}, socks: []ports.Socket{s(37880, "127.0.0.1", 8099)}},
	}
}

// Cómo se lee el listado: en 100 columnas (el ancho de un pipe) entra todo
// lo esencial sin pasarse; en 180 aparecen DESDE y la DIRECCIÓN exacta y el
// comando se ve entero; en todos, el programa del COMANDO se ve entero y la
// marca del puerto compartido está.
func TestListadoSeLeeEn100Y180(t *testing.T) {
	filas := filasDeMuestra()
	for _, ancho := range []int{80, 100, 120, 140, 180} {
		for _, numerar := range []bool{false, true} {
			term := tui.Open()
			term.SetWidth(ancho)
			lineas := tablaListado(term, nil, filas, numerar, false)
			texto := strings.Join(lineas, "\n")
			for _, l := range lineas {
				if w := tui.Width(l); w > ancho {
					t.Errorf("a %d columnas (numerar=%v) la línea se pasa (%d):\n%s", ancho, numerar, w, l)
				}
			}
			cols := []string{"PUERTO", "PROTO", "ALCANCE", "PID", "PROCESO", "PROYECTO", "COMANDO"}
			if ancho < 100 {
				cols = cols[:5] // PROYECTO se va antes que apretar ALCANCE o COMANDO (X-12)
			}
			for _, col := range cols {
				if !strings.Contains(lineas[0], col) {
					t.Errorf("a %d columnas falta %s:\n%s", ancho, col, texto)
				}
			}
			if ancho < 100 {
				// A 80: el "(v6)" de ALCANCE entero (es lo que la columna vino a
				// decir), el programa y el script del COMANDO a la vista.
				for _, want := range []string{"esta PC (v6)", "node server.js", "python -m http.", "llama-server -m"} {
					if !strings.Contains(texto, want) {
						t.Errorf("a %d columnas (numerar=%v) falta %q:\n%s", ancho, numerar, want, texto)
					}
				}
				if ancho == 80 && !numerar {
					t.Logf("a 80 columnas:\n%s", texto)
				}
				continue
			}
			for _, want := range []string{"! 5173", "esta PC (v6)", "toda la red", "node ", "python ", "llama-server ", "node.exe", "python.exe"} {
				if !strings.Contains(texto, want) {
					t.Errorf("a %d columnas falta %q:\n%s", ancho, want, texto)
				}
			}
			if ancho == 180 && !numerar {
				for _, want := range []string{"DESDE", "DIRECCIÓN", "[::1]", "∗ v4+v6", "hace 28 h", "--ctx-size 65536 --jinja", "CoordinatorService.exe", "Contoso_CoordinatorService"} {
					if !strings.Contains(texto, want) {
						t.Errorf("a 180 columnas se ve %q:\n%s", want, texto)
					}
				}
			}
			if ancho == 100 && !numerar {
				t.Logf("a 100 columnas:\n%s", texto)
			}
			if ancho == 180 && !numerar {
				t.Logf("a 180 columnas:\n%s", texto)
			}
		}
	}
}
