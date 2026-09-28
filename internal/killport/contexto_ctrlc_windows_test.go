//go:build windows

package killport

// El aviso del Ctrl+C ANTES de preguntar dice lo que el motor va a hacer.
//
// Lo vio la regresión final (t3\real, caso "familia"): un padre en 0.0.0.0 y
// su hijo en [::1], en la misma consola, y `killport localhost:P`. El bloque
// avisaba "el Ctrl+C le llega también a node.exe (PID padre), su padre:
// comparte la consola" —el motor no lo manda: el padre quedó al margen— y
// callaba que el hijo iba a cerrarse en seco. Ahora el contexto sale de
// reaper.Plan.CtrlCOutlook, la misma regla que usa el motor.

import (
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

func TestContextoCtrlCComoElMotor(t *testing.T) {
	yo := reaper.Proc{ID: reaper.ID{PID: 9000, Created: t0}, Name: "killport.exe", Parent: 1, SessionID: 1}
	node := func(pid, padre uint32, antes time.Duration) reaper.Proc {
		return reaper.Proc{ID: reaper.ID{PID: pid, Created: t0.Add(-antes)}, Name: "node.exe", Parent: padre, SessionID: 1,
			Path: `C:\Program Files\nodejs\node.exe`}
	}
	padre, hijo, ajeno := node(100, 1, 2*time.Minute), node(101, 100, time.Minute), node(300, 1, 3*time.Minute)
	casos := []struct {
		nombre   string
		pedido   string
		socks    []ports.Socket
		consola  []uint32
		quiere   string // lo que tiene que decir el bloque
		noQuiere string // y lo que no
		pregunta []string
	}{
		{nombre: "padre al margen: no hay Ctrl+C, el hijo va en seco", pedido: "localhost:5173",
			socks:    []ports.Socket{sock(ports.TCP, "0.0.0.0", 5173, ports.Listen, 100, ""), sock(ports.TCP, "::1", 5173, ports.Listen, 101, "")},
			consola:  []uint32{100, 101},
			quiere:   "sin Ctrl+C: la consola la comparte también node.exe (PID 100), que queda al margen; se cierra en seco",
			noQuiere: "le llega también"},
		{nombre: "padre que no escucha (npm): le llega también", pedido: "5173",
			socks:   []ports.Socket{sock(ports.TCP, "::1", 5173, ports.Listen, 101, "")},
			consola: []uint32{100, 101},
			quiere:  "el Ctrl+C le llega también a node.exe (PID 100), su padre: comparte la consola", noQuiere: "sin Ctrl+C",
			pregunta: []string{"node.exe (PID 100)"}},
		{nombre: "hijo sin --tree: le llega también", pedido: "5173",
			socks:   []ports.Socket{sock(ports.TCP, "0.0.0.0", 5173, ports.Listen, 100, "")},
			consola: []uint32{100, 101},
			quiere:  "el Ctrl+C le llega también a node.exe (PID 101), su hijo: comparte la consola", noQuiere: "sin Ctrl+C",
			pregunta: []string{"node.exe (PID 101)"}},
		{nombre: "un ajeno en la consola: no hay Ctrl+C", pedido: "5173",
			socks:    []ports.Socket{sock(ports.TCP, "::1", 5173, ports.Listen, 101, "")},
			consola:  []uint32{101, 300},
			quiere:   "sin Ctrl+C: la consola la comparte también node.exe (PID 300), que no se toca; se cierra en seco",
			noQuiere: "le llega también"},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			f := reaper.NewForest(conWindows(yo, padre, hijo, ajeno))
			cfg := reaper.Config{Self: yo.ID, SystemRoot: raizWindows, OwnConsole: map[uint32]bool{9000: true}}
			q, err := ports.ParseRequest([]string{c.pedido})
			if err != nil {
				t.Fatal(err)
			}
			plan := reaper.Focus(reaper.PlanKill(q.Ports, c.socks, f, nil, cfg), q, nil)
			if len(plan.Targets) != 1 {
				t.Fatalf("plan = %+v", plan.Targets)
			}
			sys := &sistema{consolaDe: func(uint32) ([]uint32, bool, error) { return c.consola, true, nil }}
			var det detalle
			det.conContexto(sys, f, plan, nil, cfg)
			got := strings.Join(plano(bloqueObjetivo(tui.Open(), sys, f, plan.Targets[0], cfg, det, t0, 0)), "\n")
			if !strings.Contains(got, c.quiere) || strings.Contains(got, c.noQuiere) {
				t.Fatalf("bloque (quería %q, sin %q):\n%s", c.quiere, c.noQuiere, got)
			}
			if a := det.alcanzados(); strings.Join(a, ",") != strings.Join(c.pregunta, ",") {
				t.Fatalf("la pregunta avisa que le llega también a %v, quería %v", a, c.pregunta)
			}
			// Con --force no hay Ctrl+C: nada que avisar.
			cfg.Force = true
			var seco detalle
			seco.conContexto(sys, f, plan, nil, cfg)
			got = strings.Join(plano(bloqueObjetivo(tui.Open(), sys, f, plan.Targets[0], cfg, seco, t0, 0)), "\n")
			if strings.Contains(got, "Ctrl+C") {
				t.Fatalf("con --force el bloque no habla del Ctrl+C:\n%s", got)
			}
		})
	}
}
