package tui

import (
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Q-03: la presentación no depende de la capa de sistema. tui llega a la
// consola por win/desk (modo y tamaño) y no carga con lo de win: las tablas
// de red (net/netip), el ejecutor acotado (os/exec), el cliente de named
// pipes. Así tui se prueba sin Windows de por medio y el dibujo no puede
// terminar llamando a la API de procesos por accidente.
func TestSinLaCapaDeSistema(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: sin correr go list")
	}
	out, err := exec.Command("go", "list", "-deps", "github.com/agustinyarrus/killport/internal/tui").Output()
	if err != nil {
		t.Skipf("go list no anduvo: %v", err)
	}
	deps := strings.Fields(string(out))
	if runtime.GOOS == "windows" && !slices.Contains(deps, "github.com/agustinyarrus/killport/internal/win/desk") {
		t.Errorf("go list no devolvió lo esperado (falta win/desk):\n%s", out)
	}
	for _, no := range []string{"net/netip", "os/exec", "github.com/agustinyarrus/killport/internal/win", "github.com/agustinyarrus/killport/internal/win/pipeio"} {
		if slices.Contains(deps, no) {
			t.Errorf("tui depende de %s", no)
		}
	}
}
