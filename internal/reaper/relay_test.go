package reaper

import (
	"math/rand/v2"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
)

// C1 — regresión: un intermediario (Docker Desktop, Podman, WSL, VS Code)
// jamás se planea como víctima. Antes de esta corrección, com.docker.backend
// (que vive en Program Files y no es un servicio) caía en "proceso común" y
// terminaba en TerminateProcess: se caía Docker Desktop entero y el contenedor
// seguía definido.
func TestC1RelaysSonNegativa(t *testing.T) {
	for _, tc := range []struct {
		imagen, ruta string
		clase        Relay
	}{
		{"com.docker.backend.exe", `C:\Program Files\Docker\Docker\resources\com.docker.backend.exe`, RelayContainer},
		{"com.docker.proxy.exe", `C:\Program Files\Docker\Docker\resources\com.docker.proxy.exe`, RelayContainer},
		{"vpnkit.exe", `C:\Program Files\Docker\Docker\resources\vpnkit.exe`, RelayContainer},
		{"gvproxy.exe", `C:\Program Files\RedHat\Podman\gvproxy.exe`, RelayContainer},
		{"win-sshproxy.exe", `C:\Program Files\RedHat\Podman\win-sshproxy.exe`, RelayContainer},
		{"wslrelay.exe", `C:\Program Files\WSL\wslrelay.exe`, RelayWSL},
		{"wslhost.exe", `C:\Program Files\WSL\wslhost.exe`, RelayWSL},
		{"Code.exe", `C:\Users\u\AppData\Local\Programs\Microsoft VS Code\Code.exe`, RelayEditor},
		{"COM.DOCKER.BACKEND.EXE", `D:\Docker\resources\com.docker.backend.exe`, RelayContainer}, // otra instalación, otras mayúsculas
	} {
		t.Run(tc.imagen, func(t *testing.T) {
			relay := Proc{ID: ID{PID: 300, Created: at(1)}, Name: tc.imagen, Path: tc.ruta, Parent: 1, SessionID: 1}
			f := NewForest([]Proc{relay})
			socks := []ports.Socket{tcpListen(5432, 300, at(5))}
			// Con y sin un servicio registrado en ese PID: la regla del relay
			// va antes que la del servicio de terceros (que lo "detendría").
			for _, svc := range []map[uint32][]Service{nil, {300: {{Name: "com.docker.service"}}}} {
				for _, tree := range []bool{false, true} {
					pl := PlanKill(ports.Of(5432), socks, f, svc, Config{SystemRoot: `C:\Windows`, Tree: tree})
					if len(pl.Targets) != 0 {
						t.Fatalf("un intermediario nunca es objetivo: %+v", pl.Targets)
					}
					if len(pl.Refusals) != 1 || pl.Refusals[0].Relay != tc.clase {
						t.Fatalf("esperaba una negativa de clase %v: %+v", tc.clase, pl.Refusals)
					}
					if r := pl.Refusals[0]; r.Reason == "" || r.Advice == "" {
						t.Fatalf("la negativa tiene que explicar y aconsejar: %+v", r)
					}
				}
			}
			if IsWindowsProc(relay, f, nil, Config{SystemRoot: `C:\Windows`}) {
				t.Fatal("un relay tiene servidores del usuario: no se oculta en el listado")
			}
		})
	}
}

// C1: el WSL de la caja trae wslhost en System32; igual se muestra (no es
// "cosa de Windows": reenvía los servidores del usuario).
func TestC1RelayEnSystem32NoSeOculta(t *testing.T) {
	p := Proc{ID: ID{PID: 70, Created: at(1)}, Name: "wslhost.exe", Path: `C:\Windows\System32\wslhost.exe`, SessionID: 1}
	if IsWindowsProc(p, NewForest([]Proc{p}), nil, Config{SystemRoot: `C:\Windows`}) {
		t.Fatal("wslhost en System32 no debería ocultarse")
	}
}

// Poda del subárbol con --tree: lo que la planificación niega como dueño no
// vuelve a entrar arrastrado por el subárbol de otro objetivo. node (20)
// escucha el 3000 y cuelga de él: un worker (21), Docker Desktop (22 → 23 →
// 25) y un proceso MARCADO crítico (26), que no se toca se llame como se
// llame. (La terminal de
// killport no puede colgar de un objetivo: si colgara, el objetivo sería su
// ancestro y ya estaría negado; la poda la cubre igual, por las dudas.)
func TestTreePodaIntocables(t *testing.T) {
	self := ID{PID: 600, Created: at(40)}
	f := NewForest(con(
		proc(1, 0, "explorer.exe", 0),
		proc(20, 10, "node.exe", 1),
		proc(21, 11, "worker.exe", 20),
		proc(22, 12, "com.docker.backend.exe", 20),
		proc(23, 13, "vpnkit.exe", 22),
		proc(24, 14, "pwsh.exe", 1),
		Proc{ID: self, Name: "killport.exe", Parent: 24},
		proc(25, 15, "helper.exe", 23), // cuelga de un intocable: también se poda
		Proc{ID: ID{PID: 26, Created: at(16)}, Name: "svchost.exe", Parent: 20, Critical: true}, // marcado crítico
	))
	pl := PlanKill(ports.Of(3000), []ports.Socket{tcpListen(3000, 20, at(16))}, f, nil, Config{Self: self, Tree: true, SystemRoot: `C:\Windows`})
	if len(pl.Targets) != 1 {
		t.Fatalf("plan = %+v", pl)
	}
	got := pl.Targets[0].Victims
	if len(got) != 2 || got[0].PID != 20 || got[1].PID != 21 {
		t.Fatalf("víctimas = %v, esperaba [20 21]: ni Docker Desktop ni la terminal de killport", got)
	}
}

// X-07: el anfitrión de la consola (conhost de System32, OpenConsole de
// Windows Terminal) cuelga del primer proceso de la consola, pero no es un
// hijo del programa: con --tree no es víctima. Uno que solo se LLAMA conhost
// y vive en otra carpeta sí lo es (no se protege un nombre).
func TestTreeSinAnfitrionDeConsola(t *testing.T) {
	f := NewForest(con(
		proc(20, 10, "node.exe", 1),
		proc(21, 11, "node.exe", 20),
		Proc{ID: ID{PID: 22, Created: at(11)}, Name: "conhost.exe", Parent: 20, Path: `C:\Windows\System32\conhost.exe`},
		Proc{ID: ID{PID: 23, Created: at(12)}, Name: "OpenConsole.exe", Parent: 20, Path: `C:\Program Files\WindowsApps\Microsoft.WindowsTerminal\OpenConsole.exe`},
		Proc{ID: ID{PID: 24, Created: at(13)}, Name: "conhost.exe", Parent: 20, Path: `C:\Users\u\AppData\Local\Temp\conhost.exe`},
	))
	pl := PlanKill(ports.Of(3000), []ports.Socket{tcpListen(3000, 20, at(16))}, f, nil, Config{Tree: true, SystemRoot: `C:\Windows`})
	var got []uint32
	for _, v := range pl.Targets[0].Victims {
		got = append(got, v.PID)
	}
	if len(got) != 3 || got[0] != 20 || got[1] != 21 || got[2] != 24 {
		t.Fatalf("víctimas = %v, esperaba [20 21 24]", got)
	}
}

// Propiedad C1: sobre bosques al azar con intermediarios sembrados como
// dueños y como descendientes de otros dueños, con y sin --tree, ningún
// intocable termina jamás en la lista de víctimas.
func TestC1PropiedadIntocablesNuncaVictimas(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 1))
	relays := []string{"com.docker.backend.exe", "vpnkit.exe", "gvproxy.exe", "win-sshproxy.exe", "wslrelay.exe", "wslhost.exe", "Code.exe", "com.docker.proxy.exe"}
	for iter := 0; iter < 500; iter++ {
		self := ID{PID: 600, Created: at(10)}
		procs := con(
			proc(500, 0, "pwsh.exe", 0),
			Proc{ID: self, Name: "killport.exe", Parent: 500},
		)
		pids := rng.Perm(3000)
		n := 5 + rng.IntN(30)
		for i := 0; i < n; i++ {
			pid := uint32(pids[i]) + 1000
			parent := procs[rng.IntN(len(procs))].PID
			p := proc(pid, 100+i, "app.exe", parent) // Created creciente: aristas válidas
			switch rng.IntN(5) {
			case 0:
				p.Name = relays[rng.IntN(len(relays))]
			case 1:
				p = svchostReal(pid, 100+i)
			}
			procs = append(procs, p)
		}
		f := NewForest(procs)
		var socks []ports.Socket
		port := uint16(2000)
		for _, p := range procs {
			if rng.IntN(2) == 0 {
				socks = append(socks, tcpListen(port, p.PID, at(5000)))
				port++
			}
		}
		cfg := Config{Self: self, Tree: rng.IntN(2) == 0, SystemRoot: `C:\Windows`}
		pl := PlanKill(ports.NewSet(ports.R(1, 65535)), socks, f, nil, cfg)
		w := newWorld(f, nil, cfg)
		for _, tg := range pl.Targets {
			for _, v := range tg.Victims {
				p, _ := f.Lookup(v)
				if w.untouchable(p) {
					t.Fatalf("iter %d: %s (PID %d) quedó como víctima (tree=%v)", iter, p.Name, p.PID, cfg.Tree)
				}
			}
		}
		for _, r := range pl.Refusals {
			if p, ok := f.Lookup(r.Owner); ok && RelayOf(p) != RelayNone && r.Relay == RelayNone {
				t.Fatalf("iter %d: la negativa de %s no dice que es un intermediario", iter, p.Name)
			}
		}
	}
}
