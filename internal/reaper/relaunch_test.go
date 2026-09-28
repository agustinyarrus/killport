package reaper

import (
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// C-09: un supervisor (misma imagen que la víctima, como pm2 o el recargador
// de uvicorn) relanza a la víctima `relanza` después de que muere, y el
// relanzado vuelve a escuchar el 3000 `escucha` después de nacer. La víctima
// cede al Ctrl+C a los 200 ms.
func escenarioSupervisor(relanza, escucha time.Duration) (*simOS, Config, Options, *Forest, Plan) {
	s, cfg, opts := escenario()
	opts.Settle = 350 * time.Millisecond // la ventana de producción
	sup := ID{PID: 10, Created: at(0)}
	s.add(&simProc{id: sup, name: "node.exe", parent: 500})
	n := s.add(&simProc{id: ID{PID: 100, Created: at(50)}, name: "node.exe", parent: 10})
	s.sockets = append(s.sockets, tcpListen(3000, 100, at(60)))
	n.console = []uint32{100}
	n.ctrlCReacts, n.ctrlCAfter = true, 200*time.Millisecond
	before, pl := buildPlan(s, cfg, req, opts.Filter)

	muere := at(200)
	nace := muere.Add(relanza)
	escuchaDesde := nace.Add(escucha)
	s.afterFn = func(s *simOS) []ports.Socket {
		switch {
		case !s.now.Before(escuchaDesde):
			return []ports.Socket{tcpListen(3000, 101, escuchaDesde)}
		case s.now.Before(muere):
			return []ports.Socket{tcpListen(3000, 100, at(60))}
		}
		return nil
	}
	s.snapFn = func(s *simOS) []Proc {
		out := []Proc{
			{ID: ID{PID: 500, Created: at(0)}, Name: "pwsh.exe"},
			{ID: kpSelf(), Name: "killport.exe", Parent: 500},
			{ID: sup, Name: "node.exe", Parent: 500},
		}
		if !s.now.Before(nace) {
			out = append(out, Proc{ID: ID{PID: 101, Created: nace}, Name: "node.exe", Parent: 10})
		}
		return out
	}
	return s, cfg, opts, before, pl
}

// El caso real de la revisión: relanza a los 50 ms y el relanzado escucha
// 800 ms después. Settle (350 ms) ya pasó cuando vuelve a escuchar.
func TestRelanzadoLentoNoEsLibre(t *testing.T) {
	s, cfg, opts, before, pl := escenarioSupervisor(50*time.Millisecond, 800*time.Millisecond)
	rep, _ := exec(s, before, req, pl, cfg, opts)
	assertVictim(t, rep, OutcomeClosed, MethodCtrlC)
	pr := assertPort(t, rep, PortRespawned)
	if pr.Owner.PID != 101 || pr.SupervisorName != "node.exe" || pr.Supervisor.PID != 10 || pr.Starting {
		t.Fatalf("dictamen = %+v", pr)
	}
}

// El supervisor espera 600 ms antes de relanzar: cuando se confirma libre,
// el relanzado todavía no nació. La pista es el padre vivo con la misma
// imagen.
func TestRelanzadoTardioNoEsLibre(t *testing.T) {
	s, cfg, opts, before, pl := escenarioSupervisor(600*time.Millisecond, 800*time.Millisecond)
	rep, _ := exec(s, before, req, pl, cfg, opts)
	pr := assertPort(t, rep, PortRespawned)
	if pr.Owner.PID != 101 || pr.Supervisor.PID != 10 {
		t.Fatalf("dictamen = %+v", pr)
	}
}

// Un relanzado que no llega a escuchar antes del tope: no se da el puerto por
// libre; se nombra al supervisor.
func TestRelanzadoQueNoEscuchaATiempo(t *testing.T) {
	s, cfg, opts, before, pl := escenarioSupervisor(50*time.Millisecond, time.Hour)
	rep, _ := exec(s, before, req, pl, cfg, opts)
	pr := assertPort(t, rep, PortRespawned)
	if pr.Owner.PID != 101 || pr.Supervisor.PID != 10 || !pr.Starting || !strings.Contains(pr.Reason, "todavía no escucha") || !strings.Contains(pr.Advice, "PID 10") {
		t.Fatalf("dictamen = %+v", pr)
	}
	if el := s.now.Sub(at(200)); el > relaunchListen+time.Second {
		t.Fatalf("esperó %v: el tope es %v desde que nació el relanzado", el, relaunchListen)
	}
}

// Padre vivo con la misma imagen que NO relanza: se le da el rato para
// aparecer y el puerto queda libre.
func TestPadreQueNoRelanzaEsLibre(t *testing.T) {
	s, cfg, opts, before, pl := escenarioSupervisor(time.Hour, time.Hour)
	rep, _ := exec(s, before, req, pl, cfg, opts)
	assertPort(t, rep, PortFree)
	if el := s.now.Sub(at(200)); el < relaunchAppear || el > relaunchAppear+time.Second {
		t.Fatalf("esperó %v; esperaba ~%v", el, relaunchAppear)
	}
}

// Con un padre de otra imagen (la terminal) no hay espera extra; y un node
// nuevo de otra rama (VS Code lanzando un language server) no es un
// relanzamiento.
func TestSinSupervisorNoEspera(t *testing.T) {
	s, cfg, opts := escenario()
	opts.Settle = 350 * time.Millisecond
	n := nodeTarget(s) // hijo de pwsh
	n.console = []uint32{100}
	n.ctrlCReacts, n.ctrlCAfter = true, 200*time.Millisecond
	s.add(&simProc{id: ID{PID: 1, Created: at(0)}, name: "explorer.exe"})
	before, pl := buildPlan(s, cfg, req, opts.Filter)
	s.snapFn = func(s *simOS) []Proc {
		return append(s.liveProcs(), Proc{ID: ID{PID: 300, Created: at(250)}, Name: "node.exe", Parent: 1})
	}
	rep, _ := exec(s, before, req, pl, cfg, opts)
	assertPort(t, rep, PortFree)
	if el := s.now.Sub(at(200)); el > time.Second {
		t.Fatalf("esperó %v sin supervisor a la vista", el)
	}
}
