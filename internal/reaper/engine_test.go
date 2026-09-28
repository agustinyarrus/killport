package reaper

import (
	"context"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

var ctx = context.Background()

const kpPID = 600 // killport en los escenarios

func kpSelf() ID { return ID{PID: kpPID, Created: at(10)} }

// buildPlan toma la instantánea y la tabla del simulador y planifica, tal como
// haría el adaptador de la herramienta.
func buildPlan(s *simOS, cfg Config, requested ports.Set, filter ports.Filter) (*Forest, Plan) {
	procs, _ := s.Snapshot(ctx)
	before := NewForest(procs)
	socks, _ := s.Sockets(ctx)
	filter.Ports = requested
	var holding []ports.Socket
	for _, sk := range socks {
		if filter.Relate(sk) == ports.Holds {
			holding = append(holding, sk)
		}
	}
	svc, _ := s.Services()
	return before, PlanKill(requested, holding, before, svc, cfg)
}

// exec corre el motor recolectando los eventos.
func exec(s *simOS, before *Forest, requested ports.Set, plan Plan, cfg Config, opts Options) (Report, *recorder) {
	rec := &recorder{}
	opts.Emit = rec.emit
	c, cancel := context.WithCancel(context.Background())
	s.cancelFn = cancel
	rep := Run(c, s, before, requested, plan, cfg, opts)
	cancel()
	return rep, rec
}

// escenario arma el andamiaje común: shell + killport + un objetivo, con su
// socket, y devuelve el simulador para que cada test lo retoque.
func escenario() (*simOS, Config, Options) {
	s := newSim(epoch)
	s.add(&simProc{id: ID{PID: 500, Created: at(0)}, name: "pwsh.exe"})
	s.add(&simProc{id: kpSelf(), name: "killport.exe", parent: 500})
	cfg := Config{Self: kpSelf(), OwnConsole: map[uint32]bool{kpPID: true}, SystemRoot: `C:\Windows`}
	opts := Options{Timeout: 3 * time.Second, TerminateGrace: 500 * time.Millisecond, Filter: ports.Filter{}}
	return s, cfg, opts
}

// nodeTarget agrega un node.exe (PID 100) escuchando el 3000, hijo del shell.
func nodeTarget(s *simOS) *simProc {
	p := s.add(&simProc{id: ID{PID: 100, Created: at(50)}, name: "node.exe", parent: 500})
	s.sockets = append(s.sockets, tcpListen(3000, 100, at(60)))
	return p
}
