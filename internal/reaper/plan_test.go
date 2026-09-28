package reaper

import (
	"math/rand/v2"
	"net/netip"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
)

func proc(pid uint32, created int, name string, parent uint32) Proc {
	return Proc{ID: ID{PID: pid, Created: at(created)}, Name: name, Parent: parent}
}

// TestPlanFantasma: el dueño del socket (PID 900) ya no está en la instantánea;
// un hijo (PID 100) nacido después del socket lo heredó. El objetivo es el hijo.
func TestPlanFantasma(t *testing.T) {
	f := NewForest([]Proc{proc(100, 400, "node.exe", 900)}) // el 900 no existe
	socks := []ports.Socket{tcpListen(3000, 900, at(300))}
	pl := PlanKill(ports.Of(3000), socks, f, nil, Config{Self: kpSelf()})
	if len(pl.Targets) != 1 || pl.Targets[0].Kind != KillPhantom || pl.Targets[0].Root.PID != 100 {
		t.Fatalf("esperaba un objetivo fantasma con raíz PID 100: %+v", pl.Targets)
	}
	if pl.Targets[0].Reason == "" {
		t.Error("el fantasma debería explicarse")
	}
}

// TestPlanReciclado: el PID del dueño existe en la instantánea PERO con un
// proceso más nuevo que el socket → no es el dueño real → fantasma. El
// heredero es el hijo del muerto, que nació ANTES que el que reusó el PID; el
// hijo de ese proceso nuevo (arista válida) no pudo heredar nada.
func TestPlanReciclado(t *testing.T) {
	f := NewForest([]Proc{
		proc(900, 500, "otracosa.exe", 1), // reusó el PID 900 después del socket
		proc(100, 450, "node.exe", 900),   // heredero: nació con el muerto vivo
		proc(101, 550, "hijo.exe", 900),   // hijo de otracosa (arista válida)
	})
	socks := []ports.Socket{tcpListen(3000, 900, at(300))}
	pl := PlanKill(ports.Of(3000), socks, f, nil, Config{})
	if len(pl.Targets) != 1 || pl.Targets[0].Kind != KillPhantom || pl.Targets[0].Root.PID != 100 {
		t.Fatalf("un dueño más nuevo que el socket es impostor → fantasma con raíz 100: %+v", pl.Targets)
	}
}

// C-01: los herederos de un fantasma pasan por las mismas negativas que un
// dueño vivo, y los hijos del proceso que reusó el PID no son herederos.
func TestPlanFantasmaHerederos(t *testing.T) {
	socks := []ports.Socket{tcpListen(3000, 900, at(300))}
	objetivos := func(pl Plan) []uint32 {
		var out []uint32
		for _, tg := range pl.Targets {
			for _, v := range tg.Victims {
				out = append(out, v.PID)
			}
		}
		return out
	}
	t.Run("hijo del que reusó el PID", func(t *testing.T) {
		f := NewForest(con(
			proc(900, 500, "chrome.exe", 1),   // reusó el 900 después del socket (300)
			proc(901, 600, "chrome.exe", 900), // hijo de ESTE chrome
		))
		pl := PlanKill(ports.Of(3000), socks, f, nil, Config{Self: kpSelf(), SystemRoot: winRoot})
		if got := objetivos(pl); len(got) != 0 {
			t.Fatalf("el hijo del proceso nuevo no heredó nada y quedó de víctima: %v", got)
		}
		if len(pl.Refusals) != 1 || pl.Refusals[0].PID != 900 || !strings.Contains(pl.Refusals[0].Reason, "ningún proceso heredó") {
			t.Fatalf("negativas = %+v", pl.Refusals)
		}
	})
	t.Run("killport hijo del que reusó el PID", func(t *testing.T) {
		self := ID{PID: 700, Created: at(700)}
		f := NewForest(con(proc(900, 500, "pwsh.exe", 1), Proc{ID: self, Name: "killport.exe", Parent: 900}))
		pl := PlanKill(ports.Of(3000), socks, f, nil, Config{Self: self, SystemRoot: winRoot})
		if got := objetivos(pl); len(got) != 0 {
			t.Fatalf("killport es víctima de su propio plan: %v", got)
		}
	})
	t.Run("heredero intocable", func(t *testing.T) {
		// Hijos crudos del 900 muerto, nacidos tras el socket, sin padre
		// válido: el bosque no los distingue de un heredero. Las negativas sí.
		self := ID{PID: 700, Created: at(350)}
		f := NewForest(con(
			proc(950, 400, "wslrelay.exe", 900),
			Proc{ID: self, Name: "killport.exe", Parent: 900},
			Proc{ID: ID{PID: 960, Created: at(420)}, Name: "csrss.exe", Parent: 900, Path: winRoot + `\System32\csrss.exe`, SessionID: 1},
			proc(970, 450, "node.exe", 900), // este sí: un heredero común
		))
		pl := PlanKill(ports.Of(3000), socks, f, nil, Config{Self: self, SystemRoot: winRoot})
		if got := objetivos(pl); len(got) != 1 || got[0] != 970 || pl.Targets[0].Kind != KillPhantom {
			t.Fatalf("víctimas = %v (%+v)", got, pl.Targets)
		}
		negados := map[uint32]string{}
		for _, r := range pl.Refusals {
			negados[r.PID] = r.Reason
			if r.Relay != RelayNone {
				t.Errorf("la negativa de un heredero no se desenmascara: %+v", r)
			}
		}
		for _, pid := range []uint32{950, 700, 960} {
			if !strings.Contains(negados[pid], "heredó del PID 900") {
				t.Errorf("el heredero %d tenía que quedar negado con la historia del socket: %q", pid, negados[pid])
			}
		}
	})
}

// TestPlanNegativas: cada clase intocable cae en Refusals, no en Targets.
func TestPlanNegativas(t *testing.T) {
	self := kpSelf()
	f := NewForest(con(
		proc(500, 0, "pwsh.exe", 0), // terminal (ancestro de killport)
		Proc{ID: self, Name: "killport.exe", Parent: 500},
		svchostReal(800, 5),
		sys32(900, 5, "svcs.exe", pidServices), // servicio del sistema (imagen bajo SystemRoot)
	))
	svc := map[uint32][]Service{
		800: {{Name: "Dnscache"}},
		900: {{Name: "Themes"}},
	}
	// A cada intocable, un puerto propio.
	socks := []ports.Socket{
		tcpListen(500, 500, at(10)), tcpListen(600, self.PID, at(10)),
		tcpListen(80, 4, at(10)), tcpListen(88, pidLsass, at(10)),
		tcpListen(53, 800, at(10)), tcpListen(5357, 900, at(10)),
	}
	requested := ports.NewSet(ports.Range{Lo: 1, Hi: 65535})
	pl := PlanKill(requested, socks, f, svc, Config{Self: self, SystemRoot: winRoot})
	if len(pl.Targets) != 0 {
		t.Fatalf("no debería haber ningún objetivo, todo es intocable: %+v", pl.Targets)
	}
	if len(pl.Refusals) != 6 {
		t.Fatalf("esperaba 6 negativas, hubo %d: %+v", len(pl.Refusals), pl.Refusals)
	}
	for _, r := range pl.Refusals {
		if r.Reason == "" {
			t.Errorf("negativa sin motivo: %+v", r)
		}
	}
	// La del kernel en el 80 debe mencionar http.sys.
	for _, r := range pl.Refusals {
		if r.Ports.Contains(80) && r.Advice == "" {
			t.Errorf("la negativa del 80 (http.sys) debería aconsejar: %+v", r)
		}
	}
}

// TestPlanServicioTerceros: postgres (imagen fuera de Windows) → detener servicio.
func TestPlanServicioTerceros(t *testing.T) {
	f := NewForest([]Proc{{ID: ID{PID: 200, Created: at(0)}, Name: "postgres.exe", Path: `C:\Program Files\PostgreSQL\bin\postgres.exe`}})
	socks := []ports.Socket{tcpListen(5432, 200, at(10))}
	svc := map[uint32][]Service{200: {{Name: "postgresql-x64-16"}}}
	pl := PlanKill(ports.Of(5432), socks, f, svc, Config{SystemRoot: `C:\Windows`})
	if len(pl.Targets) != 1 || pl.Targets[0].Kind != StopThirdSvc || pl.Targets[0].Service != "postgresql-x64-16" {
		t.Fatalf("esperaba detener el servicio de terceros: %+v", pl.Targets)
	}
}

// TestPlanTreeAntichain: con --tree, un objetivo descendiente de otro se
// absorbe; cada víctima aparece una sola vez, y la raíz va antes que sus hijos.
func TestPlanTreeAntichain(t *testing.T) {
	// npm (10) → node (20) → worker (30). node y worker escuchan puertos.
	f := NewForest([]Proc{
		proc(10, 0, "npm.exe", 1),
		proc(20, 100, "node.exe", 10),
		proc(30, 200, "worker.exe", 20),
	})
	socks := []ports.Socket{tcpListen(3000, 20, at(150)), tcpListen(3001, 30, at(250))}
	pl := PlanKill(ports.Of(3000).Union(ports.Of(3001)), socks, f, nil, Config{Tree: true})
	// worker (30) es descendiente de node (20): se absorbe. Queda 1 objetivo.
	if len(pl.Targets) != 1 {
		t.Fatalf("el antichain debería dejar 1 objetivo: %+v", pl.Targets)
	}
	vic := pl.Targets[0].Victims
	if len(vic) != 2 || vic[0].PID != 20 || vic[1].PID != 30 {
		t.Fatalf("víctimas = %v, esperaba [20, 30] (raíz primero)", vic)
	}
	if !pl.Targets[0].Ports.Contains(3000) || !pl.Targets[0].Ports.Contains(3001) {
		t.Errorf("el objetivo absorbió ambos puertos: %v", pl.Targets[0].Ports)
	}
}

// randPlanForest arma procesos con PID únicos e incluye siempre a killport, su
// terminal, el kernel y algún crítico, para la propiedad de seguridad.
func randPlanForest(rng *rand.Rand) (*Forest, ID, []Proc) {
	self := ID{PID: 600, Created: at(10)}
	procs := con(
		proc(500, 0, "pwsh.exe", 0),
		Proc{ID: self, Name: "killport.exe", Parent: 500},
	)
	pids := rng.Perm(4000)
	n := 3 + rng.IntN(20)
	for i := 0; i < n; i++ {
		pid := uint32(pids[i]) + 1000
		parent := procs[rng.IntN(len(procs))].PID
		procs = append(procs, proc(pid, 100+rng.IntN(900), "app.exe", parent))
	}
	return NewForest(procs), self, procs
}

// TestPlanSeguridadPropiedad: sobre muchos bosques aleatorios, el plan NUNCA
// planea tocar el PID 0/4, un crítico, killport ni sus ancestros; y con --tree
// cada víctima aparece una sola vez.
func TestPlanSeguridadPropiedad(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 99))
	for iter := 0; iter < 400; iter++ {
		f, self, procs := randPlanForest(rng)
		cfg := Config{Self: self, SystemRoot: winRoot, Tree: rng.IntN(2) == 0}
		w := newWorld(f, nil, cfg)

		// Cadena prohibida: killport y todos sus ancestros.
		forbidden := map[ID]bool{self: true, {PID: 0}: true}
		for _, a := range f.Ancestors(self) {
			forbidden[a.ID] = true
		}

		// Sockets al azar, algunos sobre procesos prohibidos.
		var socks []ports.Socket
		port := uint16(2000)
		for _, p := range procs {
			if rng.IntN(2) == 0 {
				continue
			}
			socks = append(socks, tcpListen(port, p.PID, at(50)))
			port++
		}
		requested := ports.NewSet(ports.Range{Lo: 1, Hi: 65535})
		pl := PlanKill(requested, socks, f, nil, cfg)

		seen := map[ID]bool{}
		for _, tg := range pl.Targets {
			for _, v := range tg.Victims {
				if v.PID <= 4 {
					t.Fatalf("iter %d: planea tocar el PID %d", iter, v.PID)
				}
				if forbidden[v] {
					t.Fatalf("iter %d: planea tocar a killport o a su terminal: %v", iter, v)
				}
				if p, ok := f.Lookup(v); ok && w.critical(p) {
					t.Fatalf("iter %d: planea tocar un crítico: %v", iter, p.Name)
				}
				if cfg.Tree && seen[v] {
					t.Fatalf("iter %d: con --tree la víctima %v aparece dos veces", iter, v)
				}
				seen[v] = true
			}
		}
	}
}

// TestPlanAllStatesResiduo: una conexión en TIME_WAIT del PID 0 se informa pero
// no genera objetivo (no hay a quién cerrar).
func TestPlanResiduoNoEsObjetivo(t *testing.T) {
	f := NewForest(nil)
	tw := ports.Socket{Proto: ports.TCP, Local: netip.MustParseAddrPort("127.0.0.1:3000"), State: ports.TimeWait, PID: 0, Since: at(0)}
	// Con el filtro Holds normal, un TIME_WAIT del PID 0 no ocupa: no llega acá.
	filter := ports.Filter{Ports: ports.Of(3000), AllStates: true}
	if filter.Relate(tw) != ports.Residue {
		t.Fatalf("un TIME_WAIT del PID 0 debería ser Residue")
	}
	// Y si igual llegara como holding, no hay dueño vivo → fantasma sin heredero
	// → negativa, nunca objetivo.
	pl := PlanKill(ports.Of(3000), []ports.Socket{tw}, f, nil, Config{})
	if len(pl.Targets) != 0 {
		t.Fatalf("no debería haber objetivo: %+v", pl.Targets)
	}
}
