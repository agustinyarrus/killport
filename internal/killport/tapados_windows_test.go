//go:build windows

package killport

import (
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

// X-28: en el 5355 UDP escuchan un node del usuario (con SO_REUSEADDR) y
// Dnscache, que no se toca. Cerrar el node no libera el puerto: la pregunta no
// puede prometerlo, el bloque lo dice antes, y con --yes no se cierra a nadie
// por nada. Visto en la máquina con el 5353 (chrome, el overlay de NVIDIA,
// adb… y Dnscache): "¿Cerrar 4 procesos y liberar 5353?".
func planTapado(t *testing.T, extra ...ports.Socket) reaper.Plan {
	t.Helper()
	dns := svchostGenuino(3224)
	node := reaper.Proc{ID: reaper.ID{PID: 34676, Created: t0.Add(-time.Minute)}, Name: "node.exe", Parent: 1, Path: `C:\Program Files\nodejs\node.exe`}
	f := reaper.NewForest(conWindows(dns, node))
	svc := map[uint32][]reaper.Service{3224: {{Name: "Dnscache", Display: "Cliente DNS", Shared: true}}}
	socks := append([]ports.Socket{
		sock(ports.UDP, "0.0.0.0", 5355, ports.StateNone, 3224, "Dnscache"),
		sock(ports.UDP, "0.0.0.0", 5355, ports.StateNone, 34676, ""),
	}, extra...)
	var ps []uint16
	for _, s := range socks {
		ps = append(ps, s.Port())
	}
	req := ports.Of(ps...)
	plan := reaper.PlanKill(req, holding(socks, ports.Filter{}, req), f, svc, cfgC2)
	return reaper.Focus(plan, ports.Whole(req), nil)
}

func TestTapadosNoPrometeLiberar(t *testing.T) {
	term := tui.Open()
	plan := planTapado(t)
	if len(plan.Targets) != 1 || len(plan.Refusals) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	q := plano([]string{preguntaCierre(term, plan, false, false)})[0]
	if strings.Contains(q, "liberar") || !strings.Contains(q, "¿Cerrar node.exe igual?") || !strings.Contains(q, "no libera nada: el 5355 sigue ocupado por Dnscache") {
		t.Fatalf("pregunta = %q", q)
	}
	// Si además el node tiene un puerto que SÍ se libera, la pregunta promete
	// solo ese.
	plan2 := planTapado(t, sock(ports.TCP, "127.0.0.1", 3000, ports.Listen, 34676, ""))
	q = plano([]string{preguntaCierre(term, plan2, false, false)})[0]
	if !strings.Contains(q, "¿Cerrar node.exe y liberar el 3000?") || !strings.Contains(q, "(el 5355 sigue ocupado por Dnscache)") {
		t.Fatalf("pregunta con un puerto que sí se libera = %q", q)
	}
}

// El bloque lo avisa antes de confirmar, y con --yes no se cierra a nadie si
// no se libera nada.
func TestTapadosBloqueYYes(t *testing.T) {
	term := tui.Open()
	plan := planTapado(t)
	bl := strings.Join(plano(lineasTapados(term, plan)), "\n")
	if !strings.Contains(bl, "Aunque lo cierre, el 5355 sigue ocupado por Dnscache") || !strings.Contains(bl, "cerrarlo no libera nada") ||
		!strings.Contains(bl, "multidifusión (LLMNR)") {
		t.Fatalf("bloques =\n%s", bl)
	}
	if que, _ := decidir(term, nil, plan, opciones{yes: true}, detalle{}); que != decisionInutil {
		t.Fatalf("con --yes: decisión %v, esperaba no cerrar a nadie", que)
	}

	plan = planTapado(t, sock(ports.TCP, "127.0.0.1", 3000, ports.Listen, 34676, ""))
	if que, _ := decidir(term, nil, plan, opciones{yes: true}, detalle{}); que != decisionActuar {
		t.Fatalf("con --yes y algo que liberar: decisión %v", que)
	}
}

func TestTextoTapados(t *testing.T) {
	ts := []tapado{{5353, ports.UDP, "Dnscache"}, {5355, ports.UDP, "Dnscache"}, {80, ports.TCP, "W3SVC"}}
	if got := textoTapados(ts); got != "el 5353 y el 5355 siguen ocupados por Dnscache; el 80 sigue ocupado por W3SVC" {
		t.Fatalf("texto = %q", got)
	}
	if multidifusion(5353, ports.TCP) != "" || multidifusion(5353, ports.UDP) != "mDNS" || multidifusion(3000, ports.UDP) != "" {
		t.Fatal("multidifusión: solo los puertos UDP conocidos")
	}
}
