package reaper

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// pidsDe devuelve los PID de una lista de procesos, ordenados (para comparar).
func pidsDe(ps []Proc) []uint32 {
	out := make([]uint32, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.PID)
	}
	slices.Sort(out)
	return out
}

// El aviso de ANTES de preguntar (CtrlCOutlook) caso por caso. El primero es
// el de la regresión final: padre en 0.0.0.0 al margen e hijo en [::1] en la
// misma consola; `killport localhost:P` avisaba "el Ctrl+C le llega también
// a su padre" y el motor no lo mandaba (el padre está al margen): el hijo se
// cerraba en seco.
func TestCtrlCOutlook(t *testing.T) {
	type caso struct {
		nombre         string
		arma           func(s *simOS) []uint32 // devuelve la consola que se pregunta
		pedido         []string
		tree           bool
		also, bloquean []uint32
	}
	nodo := func(s *simOS, pid uint32, created int, parent uint32, consola []uint32) {
		s.add(&simProc{id: ID{PID: pid, Created: at(created)}, name: "node.exe", parent: parent, console: consola, ctrlCReacts: true, ctrlCAfter: 100 * time.Millisecond})
	}
	casos := []caso{
		{nombre: "padre al margen en la misma consola: nadie recibe, el padre lo impide", pedido: []string{"localhost:5173"},
			arma: func(s *simOS) []uint32 {
				c := []uint32{100, 101}
				nodo(s, 100, 50, 500, c)
				nodo(s, 101, 55, 100, c)
				s.sockets = append(s.sockets, listenAt("0.0.0.0", 5173, 100, at(60)), listenAt("::1", 5173, 101, at(65)))
				return c
			}, bloquean: []uint32{100}},
		{nombre: "padre al margen con --tree: igual", pedido: []string{"localhost:5173"}, tree: true,
			arma: func(s *simOS) []uint32 {
				c := []uint32{100, 101}
				nodo(s, 100, 50, 500, c)
				nodo(s, 101, 55, 100, c)
				s.sockets = append(s.sockets, listenAt("0.0.0.0", 5173, 100, at(60)), listenAt("::1", 5173, 101, at(65)))
				return c
			}, bloquean: []uint32{100}},
		{nombre: "padre que no escucha (npm): le llega también", pedido: []string{"5173"},
			arma: func(s *simOS) []uint32 {
				c := []uint32{100, 101}
				nodo(s, 100, 50, 500, c)
				nodo(s, 101, 55, 100, c)
				s.sockets = append(s.sockets, listenAt("::1", 5173, 101, at(65)))
				return c
			}, also: []uint32{100}},
		{nombre: "hijo que no escucha, sin --tree: le llega también", pedido: []string{"5173"},
			arma: func(s *simOS) []uint32 {
				c := []uint32{100, 101}
				nodo(s, 100, 50, 500, c)
				nodo(s, 101, 55, 100, c)
				s.sockets = append(s.sockets, listenAt("0.0.0.0", 5173, 100, at(60)))
				return c
			}, also: []uint32{101}},
		{nombre: "hijo con --tree: es víctima, no 'también'", pedido: []string{"5173"}, tree: true,
			arma: func(s *simOS) []uint32 {
				c := []uint32{100, 101}
				nodo(s, 100, 50, 500, c)
				nodo(s, 101, 55, 100, c)
				s.sockets = append(s.sockets, listenAt("0.0.0.0", 5173, 100, at(60)))
				return c
			}},
		{nombre: "un ajeno en la consola lo impide", pedido: []string{"5173"},
			arma: func(s *simOS) []uint32 {
				c := []uint32{101, 300}
				nodo(s, 101, 55, 500, c)
				nodo(s, 300, 40, 500, c)
				s.sockets = append(s.sockets, listenAt("::1", 5173, 101, at(65)))
				return c
			}, bloquean: []uint32{300}},
		{nombre: "un PID que no está en la foto lo impide", pedido: []string{"5173"},
			arma: func(s *simOS) []uint32 {
				c := []uint32{101, 999}
				nodo(s, 101, 55, 500, c)
				s.sockets = append(s.sockets, listenAt("::1", 5173, 101, at(65)))
				return c
			}, bloquean: []uint32{999}},
		{nombre: "dos objetivos hermanos en la misma consola: un solo Ctrl+C, nadie lo impide", pedido: []string{"5173", "5174"},
			arma: func(s *simOS) []uint32 {
				c := []uint32{100, 102}
				nodo(s, 100, 50, 500, c)
				nodo(s, 102, 52, 500, c)
				s.sockets = append(s.sockets, listenAt("::1", 5173, 100, at(60)), listenAt("::1", 5174, 102, at(62)))
				return c
			}},
		{nombre: "una consola sin víctimas del plan: nada que decir", pedido: []string{"5173"},
			arma: func(s *simOS) []uint32 {
				nodo(s, 101, 55, 500, []uint32{101})
				nodo(s, 300, 40, 500, []uint32{300})
				s.sockets = append(s.sockets, listenAt("::1", 5173, 101, at(65)))
				return []uint32{300}
			}},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			s, cfg, opts := escenario()
			cfg.Tree = c.tree
			consola := c.arma(s)
			q := pedido(t, c.pedido...)
			before, pl := enfocado(s, cfg, opts, q, nil)
			x := pl.CtrlCOutlook(before, consola)
			if got := pidsDe(x.Also); !slices.Equal(got, c.also) && (len(got) > 0 || len(c.also) > 0) {
				t.Errorf("Also = %v, quería %v", got, c.also)
			}
			if got := pidsDe(x.Blocked); !slices.Equal(got, c.bloquean) && (len(got) > 0 || len(c.bloquean) > 0) {
				t.Errorf("Blocked = %v, quería %v", got, c.bloquean)
			}
		})
	}
}

// Propiedad: lo que CtrlCOutlook avisa antes de preguntar es lo que el motor
// hace después. En familias al azar (padres, hijos, ajenos y PID que no están
// en la foto, repartidos en consolas; escuchando en direcciones distintas;
// pedidos con y sin dirección; con y sin --tree), para cada consola donde hay
// víctimas: si la predicción dice que alguien lo impide, el motor omite el
// Ctrl+C (EvCtrlCSkipped); si no, lo manda (EvCtrlC) y a los que no son
// víctimas les llega exactamente a los que dijo (Event.Also).
func TestCtrlCOutlookComoElMotor(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260928, 5))
	pedidos := [][]string{{"5173"}, {"localhost:5173"}, {"127.0.0.1:5173"}, {"[::1]:5173"}, {"5173", "5174"}}
	dirs := []string{"0.0.0.0", "::1", "127.0.0.1", "::"}
	casos, comparadas := 0, 0
	for it := 0; it < 600; it++ {
		s, cfg, opts := escenario()
		cfg.Tree = rng.IntN(2) == 0
		n := 2 + rng.IntN(5)
		type fam struct {
			pid     uint32
			consola int // 0 = sin consola, 1 o 2
		}
		var fs []fam
		for i := 0; i < n; i++ {
			fs = append(fs, fam{pid: uint32(100 + i), consola: rng.IntN(3)})
		}
		extra := map[int][]uint32{} // PID que no están en la foto, por consola
		if rng.IntN(4) == 0 {
			extra[1+rng.IntN(2)] = []uint32{999}
		}
		consolas := map[int][]uint32{}
		for _, f := range fs {
			if f.consola > 0 {
				consolas[f.consola] = append(consolas[f.consola], f.pid)
			}
		}
		for k, xs := range extra {
			if len(consolas[k]) > 0 {
				consolas[k] = append(consolas[k], xs...)
			}
		}
		t0 := 50
		for i, f := range fs {
			parent := uint32(500)
			if i > 0 && rng.IntN(3) > 0 {
				parent = fs[rng.IntN(i)].pid
			}
			s.add(&simProc{id: ID{PID: f.pid, Created: at(t0 + i)}, name: "node.exe", parent: parent,
				console: consolas[f.consola], ctrlCReacts: true, ctrlCAfter: 100 * time.Millisecond})
			switch rng.IntN(4) {
			case 0: // no escucha
			case 1:
				s.sockets = append(s.sockets, listenAt(dirs[rng.IntN(len(dirs))], 5174, f.pid, at(200+i)))
			default:
				s.sockets = append(s.sockets, listenAt(dirs[rng.IntN(len(dirs))], 5173, f.pid, at(200+i)))
			}
		}
		args := pedidos[rng.IntN(len(pedidos))]
		q := pedido(t, args...)
		before, pl := enfocado(s, cfg, opts, q, nil)
		if len(pl.Targets) == 0 {
			continue
		}
		casos++
		// La predicción, por consola con víctimas.
		pred := map[string]CtrlCOutlook{}
		for _, id := range pl.Victims() {
			p := s.procs[id]
			if p == nil || len(p.console) == 0 {
				continue
			}
			pred[consoleKey(p.console)] = pl.CtrlCOutlook(before, p.console)
		}
		_, rec := exec(s, before, q.Ports, pl, cfg, opts)
		visto := map[string]bool{}
		for _, e := range rec.evs {
			if e.Kind != EvCtrlC && e.Kind != EvCtrlCSkipped {
				continue
			}
			key := consoleKey(s.procs[e.Victim].console)
			visto[key] = true
			x, ok := pred[key]
			if !ok {
				t.Fatalf("it %d: el motor tocó una consola que la predicción no vio: %v", it, s.procs[e.Victim].console)
			}
			comparadas++
			desc := fmt.Sprintf("it %d (tree=%v, pedido %v, consola %v)", it, cfg.Tree, args, s.procs[e.Victim].console)
			switch {
			case len(x.Blocked) > 0 && e.Kind != EvCtrlCSkipped:
				t.Fatalf("%s: la predicción dice que lo impiden %v y el motor lo mandó", desc, pidsDe(x.Blocked))
			case len(x.Blocked) == 0 && e.Kind != EvCtrlC:
				t.Fatalf("%s: la predicción dice que se manda y el motor lo omitió: %s", desc, e.Detail)
			case e.Kind == EvCtrlC && !slices.Equal(pidsDe(x.Also), pidsDe(e.Also)):
				t.Fatalf("%s: la predicción dice que le llega también a %v y el motor a %v", desc, pidsDe(x.Also), pidsDe(e.Also))
			}
		}
		for key := range pred {
			if !visto[key] {
				t.Fatalf("it %d: la predicción habla de la consola %s y el motor no la tocó", it, key)
			}
		}
	}
	if casos < 300 || comparadas < 300 {
		t.Fatalf("pocos casos con objetivos (%d) o consolas comparadas (%d): el generador no ejercita la propiedad", casos, comparadas)
	}
}
