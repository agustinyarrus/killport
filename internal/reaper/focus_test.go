package reaper

import (
	"math/rand"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

func listenAt(addr string, port uint16, pid uint32, since time.Time) ports.Socket {
	return ports.Socket{Proto: ports.TCP, Local: netip.AddrPortFrom(netip.MustParseAddr(addr), port), State: ports.Listen, PID: pid, Since: since}
}

func pedido(t *testing.T, args ...string) ports.Request {
	t.Helper()
	q, err := ports.ParseRequest(args)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// escenarioCompartido: el puerto compartido en silencio del contrato. node en
// [::1]:5173 (contesta localhost) y python en 0.0.0.0:5173 (contesta
// 127.0.0.1 y la red), los dos hijos del shell y los dos cierran con Ctrl+C.
func escenarioCompartido() (*simOS, Config, Options, *simProc, *simProc) {
	s, cfg, opts := escenario()
	node := s.add(&simProc{id: ID{PID: 100, Created: at(50)}, name: "node.exe", parent: 500, console: []uint32{100}, ctrlCReacts: true, ctrlCAfter: 100 * time.Millisecond})
	py := s.add(&simProc{id: ID{PID: 200, Created: at(70)}, name: "python.exe", parent: 500, console: []uint32{200}, ctrlCReacts: true, ctrlCAfter: 100 * time.Millisecond})
	s.sockets = append(s.sockets, listenAt("::1", 5173, 100, at(60)), listenAt("0.0.0.0", 5173, 200, at(80)))
	return s, cfg, opts, node, py
}

func enfocado(s *simOS, cfg Config, opts Options, q ports.Request, picked func(ports.Socket) bool) (*Forest, Plan) {
	before, pl := buildPlan(s, cfg, q.Ports, opts.Filter)
	return before, Focus(pl, q, picked)
}

// localhost:5173 apunta SOLO a node: python queda afuera y sigue vivo. Lo
// pedido NO quedó libre: ahora localhost llega a python (el navegador cae a
// 127.0.0.1). Antes se lo daba por libre ("✓ localhost:5173 libre", exit 0)
// con esa misma frase al lado (X-14).
func TestFocusLocalhost(t *testing.T) {
	s, cfg, opts, node, py := escenarioCompartido()
	q := pedido(t, "localhost:5173")
	before, pl := enfocado(s, cfg, opts, q, nil)
	if len(pl.Targets) != 1 || pl.Targets[0].Root != node.id || len(pl.Bystanders) != 1 || pl.Bystanders[0].Owner != py.id {
		t.Fatalf("plan = %+v / al margen %+v", pl.Targets, pl.Bystanders)
	}
	rep, _ := exec(s, before, q.Ports, pl, cfg, opts)
	assertVictim(t, rep, OutcomeClosed, MethodCtrlC)
	if py.exited {
		t.Fatal("python no se pidió y cerró igual")
	}
	pr := assertPort(t, rep, PortAnswered)
	if pr.Bystander != py.id || !strings.Contains(pr.Reason, "ahora localhost (por 127.0.0.1) llega a python.exe (PID 200)") {
		t.Fatalf("puerto = %+v", pr)
	}
	if free, total := rep.FreedPorts(); free != 0 || total != 1 {
		t.Fatalf("libres %d de %d: localhost sigue contestando", free, total)
	}
	if s.probes != 0 || s.exclReads != 0 {
		t.Fatalf("un puerto que tiene alguien atado no se diagnostica (sondeos %d, netsh %d)", s.probes, s.exclReads)
	}
}

// 127.0.0.1:5173 apunta a python; node, en [::1], no contesta esa dirección.
func TestFocus127(t *testing.T) {
	s, cfg, opts, node, py := escenarioCompartido()
	q := pedido(t, "127.0.0.1:5173")
	before, pl := enfocado(s, cfg, opts, q, nil)
	if len(pl.Targets) != 1 || pl.Targets[0].Root != py.id {
		t.Fatalf("plan = %+v", pl.Targets)
	}
	rep, _ := exec(s, before, q.Ports, pl, cfg, opts)
	pr := assertPort(t, rep, PortFree)
	if node.exited || pr.Bystander != node.id || !strings.Contains(pr.Reason, "nadie contesta 127.0.0.1; node.exe (PID 100) escucha el 5173 en [::1]") {
		t.Fatalf("puerto = %+v", pr)
	}
}

// Sin dirección, los dos: el pedido de siempre.
func TestFocusPuertoEntero(t *testing.T) {
	s, cfg, opts, node, py := escenarioCompartido()
	q := pedido(t, "5173")
	before, pl := enfocado(s, cfg, opts, q, nil)
	if len(pl.Targets) != 2 || len(pl.Bystanders) != 0 {
		t.Fatalf("plan = %+v", pl)
	}
	rep, _ := exec(s, before, q.Ports, pl, cfg, opts)
	if pr := assertPort(t, rep, PortFree); !pr.Bystander.Zero() || pr.Reason != "" || !node.exited || !py.exited {
		t.Fatalf("puerto = %+v", pr)
	}
}

// Un supervisor relanza a node en [::1]: eso SÍ contesta localhost, es un
// respawn aunque python siga en 0.0.0.0.
func TestFocusRespawnEnLaDireccion(t *testing.T) {
	s, cfg, opts, _, _ := escenarioCompartido()
	s.add(&simProc{id: ID{PID: 10, Created: at(0)}, name: "nodemon.exe", parent: 500})
	s.procs[ID{PID: 100, Created: at(50)}].parent = 10
	q := pedido(t, "localhost:5173")
	before, pl := enfocado(s, cfg, opts, q, nil)
	s.afterFn = func(*simOS) []ports.Socket {
		return []ports.Socket{listenAt("0.0.0.0", 5173, 200, at(80)), listenAt("::1", 5173, 101, at(1000))}
	}
	s.snapFn = func(*simOS) []Proc {
		return []Proc{
			{ID: ID{PID: 500, Created: at(0)}, Name: "pwsh.exe"},
			{ID: ID{PID: 10, Created: at(0)}, Name: "nodemon.exe", Parent: 500},
			{ID: ID{PID: 101, Created: at(1000)}, Name: "node.exe", Parent: 10},
			{ID: ID{PID: 200, Created: at(70)}, Name: "python.exe", Parent: 500},
		}
	}
	rep, _ := exec(s, before, q.Ports, pl, cfg, opts)
	if pr := assertPort(t, rep, PortRespawned); pr.SupervisorName != "nodemon.exe" || pr.Owner.PID != 101 {
		t.Fatalf("puerto = %+v", pr)
	}
}

// Un python NUEVO en 0.0.0.0 a la sombra del de antes no contesta localhost:
// no cuenta. Pero uno nuevo en la misma dirección que el que no se pidió
// (reparto indefinido) sí.
func TestFocusALaSombra(t *testing.T) {
	s, cfg, opts, _, _ := escenarioCompartido()
	q := pedido(t, "127.0.0.1:5173")
	s.procs[ID{PID: 200, Created: at(70)}].ctrlCReacts = false
	s.procs[ID{PID: 200, Created: at(70)}].openErr = ErrAccessDenied // python no se va
	before, pl := enfocado(s, cfg, opts, q, nil)
	rep, _ := exec(s, before, q.Ports, pl, cfg, opts)
	assertPort(t, rep, PortBusy)

	// localhost: node se va; aparece OTRO en [::] (a la sombra de nadie: contesta ::1).
	s, cfg, opts, _, _ = escenarioCompartido()
	q = pedido(t, "localhost:5173")
	before, pl = enfocado(s, cfg, opts, q, nil)
	s.add(&simProc{id: ID{PID: 300, Created: at(890)}, name: "otro.exe", parent: 500})
	s.afterFn = func(*simOS) []ports.Socket {
		return []ports.Socket{listenAt("0.0.0.0", 5173, 200, at(80)), listenAt("::", 5173, 300, at(900))}
	}
	rep, _ = exec(s, before, q.Ports, pl, cfg, opts)
	if pr := assertPort(t, rep, PortRespawned); pr.Owner.PID != 300 {
		t.Fatalf("el que llegó a [::] contesta ::1: %+v", pr)
	}
	// 127.0.0.1: python se va; aparece otro en [::1], que no contesta
	// 127.0.0.1 (está a la sombra de nadie: es otra dirección). Libre.
	s, cfg, opts, _, _ = escenarioCompartido()
	q = pedido(t, "127.0.0.1:5173")
	before, pl = enfocado(s, cfg, opts, q, nil)
	s.add(&simProc{id: ID{PID: 301, Created: at(890)}, name: "otro.exe", parent: 500})
	s.afterFn = func(*simOS) []ports.Socket {
		return []ports.Socket{listenAt("::1", 5173, 100, at(60)), listenAt("::1", 5173, 301, at(900))}
	}
	rep, _ = exec(s, before, q.Ports, pl, cfg, opts)
	if pr := assertPort(t, rep, PortFree); !strings.Contains(pr.Reason, "nadie contesta 127.0.0.1") {
		t.Fatalf("uno nuevo en [::1] no contesta 127.0.0.1: %+v", pr)
	}
	// Dos en 0.0.0.0 (reparto indefinido: Windows no promete a cuál llega):
	// 127.0.0.1 lo pueden contestar los dos, así que los dos son objetivo, el
	// más viejo primero. node, en [::1], no.
	s, cfg, opts, _, _ = escenarioCompartido()
	s.add(&simProc{id: ID{PID: 250, Created: at(20)}, name: "viejo.exe", parent: 500})
	s.sockets = append(s.sockets, listenAt("0.0.0.0", 5173, 250, at(30)))
	q = pedido(t, "127.0.0.1:5173")
	_, pl = enfocado(s, cfg, opts, q, nil)
	if len(pl.Targets) != 2 || len(pl.Bystanders) != 1 || pl.Bystanders[0].PID != 100 {
		t.Fatalf("los dos de 0.0.0.0 contestan 127.0.0.1: %+v / %+v", pl.Targets, pl.Bystanders)
	}
}

// --tree con un padre y su hijo en el mismo puerto (el padre en 0.0.0.0, el
// hijo en [::1]). La planificación los junta (antichain: el hijo se absorbe
// en el padre). Focus lo deshace:
//   - localhost apunta al HIJO: el padre no cae por ser su ancestro, queda al
//     margen y sigue vivo;
//   - 127.0.0.1 apunta al PADRE: el hijo cae con él (eso pide --tree), así que
//     no se lo nombra "al margen".
func TestFocusTree(t *testing.T) {
	arma := func() (*simOS, Config, Options, *simProc, *simProc) {
		s, cfg, opts := escenario()
		cfg.Tree = true
		padre := s.add(&simProc{id: ID{PID: 100, Created: at(50)}, name: "node.exe", parent: 500, console: []uint32{100, 101}, ctrlCReacts: true, ctrlCAfter: 100 * time.Millisecond})
		hijo := s.add(&simProc{id: ID{PID: 101, Created: at(55)}, name: "node.exe", parent: 100, console: []uint32{100, 101}, ctrlCReacts: true, ctrlCAfter: 100 * time.Millisecond})
		s.sockets = append(s.sockets, listenAt("0.0.0.0", 5173, 100, at(60)), listenAt("::1", 5173, 101, at(65)))
		return s, cfg, opts, padre, hijo
	}

	s, cfg, opts, padre, hijo := arma()
	q := pedido(t, "localhost:5173")
	before, pl := buildPlan(s, cfg, q.Ports, opts.Filter)
	if len(pl.Targets) != 1 || pl.Targets[0].Root != padre.id {
		t.Fatalf("sin enfocar, el antichain junta al hijo en el padre: %+v", pl.Targets)
	}
	pl = Focus(pl, q, nil)
	if len(pl.Targets) != 1 || pl.Targets[0].Root != hijo.id || !reflect.DeepEqual(pl.Targets[0].Victims, []ID{hijo.id}) ||
		len(pl.Bystanders) != 1 || pl.Bystanders[0].Owner != padre.id {
		t.Fatalf("localhost con --tree: objetivos %+v, al margen %+v", pl.Targets, pl.Bystanders)
	}
	rep, _ := exec(s, before, q.Ports, pl, cfg, opts)
	if padre.exited || !hijo.exited {
		t.Fatalf("cerró al padre (%v) o no al hijo (%v)", padre.exited, hijo.exited)
	}
	assertPort(t, rep, PortAnswered) // localhost llega ahora al padre, en 0.0.0.0

	s, cfg, opts, padre, hijo = arma()
	q = pedido(t, "127.0.0.1:5173")
	_, pl = buildPlan(s, cfg, q.Ports, opts.Filter)
	pl = Focus(pl, q, nil)
	if len(pl.Targets) != 1 || pl.Targets[0].Root != padre.id || !reflect.DeepEqual(pl.Targets[0].Victims, []ID{padre.id, hijo.id}) || len(pl.Bystanders) != 0 {
		t.Fatalf("127.0.0.1 con --tree: objetivos %+v, al margen %+v", pl.Targets, pl.Bystanders)
	}
	// Un selector con --tree, igual: -n que elige solo al hijo.
	s, cfg, opts, padre, hijo = arma()
	q = ports.Whole(ports.Of(5173))
	_, pl = buildPlan(s, cfg, q.Ports, opts.Filter)
	pl = Focus(pl, q, func(sk ports.Socket) bool { return sk.PID == 101 })
	if len(pl.Targets) != 1 || pl.Targets[0].Root != hijo.id || len(pl.Bystanders) != 1 || pl.Bystanders[0].Owner != padre.id {
		t.Fatalf("selector con --tree: objetivos %+v, al margen %+v", pl.Targets, pl.Bystanders)
	}
	// Tres generaciones, absorbidas en cadena: se deshacen enteras.
	s, cfg, opts, padre, hijo = arma()
	nieto := s.add(&simProc{id: ID{PID: 102, Created: at(57)}, name: "node.exe", parent: 101})
	s.sockets = append(s.sockets, listenAt("::1", 5174, 102, at(66)))
	q = pedido(t, "localhost:5173", "5174")
	_, pl = buildPlan(s, cfg, q.Ports, opts.Filter)
	pl = Focus(pl, q, nil)
	if len(pl.Targets) != 1 || pl.Targets[0].Root != hijo.id || !reflect.DeepEqual(pl.Targets[0].Victims, []ID{hijo.id, nieto.id}) ||
		!pl.Targets[0].Ports.Equal(ports.Of(5173, 5174)) || len(pl.Bystanders) != 1 || pl.Bystanders[0].Owner != padre.id {
		t.Fatalf("tres generaciones: objetivos %+v, al margen %+v", pl.Targets, pl.Bystanders)
	}
}

// El Ctrl+C va a la CONSOLA. Si el hijo pedido la comparte con su padre, que
// quedó al margen, mandarlo cerraría también al padre (al que se le dijo al
// usuario que no se toca). Lo de al margen no entra al conjunto permitido: el
// Ctrl+C se omite por consola compartida, el padre no recibe nada, y el hijo
// se termina en seco. Con o sin --tree.
func TestFocusAlMargenNoRecibeElCtrlC(t *testing.T) {
	for _, tree := range []bool{false, true} {
		s, cfg, opts := escenario()
		cfg.Tree = tree
		padre := s.add(&simProc{id: ID{PID: 100, Created: at(50)}, name: "node.exe", parent: 500, console: []uint32{100, 101}, ctrlCReacts: true, ctrlCAfter: 100 * time.Millisecond})
		hijo := s.add(&simProc{id: ID{PID: 101, Created: at(55)}, name: "node.exe", parent: 100, console: []uint32{100, 101}, ctrlCReacts: true, ctrlCAfter: 100 * time.Millisecond})
		s.sockets = append(s.sockets, listenAt("0.0.0.0", 5173, 100, at(60)), listenAt("::1", 5173, 101, at(65)))
		q := pedido(t, "localhost:5173")
		before, pl := enfocado(s, cfg, opts, q, nil)
		rep, rec := exec(s, before, q.Ports, pl, cfg, opts)
		for _, allow := range s.ctrlCAllow {
			for _, id := range allow {
				if id == padre.id {
					t.Fatalf("tree=%v: el padre (al margen) entró al conjunto permitido del Ctrl+C: %v", tree, allow)
				}
			}
		}
		if padre.scheduled || rec.has(EvCtrlC) || !rec.has(EvCtrlCSkipped) {
			t.Fatalf("tree=%v: al padre le llegó el Ctrl+C (programado %v) o no se omitió: %v", tree, padre.scheduled, rec.kinds())
		}
		assertVictim(t, rep, OutcomeTerminated, MethodTerminate)
		if !hijo.exited {
			t.Fatalf("tree=%v: el hijo no cerró", tree)
		}
		assertPort(t, rep, PortAnswered) // localhost llega ahora al padre
	}
	// Sin nadie al margen en la consola, el Ctrl+C se manda como siempre.
	s, cfg, opts := escenario()
	padre := s.add(&simProc{id: ID{PID: 100, Created: at(50)}, name: "node.exe", parent: 500, console: []uint32{100, 101}, ctrlCReacts: true, ctrlCAfter: 100 * time.Millisecond})
	s.add(&simProc{id: ID{PID: 101, Created: at(55)}, name: "node.exe", parent: 100, console: []uint32{100, 101}, ctrlCReacts: true, ctrlCAfter: 100 * time.Millisecond})
	s.sockets = append(s.sockets, listenAt("::1", 5173, 101, at(65)))
	q := pedido(t, "localhost:5173")
	before, pl := enfocado(s, cfg, opts, q, nil)
	rep, rec := exec(s, before, q.Ports, pl, cfg, opts)
	if !rec.has(EvCtrlC) || !padre.scheduled {
		t.Fatalf("el padre, que no escucha el puerto, comparte la consola como siempre: %v", rec.kinds())
	}
	assertVictim(t, rep, OutcomeClosed, MethodCtrlC)
}

// --tree y http.sys: node escucha el 3000 y ADEMÁS registró una URL del 8080
// (el socket es de System); su hijo escucha el 3001. El antichain junta al
// hijo en node, y después Unmask le suma a node el 8080 de http.sys. Focus,
// al deshacer, tiene que encontrar ese 8080 en node (es suyo) aunque se lo
// hayan sumado después de absorber: si no, node quedaría al margen por su
// 3000 (que no contesta [::1]) y el 8080 pedido no tendría a nadie.
func TestFocusTreeConHTTPSys(t *testing.T) {
	s, cfg, opts := escenario()
	cfg.Tree = true
	for _, p := range windows() {
		s.add(&simProc{id: p.ID, name: p.Name, parent: p.Parent, path: p.Path})
	}
	node := s.add(&simProc{id: ID{PID: 100, Created: at(50)}, name: "node.exe", parent: 500})
	hijo := s.add(&simProc{id: ID{PID: 101, Created: at(55)}, name: "node.exe", parent: 100})
	s.sockets = append(s.sockets, listenAt("0.0.0.0", 3000, 100, at(60)), listenAt("::1", 3001, 101, at(65)), listenAt("0.0.0.0", 8080, 4, at(5)))
	q := pedido(t, "[::1]:3000", "127.0.0.1:8080", "[::1]:3001")
	_, pl := buildPlan(s, cfg, q.Ports, opts.Filter)
	pl = Unmask(pl, []Unmasking{{Via: systemID, Port: 8080, Proto: ports.TCP, Verdict: UnmaskFound, Procs: []ID{node.id}}})
	if len(pl.Targets) != 1 || pl.Targets[0].Root != node.id || !pl.Targets[0].Ports.Equal(ports.Of(3000, 3001, 8080)) {
		t.Fatalf("antes de enfocar: %+v", pl.Targets)
	}
	pl = Focus(pl, q, nil)
	if len(pl.Targets) != 1 || pl.Targets[0].Root != node.id || !reflect.DeepEqual(pl.Targets[0].Victims, []ID{node.id, hijo.id}) || len(pl.Bystanders) != 0 {
		t.Fatalf("node sigue siendo objetivo por el 8080 que contesta 127.0.0.1: %+v / al margen %+v", pl.Targets, pl.Bystanders)
	}
}

// Un selector (lo que haría -n node): el resto del puerto queda al margen.
func TestFocusSelector(t *testing.T) {
	s, cfg, opts, node, py := escenarioCompartido()
	q := ports.Whole(ports.Of(5173))
	before, pl := enfocado(s, cfg, opts, q, func(sk ports.Socket) bool { return sk.PID == 100 })
	if len(pl.Targets) != 1 || pl.Targets[0].Root != node.id {
		t.Fatalf("plan = %+v", pl.Targets)
	}
	rep, _ := exec(s, before, q.Ports, pl, cfg, opts)
	pr := assertPort(t, rep, PortFree)
	if py.exited || !strings.Contains(pr.Reason, "sigue escuchando python.exe (PID 200) en 0.0.0.0, que no se pidió") {
		t.Fatalf("puerto = %+v", pr)
	}
}

// Nadie contesta lo pedido: no hay objetivos, el que escucha queda al margen,
// y no se diagnostica un puerto que tiene alguien atado.
func TestFocusNadieContesta(t *testing.T) {
	s, cfg, opts, _, _ := escenarioCompartido()
	s.sockets = s.sockets[:1] // solo node en [::1]
	q := pedido(t, "127.0.0.1:5173")
	before, pl := enfocado(s, cfg, opts, q, nil)
	if !pl.Empty() || len(pl.Bystanders) != 1 || !pl.Ownerless.IsEmpty() {
		t.Fatalf("plan = %+v", pl)
	}
	rep, _ := exec(s, before, q.Ports, pl, cfg, opts)
	pr := assertPort(t, rep, PortFree)
	if !strings.Contains(pr.Reason, "nadie contesta 127.0.0.1") || s.probes != 0 {
		t.Fatalf("puerto = %+v (sondeos %d)", pr, s.probes)
	}
}

// Docker Desktop publica con dos intermediarios: localhost llega por
// wslrelay ([::1]) al mismo contenedor que 127.0.0.1 por el motor. Enfocado
// DESPUÉS de Unmask, el contenedor es el objetivo con los dos.
func TestFocusContenedorPorLosDosIntermediarios(t *testing.T) {
	for _, arg := range []string{"localhost:5432", "127.0.0.1:5432", "[::]:5432"} {
		_, _, _, _, pl, _ := escenarioDosRelays(t, &simExt{closeAfter: 50 * time.Millisecond})
		pl = Focus(pl, pedido(t, arg), nil)
		if len(pl.Targets) != 1 || pl.Targets[0].Kind != StopExternal || len(pl.Bystanders) != 0 {
			t.Fatalf("%s: plan = %+v", arg, pl)
		}
	}
}

// Propiedades: Focus reparte lo que había en pedido y al margen sin inventar
// ni perder nada; sin dirección ni selector no cambia el plan.
func TestFocusPropiedades(t *testing.T) {
	rng := rand.New(rand.NewSource(1234))
	dirs := []string{"0.0.0.0", "::", "127.0.0.1", "::1"}
	args := []string{"5173", "localhost:5173", "127.0.0.1:5173", "[::1]:5173", "0.0.0.0:5173", "[::]:5173"}
	for caso := 0; caso < 3000; caso++ {
		s, cfg, opts := escenario()
		for i := 0; i < 1+rng.Intn(4); i++ {
			pid := uint32(100 + i)
			s.add(&simProc{id: ID{PID: pid, Created: at(50 + i)}, name: "srv.exe", parent: 500})
			for j := 0; j < 1+rng.Intn(2); j++ {
				s.sockets = append(s.sockets, listenAt(dirs[rng.Intn(len(dirs))], 5173, pid, at(60+10*i+j)))
			}
		}
		q := pedido(t, args[rng.Intn(len(args))])
		var picked func(ports.Socket) bool
		if rng.Intn(2) == 0 {
			want := uint32(100 + rng.Intn(4))
			picked = func(sk ports.Socket) bool { return sk.PID == want }
		}
		_, orig := buildPlan(s, cfg, q.Ports, opts.Filter)
		pl := Focus(orig, q, picked)
		if picked == nil && !q.Aimed() && !reflect.DeepEqual(pl.Targets, orig.Targets) {
			t.Fatalf("caso %d: sin dirección ni selector el plan cambió", caso)
		}
		seen := map[uint32]int{}
		for _, tg := range pl.Targets {
			seen[tg.Root.PID]++
		}
		for _, b := range pl.Bystanders {
			seen[b.PID]++
		}
		for _, tg := range orig.Targets {
			if seen[tg.Root.PID] != 1 {
				t.Fatalf("caso %d: %d quedó %d veces entre pedidos y al margen", caso, tg.Root.PID, seen[tg.Root.PID])
			}
		}
		if len(pl.Targets)+len(pl.Bystanders) != len(orig.Targets) {
			t.Fatalf("caso %d: aparecieron o se perdieron objetivos", caso)
		}
		// Con una dirección que contesta alguien, el que contesta está pedido.
		if ds := q.Dests(5173); len(ds) == 1 && picked == nil {
			var all []ports.Socket
			for _, tg := range orig.Targets {
				all = append(all, tg.Sockets...)
			}
			who, _ := ports.AnswerDest(ds[0], ports.TCP, 5173, all)
			for _, w := range who {
				found := false
				for _, tg := range pl.Targets {
					found = found || tg.Root.PID == w.PID
				}
				if !found {
					t.Fatalf("caso %d: %v contesta %v y quedó al margen", caso, w.PID, ds[0])
				}
			}
		}
	}
}
