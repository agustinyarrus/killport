//go:build windows

package killport

// Los puertos pedidos que nadie escucha: cómo se cuentan (X-34), cuándo
// (X-29: antes de preguntar, así los ve también --list) y con qué código sale
// --list (X-27).

import (
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

// Un rango libre es UNA línea, no mil iguales; lo que tiene algo que decir
// (una salvedad, un puerto reservado) va aparte, con su sugerencia.
func TestLineasDiagnosticoAgrupaLibres(t *testing.T) {
	term := tui.Open()
	var res []reaper.PortResult
	for p := uint16(20000); p <= 20999; p++ {
		res = append(res, reaper.PortResult{Port: p, Status: reaper.PortFree})
	}
	got := plano(lineasDiagnostico(term, res, ports.Request{}, nil))
	if len(got) != 1 || got[0] != "✓ 20000–20999 ya estaban libres" {
		t.Fatalf("rango libre = %q", got)
	}
	res = append(res,
		reaper.PortResult{Port: 50010, Status: reaper.PortFree, Reason: "cae en la exclusión administrada 50000–50059"},
		reaper.PortResult{Port: 3000, Status: reaper.PortReserved, Reason: "cae en el bloque 2980–3079", Advice: "no hay proceso que cerrar"})
	got = plano(lineasDiagnostico(term, res, ports.Request{}, map[uint16]uint16{3000: 3080}))
	texto := strings.Join(got, "\n")
	for _, want := range []string{"✓ 20000–20999 ya estaban libres", "✓ puerto 50010 libre (ya estaba libre)", "puerto 3000 lo reserva Windows", "el 3080 está libre"} {
		if !strings.Contains(texto, want) {
			t.Fatalf("falta %q en:\n%s", want, texto)
		}
	}
	// Uno solo: como siempre.
	got = plano(lineasDiagnostico(term, res[:1], ports.Request{}, nil))
	if len(got) != 1 || got[0] != "✓ puerto 20000 libre (ya estaba libre)" {
		t.Fatalf("uno = %q", got)
	}
	// Muchos pedazos: se cuentan.
	var salteados []reaper.PortResult
	for p := uint16(21000); p < 21040; p += 2 {
		salteados = append(salteados, reaper.PortResult{Port: p, Status: reaper.PortFree})
	}
	got = plano(lineasDiagnostico(term, salteados, ports.Request{}, nil))
	if len(got) != 1 || got[0] != "✓ 20 puertos ya estaban libres" {
		t.Fatalf("salteados = %q", got)
	}
}

// --list sale 0 si pudo mirar, tenga el puerto dueño o no: antes un puerto
// reservado por WinNAT salía 3 y el 445 (una negativa), 0.
func TestListSaleCeroSiPudoMirar(t *testing.T) {
	sys := sistemaDeRed(redConBloque(), false) // el 3000 cae en un bloque de WinNAT
	out, code := capturar(t, func(term *tui.Term) int {
		return correrPuertos(term, sys, ports.Whole(ports.Of(3000)), opciones{list: true, timeout: 3e9})
	})
	if code != cli.ExitOK || !strings.Contains(out, "lo reserva Windows") {
		t.Fatalf("killport 3000 --list: código %d\n%s", code, out)
	}
	// Sin --list (sin consola, sin --yes) el veredicto manda: no se pudo usar.
	_, code = capturar(t, func(term *tui.Term) int {
		return correrPuertos(term, sys, ports.Whole(ports.Of(3000)), opciones{timeout: 3e9})
	})
	if code != cli.ExitFailure {
		t.Fatalf("killport 3000 sin --list: código %d", code)
	}
	// Y el JSON, igual que el texto.
	reservado := reaper.Report{Ports: []reaper.PortResult{{Port: 3000, Status: reaper.PortReserved}}}
	if got := codigoJSON(reservado, reaper.Plan{}, opciones{list: true}); got != cli.ExitOK {
		t.Fatalf("--json --list reservado: %d", got)
	}
}

// X-26: reiniciar WinNAT es UNA salida de dos pasos. En el JSON, "comando"
// era "net stop winnat" y el "net start winnat" iba adentro del efecto: quien
// ejecutara la columna de comandos dejaba sin red a WSL, Docker y Hyper-V.
func TestRemedioWinNATNoSeParte(t *testing.T) {
	sys := sistemaDeRed(redConBloque(), false) // el 3000 cae en un bloque de WinNAT
	res := reaper.NewDiagnoser(sys, ports.Filter{TCP: true}, true).Port(3000)
	if res.Status != reaper.PortReserved {
		t.Fatalf("3000 = %+v", res)
	}
	doc := construirJSONReporte(reaper.Report{Ports: []reaper.PortResult{res}}, reaper.Plan{}, true)
	var cmds []string
	for _, r := range doc.Puertos[0].Remedios {
		cmds = append(cmds, r.Comando)
		if r.Comando == "net stop winnat" {
			t.Fatalf("una salida es solo detener WinNAT: %+v", doc.Puertos[0].Remedios)
		}
	}
	if strings.Join(cmds, " | ") != "killport 3000 --reserve | killport --fix-dynamic | net stop winnat && net start winnat" {
		t.Fatalf("comandos = %q", cmds)
	}
	txt := strings.Join(plano(lineasPuertoDe(tui.Open(), res, "puerto 3000")), "\n")
	if !strings.Contains(txt, "→ net stop winnat && net start winnat") {
		t.Fatalf("texto:\n%s", txt)
	}
}
