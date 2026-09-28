//go:build windows

package killport

import (
	"context"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

// X-02: a 80 y a 100 columnas nada de lo que killport escribe se pasa del
// ancho ni parte un comando: las salidas de un puerto de WinNAT (el efecto se
// parte debajo de sí mismo), el plan de --reserve (el comando va entero en su
// renglón) y la línea de comandos del bloque (X-03: el script se ve).
func TestNadaSePasaDelAncho(t *testing.T) {
	sys := sistemaDeRed(redConBloque(), false)
	res := reaper.NewDiagnoser(sys, ports.Filter{TCP: true}, true).Port(3000)
	e, err := leerEstadoRed(context.Background(), sys)
	if err != nil {
		t.Fatal(err)
	}
	ped := pedidoConfig{Reservar: ports.Of(55200), TCP: true}
	plan := planificarConfig(e, ped)
	cmd := `"C:\Program Files\nodejs\node.exe" C:\Users\mauro\dev\_pruebas\killport\revision\x\srv.js normal 0.0.0.0 0 A C:\Users\mauro\dev\_pruebas\killport\revision\x\m-A.txt`
	legible, _ := comandoLegible(cmd, rutasDe{home: `C:\Users\mauro`})
	for _, ancho := range []int{80, 100} {
		term := tui.Open()
		term.SetWidth(ancho)
		var todo []string
		todo = append(todo, lineasPuertoDe(term, res, "puerto 3000")...)
		todo = append(todo, bloqueConfig(term, e, ped, plan)...)
		bloque := lineasComando(term, legible, cmd)
		todo = append(todo, bloque...)
		texto := strings.Join(plano(todo), "\n")
		for _, l := range todo {
			if w := tui.Width(l); w > ancho && !strings.Contains(l, "netsh") {
				t.Errorf("a %d, renglón de %d:\n%s", ancho, w, l)
			}
		}
		for _, entero := range []string{"killport 3000 --reserve", "net stop winnat && net start winnat", "store=persistent"} {
			if !strings.Contains(texto, entero) {
				t.Errorf("a %d se partió %q:\n%s", ancho, entero, texto)
			}
		}
		if !strings.Contains(strings.Join(plano(bloque), " "), "srv.js") {
			t.Errorf("a %d la línea de comandos del bloque perdió el script:\n%s", ancho, strings.Join(plano(bloque), "\n"))
		}
		for _, l := range plano(todo) {
			if strings.Contains(l, "netsh") && !strings.Contains(l, "store=persistent") && strings.Contains(l, "excludedportrange") {
				t.Errorf("a %d el comando de netsh se partió: %q", ancho, l)
			}
		}
	}
}

// El pie del listado se parte como las demás pistas. Con lo que se ocultó en
// la captura del README (19 procesos de Windows, 75 sockets UDP), a 100
// columnas la consola lo cortaba en "--" / "all-states"; ahora el renglón
// termina antes del separador y el que sigue cuelga debajo del texto. En un
// pipe va entero, en una línea.
func TestPieDelListadoSeParte(t *testing.T) {
	oc := sinMostrar{filas: 94, windows: 19, efimeros: 75}
	term := tui.Open()
	term.SetWidth(100)
	want := []string{
		"    › 19 procesos de Windows ocultos · --system los muestra; 75 sockets UDP de clientes ocultos",
		"      · --all-states los muestra",
		"    › killport <puerto> cierra lo que ocupa ese puerto",
	}
	if got := pieListado(term, oc); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("a 100 columnas:\n%s\nesperaba:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, ancho := range []int{60, 80, 120} {
		term.SetWidth(ancho)
		got := pieListado(term, sinMostrar{filas: 3, windows: 1, efimeros: 1, linux: 1})
		for _, l := range got {
			if tui.Width(l) > ancho || strings.HasSuffix(l, "--") || strings.HasSuffix(l, "·") {
				t.Errorf("a %d: %q", ancho, got)
			}
		}
	}
	if got := pieListado(term, sinMostrar{}); len(got) != 1 {
		t.Errorf("sin nada oculto, solo la pista de cerrar: %q", got)
	}
	if pipe := tui.Open(); !pipe.Bounded() {
		if got := pieListado(pipe, oc); len(got) != 2 || !strings.HasSuffix(got[0], "--all-states los muestra") {
			t.Errorf("a un pipe, cada pista en una línea: %q", got)
		}
	}
}
