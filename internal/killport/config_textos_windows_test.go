//go:build windows

package killport

import (
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/tui"
)

// X-40: sin nada que hacer, cada dato sale UNA vez: los "↷ ya es…" y el "No
// hay nada que hacer", sin el "hoy · …" que decía lo mismo justo arriba.
func TestConfigNadaQueHacerSinRepetir(t *testing.T) {
	e := estadoRed{Dyn: dinamicos(49152, 16384), WinnatExiste: true, WinnatCorriendo: true}
	ped := pedidoConfig{Dinamico: true}
	plan := planificarConfig(e, ped)
	if len(plan.Pasos) != 0 || len(plan.Hechos) == 0 {
		t.Fatalf("plan = %+v", plan)
	}
	texto := strings.Join(plano(bloqueConfig(tui.Open(), e, ped, plan)), "\n")
	if strings.Contains(texto, "hoy · ") || !strings.Contains(texto, "ya es el de fábrica") {
		t.Fatalf("bloque:\n%s", texto)
	}
	// Con algo que hacer, el "hoy" sí va (es el antes del plan).
	e.Dyn = dinamicos(1025, 3976)
	plan = planificarConfig(e, ped)
	if texto = strings.Join(plano(bloqueConfig(tui.Open(), e, ped, plan)), "\n"); !strings.Contains(texto, "hoy · ") {
		t.Fatalf("con pasos:\n%s", texto)
	}
}

// Q-05: --timeout es del cierre prolijo; con las acciones de configuración
// se ignoraba en silencio. Ahora es un error de línea de comandos.
func TestConfigRechazaTimeout(t *testing.T) {
	if err := validarConfig([]string{"3000"}, opciones{reserve: true, timeoutDado: true}); err == nil || !strings.Contains(err.Error(), "--timeout") {
		t.Fatalf("--reserve --timeout: %v", err)
	}
	if err := validarConfig([]string{"3000"}, opciones{reserve: true, list: true}); err != nil {
		t.Fatalf("--reserve --list sí va: %v", err)
	}
}
