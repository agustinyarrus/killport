package reaper

import (
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// X-09: el Ctrl+C va a la CONSOLA. Si el servidor la comparte con quien lo
// lanzó (un supervisor, npm), a ese también le llega: es lo que haría
// apretar Ctrl+C en esa ventana, y está permitido (ancestro), pero no es una
// víctima. El evento lo nombra (Also) y el reporte lo guarda (Reached), para
// que la herramienta no informe "se cerró uno" cuando se cerraron dos.
func TestCtrlCAlcanzaAlQueLoLanzo(t *testing.T) {
	s, cfg, opts := escenario()
	sup := s.add(&simProc{id: ID{PID: 300, Created: at(40)}, name: "node.exe", parent: 500, console: []uint32{300, 301},
		ctrlCReacts: true, ctrlCAfter: 50 * time.Millisecond})
	hijo := s.add(&simProc{id: ID{PID: 301, Created: at(45)}, name: "node.exe", parent: 300, console: []uint32{300, 301},
		ctrlCReacts: true, ctrlCAfter: 50 * time.Millisecond})
	s.sockets = append(s.sockets, tcpListen(47813, 301, at(60)))
	req := ports.Of(47813)
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	if len(pl.Targets) != 1 || pl.Targets[0].Root != hijo.id {
		t.Fatalf("plan = %+v", pl.Targets)
	}
	rep, rec := exec(s, before, req, pl, cfg, opts)
	var ev *Event
	for i := range rec.evs {
		if rec.evs[i].Kind == EvCtrlC {
			ev = &rec.evs[i]
		}
	}
	if ev == nil || len(ev.Members) != 1 || ev.Members[0] != hijo.id || len(ev.Also) != 1 || ev.Also[0].ID != sup.id {
		t.Fatalf("evento del Ctrl+C = %+v", ev)
	}
	if len(rep.Reached) != 1 || rep.Reached[0].ID != sup.id || len(rep.Victims) != 1 {
		t.Fatalf("reporte: alcanzados %+v, víctimas %+v", rep.Reached, rep.Victims)
	}
}

// X-10: el supervisor que relanza a un servidor no escucha nada: ningún
// selector lo alcanza. PlanProcess lo planifica por identidad, con las mismas
// negativas que un dueño de socket, y con --tree cae con sus hijos; la
// verificación vigila los puertos que tienen que quedar libres.
func TestPlanProcessSupervisor(t *testing.T) {
	s, cfg, opts := escenario()
	cfg.Tree = true
	sup := s.add(&simProc{id: ID{PID: 300, Created: at(40)}, name: "node.exe", parent: 500, console: []uint32{300, 301},
		ctrlCReacts: true, ctrlCAfter: 50 * time.Millisecond})
	hijo := s.add(&simProc{id: ID{PID: 301, Created: at(45)}, name: "node.exe", parent: 300, console: []uint32{300, 301},
		ctrlCReacts: true, ctrlCAfter: 50 * time.Millisecond})
	s.sockets = append(s.sockets, tcpListen(47812, 301, at(60)))
	procs, _ := s.Snapshot(ctx)
	f := NewForest(procs)
	pl := PlanProcess(sup.id, ports.Of(47812), f, nil, cfg)
	if len(pl.Targets) != 1 || pl.Targets[0].Root != sup.id || len(pl.Targets[0].Victims) != 2 || pl.Targets[0].Victims[1] != hijo.id ||
		!pl.Targets[0].Ports.Equal(ports.Of(47812)) {
		t.Fatalf("plan = %+v", pl.Targets)
	}
	rep, _ := exec(s, f, ports.Of(47812), pl, cfg, opts)
	assertPort(t, rep, PortFree)
	// La terminal de killport no se planifica: queda negada, como siempre.
	pl = PlanProcess(ID{PID: 500, Created: at(0)}, ports.Of(47812), f, nil, cfg)
	if len(pl.Targets) != 0 || len(pl.Refusals) != 1 {
		t.Fatalf("la terminal = %+v", pl)
	}
	// Uno que ya no está: plan vacío.
	if pl = PlanProcess(ID{PID: 777, Created: at(1)}, ports.Of(47812), f, nil, cfg); !pl.Empty() {
		t.Fatalf("un muerto = %+v", pl)
	}
}
