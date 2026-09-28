//go:build windows

package killport

// Cómo se cuenta la escalada (X-07, X-08, X-09, X-16).

import (
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

// arbolDeTres: node (100) con dos hijos node (101, 102), --tree.
func arbolDeTres() (*reaper.Forest, reaper.Plan) {
	mk := func(pid, padre uint32) reaper.Proc {
		return reaper.Proc{ID: reaper.ID{PID: pid, Created: t0.Add(-time.Minute)}, Name: "node.exe", Parent: padre, Path: `C:\Program Files\nodejs\node.exe`}
	}
	f := reaper.NewForest([]reaper.Proc{mk(100, 1), mk(101, 100), mk(102, 100)})
	s := sock(ports.TCP, "127.0.0.1", 3000, ports.Listen, 100, "")
	plan := reaper.PlanKill(ports.Of(3000), []ports.Socket{s}, f, nil, reaper.Config{Tree: true, SystemRoot: `C:\Windows`})
	return f, plan
}

func TestNarracionEnSecoUnaLineaConPorque(t *testing.T) {
	term := tui.Open()
	f, plan := arbolDeTres()
	como := nuevaNarracion(plan, f, false, false, time.Second)
	id := func(pid uint32) reaper.ID { return reaper.ID{PID: pid, Created: t0.Add(-time.Minute)} }

	// X-09: el Ctrl+C dice a quién más le llega.
	l := plano([]string{lineaEvento(term, reaper.Event{Kind: reaper.EvCtrlC, Victim: id(100), Name: "node.exe",
		Members: []reaper.ID{id(100), id(101)}, Also: []reaper.Proc{{ID: id(50), Name: "npm.exe"}}}, como)})[0]
	if !strings.Contains(l, "Ctrl+C a node.exe (PID 100)") || !strings.Contains(l, "le llega también a npm.exe (PID 50), que comparte la consola") {
		t.Fatalf("Ctrl+C = %q", l)
	}
	// X-08: terminado en seco es UNA línea, con el porqué; la confirmación
	// del kernel no suma un "✓ cerró".
	l = plano([]string{lineaEvento(term, reaper.Event{Kind: reaper.EvTerminated, Victim: id(101), Name: "node.exe"}, como)})[0]
	if !strings.Contains(l, "! node.exe (PID 101) terminado en seco") || !strings.Contains(l, "no cedió al Ctrl+C en 1 s") {
		t.Fatalf("terminado (con Ctrl+C) = %q", l)
	}
	if l := lineaEvento(term, reaper.Event{Kind: reaper.EvExited, Victim: id(101), Name: "node.exe", Method: reaper.MethodTerminate, Elapsed: 5 * time.Millisecond}, como); l != "" {
		t.Fatalf("la salida de uno terminado no suma línea: %q", l)
	}
	lineaEvento(term, reaper.Event{Kind: reaper.EvCtrlCSkipped, Victim: id(102), Name: "node.exe", Detail: "la consola la comparten otros procesos"}, como)
	if l := lineaEvento(term, reaper.Event{Kind: reaper.EvTerminated, Victim: id(102), Name: "node.exe"}, como); !strings.Contains(l, "sin Ctrl+C: la consola la comparten otros procesos") {
		t.Fatalf("terminado (Ctrl+C omitido) = %q", l)
	}
	if l := lineaEvento(term, reaper.Event{Kind: reaper.EvTerminated, Victim: id(999), Name: "app.exe"}, como); !strings.Contains(l, "no tiene consola ni ventana") {
		t.Fatalf("terminado (nada que pedirle) = %q", l)
	}
	como.enSeco = true
	if l := lineaEvento(term, reaper.Event{Kind: reaper.EvTerminated, Victim: id(100), Name: "node.exe"}, como); !strings.Contains(l, "--force") {
		t.Fatalf("terminado con --force = %q", l)
	}
	// X-16: debajo del milisegundo, "<1 ms" y no "0,00 ms".
	como.enSeco = false
	if l := lineaEvento(term, reaper.Event{Kind: reaper.EvExited, Victim: id(100), Name: "node.exe", Method: reaper.MethodCtrlC}, como); !strings.Contains(l, "<1 ms · Ctrl+C") || strings.Contains(l, "0,00") {
		t.Fatalf("cerró = %q", l)
	}
	// Un nombre que no se repite no lleva PID.
	if l := lineaEvento(term, reaper.Event{Kind: reaper.EvExited, Victim: reaper.ID{PID: 7}, Name: "python.exe", Method: reaper.MethodCtrlC, Elapsed: 20 * time.Millisecond}, como); strings.Contains(l, "PID") {
		t.Fatalf("sin repetidos = %q", l)
	}
}

// X-07: el bloque de --tree dice QUÉ cierra, con nombre y PID.
func TestBloqueTreeNombraLosHijos(t *testing.T) {
	f, plan := arbolDeTres()
	got := strings.Join(plano(bloqueObjetivo(tui.Open(), &sistema{}, f, plan.Targets[0], reaper.Config{Tree: true}, detalle{}, t0, 0)), "\n")
	if !strings.Contains(got, "--tree: cierra también node.exe (PID 101) y node.exe (PID 102)") {
		t.Fatalf("bloque:\n%s", got)
	}
}

// X-36: un proceso de otra sesión (SYSTEM, otro usuario), hijo de un servicio
// de terceros: antes de confirmar se dice que pide administrador y que el
// servicio que lo lanzó puede volver a lanzarlo.
func TestContextoOtraSesionYServicioPadre(t *testing.T) {
	yo := reaper.Proc{ID: reaper.ID{PID: 9000, Created: t0}, Name: "killport.exe", Parent: 1, SessionID: 1}
	manager := reaper.Proc{ID: reaper.ID{PID: 6000, Created: t0.Add(-time.Hour)}, Name: "ManagerService.exe", Parent: pidServices,
		Path: `C:\Program Files (x86)\Contoso\ManagerService.exe`}
	hijo := reaper.Proc{ID: reaper.ID{PID: 6284, Created: t0.Add(-time.Minute)}, Name: "Manager.exe", Parent: 6000,
		Path: `C:\Program Files (x86)\Contoso\Manager.exe`}
	f := reaper.NewForest(conWindows(yo, manager, hijo))
	svc := map[uint32][]reaper.Service{6000: {{Name: "ContosoManager"}}}
	cfg := reaper.Config{Self: yo.ID, SystemRoot: raizWindows}
	plan := reaper.PlanKill(ports.Of(8000), []ports.Socket{sock(ports.TCP, "0.0.0.0", 8000, ports.Listen, 6284, "")}, f, svc, cfg)
	if len(plan.Targets) != 1 || plan.Targets[0].Kind != reaper.KillProcess {
		t.Fatalf("plan = %+v", plan)
	}
	var det detalle
	det.conContexto(&sistema{}, f, plan, svc, cfg)
	if !det.otraSesion() {
		t.Fatal("es de la sesión 0 y killport de la 1: pide administrador")
	}
	got := strings.Join(plano(bloqueObjetivo(tui.Open(), &sistema{}, f, plan.Targets[0], cfg, det, t0, 0)), "\n")
	if !strings.Contains(got, "es de otra sesión") || !strings.Contains(got, "pide administrador") ||
		!strings.Contains(got, "lo lanzó el servicio ContosoManager (ManagerService.exe (PID 6000))") {
		t.Fatalf("bloque:\n%s", got)
	}
}

// X-10: el supervisor que relanza no escucha nada; sin consola (o con --yes,
// que no autoriza a cerrar a quien no se nombró) se da el comando exacto.
func TestSupervisorSinConsolaDaElComando(t *testing.T) {
	sup := reaper.ID{PID: 8728, Created: t0}
	rep := reaper.Report{Ports: []reaper.PortResult{{Port: 47812, Status: reaper.PortRespawned, Supervisor: sup, SupervisorName: "node.exe"}}}
	out, _ := capturar(t, func(term *tui.Term) int {
		got := ofrecerSupervisor(term, nil, &sistema{}, ports.Of(47812), nil, reaper.Config{}, reaper.Options{}, opciones{yes: true}, rep)
		if len(got.Victims) != 0 {
			t.Errorf("con --yes no se cierra al supervisor: %+v", got.Victims)
		}
		return 0
	})
	if !strings.Contains(out, "taskkill /PID 8728 /T /F") || !strings.Contains(out, "node.exe (PID 8728)") {
		t.Fatalf("salida:\n%s", out)
	}
	// El de services.exe no: ahí la salida es detener el servicio.
	rep.Ports[0].SupervisorName = "services.exe"
	if s, _, _ := supervisorDe(rep); !s.Zero() {
		t.Fatal("services.exe no es un supervisor para cerrar")
	}
}
