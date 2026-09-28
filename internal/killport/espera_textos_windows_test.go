//go:build windows

package killport

import (
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

// X-37: se espera [::1]:5173 y el servidor escucha en 127.0.0.1:5173. Al
// vencer, "nadie escucha el 5173" es falso (es la trampa de localhost): se
// dice quién escucha y dónde.
func TestEsperaVenceDiceQuienEscuchaEnOtraDireccion(t *testing.T) {
	sys := sistemaQueEscucha(t, "127.0.0.1:5173", 0, 0)
	out, code := capturar(t, func(term *tui.Term) int {
		return correrEspera(term, sys, []string{"[::1]:5173"}, opciones{wait: 300 * time.Millisecond, waitDado: true})
	})
	yo := u32(win.CurrentProcessID())
	if code != cli.ExitFailure || strings.Contains(out, "nadie escucha el 5173") ||
		!strings.Contains(out, "nadie contesta [::1]:5173; ") || !strings.Contains(out, "(PID "+yo) ||
		!strings.Contains(out, "escucha el 5173 en 127.0.0.1") {
		t.Fatalf("código %d\n%s", code, out)
	}
}

// X-17: la primera línea dice qué se espera y el plazo, sin repetir cómo se
// mira (eso lo dice la ayuda); --wait-free no repite "sin cerrar".
func TestEsperaLineasSinRepetir(t *testing.T) {
	sys := sistemaQueEscucha(t, "[::1]:5173", 0, 0)
	out, _ := capturar(t, func(term *tui.Term) int {
		return correrEspera(term, sys, []string{"5173"}, opciones{waitFree: 200 * time.Millisecond, waitFreeDado: true})
	})
	if strings.Contains(out, "no me conecto") || strings.Count(out, "sin cerrar") != 1 || strings.Contains(out, "espero a que se libere, sin cerrarlo") {
		t.Fatalf("salida:\n%s", out)
	}
}
