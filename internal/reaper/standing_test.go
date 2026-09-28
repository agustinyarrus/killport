package reaper

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
)

const winRoot = `C:\Windows`

// Los PID del esqueleto de Windows de las pruebas. smssSesion es el smss.exe
// que lanzó a csrss, wininit y winlogon y ya salió: no está en la instantánea.
const (
	pidSmss     = 41
	smssSesion  = 42
	pidCsrss    = 43
	pidWininit  = 44
	pidServices = 45
	pidLsass    = 46
	pidWinlogon = 47
	pidRegistry = 48
)

// sys32 arma un proceso cuya imagen está en System32, como los de Windows.
func sys32(pid uint32, created int, name string, parent uint32) Proc {
	p := proc(pid, created, name, parent)
	p.Path = winRoot + `\System32\` + name
	return p
}

// windows es el esqueleto de un Windows de verdad: cada proceso de sistema en
// su lugar y con su linaje (medido en Windows 11: ver standing.go).
func windows() []Proc {
	return []Proc{
		proc(4, 0, "System", 0),
		proc(pidRegistry, 0, "Registry", 4),
		sys32(pidSmss, 1, "smss.exe", 4),
		sys32(pidCsrss, 2, "csrss.exe", smssSesion),
		sys32(pidWininit, 2, "wininit.exe", smssSesion),
		sys32(pidServices, 3, "services.exe", pidWininit),
		sys32(pidLsass, 3, "lsass.exe", pidWininit),
		sys32(pidWinlogon, 3, "winlogon.exe", smssSesion),
	}
}

// svchostReal es un host de servicios genuino: en System32 e hijo de services.exe.
func svchostReal(pid uint32, created int) Proc {
	return sys32(pid, created, "svchost.exe", pidServices)
}

// con suma procesos al esqueleto.
func con(extra ...Proc) []Proc { return append(windows(), extra...) }

// C2: en un Windows de verdad, todos los procesos de sistema son genuinos.
func TestC2EsqueletoGenuino(t *testing.T) {
	f := NewForest(con(svchostReal(800, 10), sys32(801, 11, "LsaIso.exe", pidWininit),
		proc(802, 1, "MemCompression", 4), proc(803, 1, "Secure System", 4),
		Proc{ID: ID{PID: 804, Created: at(12)}, Name: "svchost.exe", Parent: pidServices, Path: `c:\windows\SysWOW64\SVCHOST.EXE`},
		sys32(805, 20, "smss.exe", pidSmss), // el smss de una sesión nueva: hijo del maestro
		sys32(806, 21, "csrss.exe", 805),    // …y su csrss, mientras ese smss sigue vivo
	))
	for _, p := range f.All() {
		if _, de := systemImageOf(p); !de {
			continue
		}
		if s, why := StandingOf(p, f, winRoot); s != StandingGenuine {
			t.Errorf("%s (PID %d) = %v (%s), esperaba genuino", p.Name, p.PID, s, why)
		}
		if !IsWindowsProc(p, f, nil, Config{SystemRoot: winRoot}) {
			t.Errorf("%s (PID %d) es de Windows: se oculta en el listado", p.Name, p.PID)
		}
	}
	if s, _ := StandingOf(proc(900, 5, "node.exe", 500), f, winRoot); s != StandingOrdinary {
		t.Errorf("node.exe = %v: su nombre no es de Windows", s)
	}
}

// C2: cada forma de llevar el nombre sin serlo.
func TestC2Impostores(t *testing.T) {
	appdata := `C:\Users\u\AppData\Roaming\`
	casos := []struct {
		nombre string
		p      Proc
		quiere Standing
		dice   string // lo que el motivo tiene que nombrar
	}{
		{"svchost en AppData", Proc{ID: ID{PID: 900, Created: at(10)}, Name: "svchost.exe", Parent: pidServices, Path: appdata + "svchost.exe"},
			StandingImpostor, appdata + "svchost.exe"},
		{"svchost en una subcarpeta de System32", Proc{ID: ID{PID: 900, Created: at(10)}, Name: "svchost.exe", Parent: pidServices, Path: winRoot + `\System32\spool\drivers\color\svchost.exe`},
			StandingImpostor, `spool\drivers\color`},
		{"lsass en SysWOW64", Proc{ID: ID{PID: 900, Created: at(10)}, Name: "lsass.exe", Parent: pidWininit, Path: winRoot + `\SysWOW64\lsass.exe`},
			StandingImpostor, "SysWOW64"},
		{"svchost de System32 lanzado por explorer (vaciado)", sys32(900, 10, "svchost.exe", 500),
			StandingImpostor, "lo lanzó explorer.exe (PID 500)"},
		{"svchost de System32 sin padre vivo", sys32(900, 10, "svchost.exe", 7777),
			StandingImpostor, "no tiene padre vivo"},
		{"lsass de System32 lanzado por services", sys32(900, 10, "lsass.exe", pidServices),
			StandingImpostor, "al de Windows lo lanza wininit.exe"},
		{"csrss de System32 lanzado por explorer", sys32(900, 10, "csrss.exe", 500),
			StandingImpostor, "lo lanzó explorer.exe"},
		{"smss que no creó el kernel", sys32(900, 10, "smss.exe", 500),
			StandingImpostor, "no lo creó el kernel"},
		{"un programa que se llama Registry", Proc{ID: ID{PID: 900, Created: at(10)}, Name: "Registry", Parent: 4, Path: appdata + "Registry"},
			StandingImpostor, "es un programa"},
		{"Registry que no creó el kernel", proc(900, 10, "Registry", 500),
			StandingImpostor, "no lo creó el kernel"},
		{"un System que no es el PID 4", proc(900, 10, "System", 4),
			StandingImpostor, "PID 4"},
		{"svchost hijo de un services.exe trucho", sys32(900, 10, "svchost.exe", 600),
			StandingImpostor, "tampoco es el de Windows"},
		{"mayúsculas y barras al revés no engañan", Proc{ID: ID{PID: 900, Created: at(10)}, Name: "SVCHOST.EXE", Parent: pidServices, Path: `c:/windows/system32/svchost.exe`},
			StandingGenuine, ""},
		{"ruta ilegible: no desmentido, tampoco confirmado", proc(900, 10, "svchost.exe", pidServices),
			StandingUnverified, ""},
	}
	for _, c := range casos {
		f := NewForest(con(c.p, proc(500, 4, "explorer.exe", 0),
			Proc{ID: ID{PID: 600, Created: at(5)}, Name: "services.exe", Parent: 500, Path: appdata + "services.exe"}))
		s, why := StandingOf(c.p, f, winRoot)
		if s != c.quiere {
			t.Errorf("%s: %v (%s), esperaba %v", c.nombre, s, why, c.quiere)
			continue
		}
		if c.dice != "" && !strings.Contains(why, c.dice) {
			t.Errorf("%s: el motivo %q no nombra %q", c.nombre, why, c.dice)
		}
		if s == StandingImpostor && !strings.HasPrefix(why, "se llama "+c.p.Name+" pero ") {
			t.Errorf("%s: el motivo %q no arranca diciendo cómo se llama", c.nombre, why)
		}
	}
}

// C2: un impostor dueño de un puerto no queda protegido por su nombre: es un
// objetivo común, con el motivo a la vista. Y no se oculta en el listado.
func TestC2ImpostorEsObjetivo(t *testing.T) {
	for _, nombre := range []string{"svchost.exe", "lsass.exe", "csrss.exe", "services.exe", "winlogon.exe", "wininit.exe", "smss.exe", "lsaiso.exe"} {
		mal := Proc{ID: ID{PID: 900, Created: at(10)}, Name: nombre, Parent: 500, Path: `C:\Users\u\AppData\Local\Temp\` + nombre, SessionID: 1}
		f := NewForest(con(mal, proc(500, 4, "explorer.exe", 0)))
		cfg := Config{SystemRoot: winRoot}
		pl := PlanKill(ports.Of(4444), []ports.Socket{tcpListen(4444, 900, at(20))}, f, nil, cfg)
		if len(pl.Refusals) != 0 || len(pl.Targets) != 1 {
			t.Fatalf("%s en Temp: plan = %+v", nombre, pl)
		}
		tg := pl.Targets[0]
		if tg.Kind != KillProcess || tg.Root != mal.ID || len(tg.Victims) != 1 || tg.Victims[0] != mal.ID {
			t.Fatalf("%s: objetivo = %+v", nombre, tg)
		}
		if !strings.Contains(tg.Impostor, `AppData\Local\Temp`) {
			t.Fatalf("%s: el objetivo no dice por qué es un impostor: %q", nombre, tg.Impostor)
		}
		if IsWindowsProc(mal, f, nil, cfg) {
			t.Fatalf("%s en Temp se ocultaría en el listado", nombre)
		}
		// Ni siquiera en la sesión de servicios, ni registrado como servicio.
		mal.SessionID = 0
		f = NewForest(con(mal, proc(500, 4, "explorer.exe", 0)))
		if IsWindowsProc(mal, f, map[uint32][]Service{900: {{Name: "WinDefend"}}}, cfg) {
			t.Fatalf("%s en Temp, en sesión 0 y con un servicio, se ocultaría igual", nombre)
		}
	}
}

// C2: un servicio de terceros que se hace llamar svchost.exe se DETIENE como
// cualquier servicio de terceros (el administrador de servicios lo lanzó, así
// que su padre es el correcto: lo delata la ruta).
func TestC2ImpostorServicioDeTerceros(t *testing.T) {
	mal := Proc{ID: ID{PID: 900, Created: at(10)}, Name: "svchost.exe", Parent: pidServices, Path: `C:\ProgramData\upd\svchost.exe`}
	f := NewForest(con(mal))
	pl := PlanKill(ports.Of(8443), []ports.Socket{tcpListen(8443, 900, at(20))}, f,
		map[uint32][]Service{900: {{Name: "UpdaterSvc"}}}, Config{SystemRoot: winRoot})
	if len(pl.Targets) != 1 || pl.Targets[0].Kind != StopThirdSvc || pl.Targets[0].Service != "UpdaterSvc" || pl.Targets[0].Impostor == "" {
		t.Fatalf("plan = %+v", pl)
	}
}

// C2: un impostor que además es la terminal de killport, o está marcado
// crítico, queda negado por ESO; el motivo del impostor viaja igual.
func TestC2ImpostorNegadoPorOtroMotivo(t *testing.T) {
	self := ID{PID: 600, Created: at(30)}
	shell := Proc{ID: ID{PID: 500, Created: at(4)}, Name: "lsass.exe", Path: `C:\tools\lsass.exe`}
	marcado := Proc{ID: ID{PID: 700, Created: at(5)}, Name: "csrss.exe", Path: `C:\tools\csrss.exe`, Critical: true}
	f := NewForest(con(shell, marcado, Proc{ID: self, Name: "killport.exe", Parent: 500}))
	pl := PlanKill(ports.Of(3000, 3001), []ports.Socket{tcpListen(3000, 500, at(40)), tcpListen(3001, 700, at(40))}, f, nil,
		Config{Self: self, SystemRoot: winRoot})
	if len(pl.Targets) != 0 || len(pl.Refusals) != 2 {
		t.Fatalf("plan = %+v", pl)
	}
	if r := pl.Refusals[0]; !strings.Contains(r.Reason, "terminal") || r.Impostor == "" {
		t.Fatalf("la terminal = %+v", r)
	}
	if r := pl.Refusals[1]; !strings.Contains(r.Reason, "crítico") || r.Impostor == "" {
		t.Fatalf("el marcado crítico = %+v", r)
	}
}

// C2: con --tree, el impostor que cuelga de un objetivo es una víctima más; el
// svchost genuino no puede colgar de nada que se cierre, y el que no se pudo
// verificar se poda.
func TestC2TreeNoPodaImpostores(t *testing.T) {
	f := NewForest(con(
		proc(500, 4, "explorer.exe", 0),
		proc(20, 10, "node.exe", 500),
		Proc{ID: ID{PID: 21, Created: at(11)}, Name: "svchost.exe", Parent: 20, Path: `C:\proy\node_modules\.bin\svchost.exe`},
		proc(22, 12, "worker.exe", 21),           // cuelga del impostor: también entra
		proc(23, 13, "lsass.exe", 20),            // sin ruta, pero con el padre equivocado: impostor
		proc(24, 14, "svchost.exe", pidServices), // sin ruta y con el padre correcto: sin verificar
	))
	pl := PlanKill(ports.Of(3000), []ports.Socket{tcpListen(3000, 20, at(16))}, f, nil, Config{Tree: true, SystemRoot: winRoot})
	if len(pl.Targets) != 1 {
		t.Fatalf("plan = %+v", pl)
	}
	var got []uint32
	for _, v := range pl.Targets[0].Victims {
		got = append(got, v.PID)
	}
	if want := []uint32{20, 21, 23, 22}; len(got) != len(want) || got[0] != 20 || got[1] != 21 || got[2] != 23 || got[3] != 22 {
		t.Fatalf("víctimas = %v, esperaba %v (raíz primero, BFS)", got, want)
	}

	// El que no se pudo verificar es intocable: protegerlo de más no rompe nada.
	w := newWorld(f, nil, Config{SystemRoot: winRoot})
	sinVerificar, _ := f.LivePID(24)
	if !w.untouchable(sinVerificar) {
		t.Fatal("un svchost sin verificar se protege")
	}
	if IsWindowsProc(sinVerificar, f, nil, Config{SystemRoot: winRoot}) {
		t.Fatal("…pero no se oculta")
	}
}

// C2, propiedad: sobre bosques al azar donde se siembran procesos con nombres
// de Windows en lugares y con padres al azar, (1) lo que el plan protege por
// nombre es siempre genuino o no desmentido, (2) ningún impostor se oculta, y
// (3) todo proceso genuino dueño de un puerto queda negado.
func TestC2Propiedad(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 22))
	nombres := []string{"svchost.exe", "lsass.exe", "csrss.exe", "services.exe", "winlogon.exe", "wininit.exe", "smss.exe", "Registry", "System"}
	carpetas := []string{winRoot + `\System32\`, winRoot + `\SysWOW64\`, `C:\Users\u\AppData\`, winRoot + `\System32\Tasks\`, winRoot + `\`, ""}
	for iter := 0; iter < 600; iter++ {
		procs := con(proc(500, 4, "explorer.exe", 0))
		padres := []uint32{4, pidServices, pidWininit, pidSmss, 500, 9999}
		n := 3 + rng.IntN(12)
		for i := 0; i < n; i++ {
			name := nombres[rng.IntN(len(nombres))]
			p := proc(uint32(1000+i), 100+i, name, padres[rng.IntN(len(padres))])
			if dir := carpetas[rng.IntN(len(carpetas))]; dir != "" {
				p.Path = dir + name
			}
			procs = append(procs, p)
		}
		f := NewForest(procs)
		cfg := Config{SystemRoot: winRoot, Tree: rng.IntN(2) == 0}
		var socks []ports.Socket
		for i, p := range procs {
			socks = append(socks, tcpListen(uint16(2000+i), p.PID, at(5000)))
		}
		pl := PlanKill(ports.NewSet(ports.R(1, 65535)), socks, f, nil, cfg)

		victima := map[ID]bool{}
		for _, tg := range pl.Targets {
			for _, v := range tg.Victims {
				victima[v] = true
			}
		}
		for _, p := range f.All() {
			s, why := StandingOf(p, f, winRoot)
			switch s {
			case StandingImpostor:
				if why == "" {
					t.Fatalf("iter %d: %s (PID %d) es impostor sin motivo", iter, p.Name, p.PID)
				}
				if IsWindowsProc(p, f, nil, cfg) {
					t.Fatalf("iter %d: el impostor %s (PID %d, %q) se oculta", iter, p.Name, p.PID, p.Path)
				}
				if p.PID > 4 && !victima[p.ID] {
					t.Fatalf("iter %d: el impostor %s (PID %d, %q, padre %d) quedó protegido: %s", iter, p.Name, p.PID, p.Path, p.Parent, why)
				}
			case StandingGenuine, StandingUnverified:
				if victima[p.ID] {
					t.Fatalf("iter %d: %s (PID %d, %q, padre %d) es %v y quedó de víctima", iter, p.Name, p.PID, p.Path, p.Parent, s)
				}
			}
		}
	}
}

// C2: el juicio no se cuelga con una instantánea que trae un ciclo de padres
// (dos procesos con la misma hora que se declaran padre uno del otro).
func TestC2CicloDePadres(t *testing.T) {
	a := sys32(900, 10, "smss.exe", 901)
	b := sys32(901, 10, "smss.exe", 900)
	f := NewForest([]Proc{a, b})
	if s, _ := StandingOf(a, f, winRoot); s != StandingImpostor {
		t.Fatalf("un smss que no desciende del kernel = %v", s)
	}
}
