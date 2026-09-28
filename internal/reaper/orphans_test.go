package reaper

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
)

// El bosque de una sesión de escritorio típica, con lo que cada caso necesita.
func bosqueHuerfanos() ([]Proc, []ports.Socket, map[uint32][]Service, Config) {
	procs := append(windows(),
		Proc{ID: ID{PID: 700, Created: at(100)}, Name: "explorer.exe", Parent: 666, Path: `C:\Windows\explorer.exe`, SessionID: 1},
		Proc{ID: ID{PID: 500, Created: at(200)}, Name: "pwsh.exe", Parent: 700, Path: `C:\Program Files\PowerShell\7\pwsh.exe`, SessionID: 1},
		Proc{ID: kpSelf(), Name: "killport.exe", Parent: 500, SessionID: 1},
		// npm → node, con npm vivo: NO es huérfano.
		Proc{ID: ID{PID: 810, Created: at(300)}, Name: "node.exe", Parent: 500, Path: `C:\Program Files\nodejs\node.exe`, SessionID: 1},
		Proc{ID: ID{PID: 811, Created: at(310)}, Name: "node.exe", Parent: 810, Path: `C:\Program Files\nodejs\node.exe`, SessionID: 1},
		// Un node cuyo padre (la terminal, PID 900) se cerró: huérfano de desarrollo.
		Proc{ID: ID{PID: 812, Created: at(320)}, Name: "node.exe", Parent: 900, Path: `C:\Program Files\nodejs\node.exe`, SessionID: 1},
		// Un python cuyo padre (PID 950) murió y el PID lo reusó chrome, que nació después.
		Proc{ID: ID{PID: 820, Created: at(330)}, Name: "python.exe", Parent: 950, Path: `C:\Python312\python.exe`, SessionID: 1},
		Proc{ID: ID{PID: 950, Created: at(400)}, Name: "chrome.exe", Parent: 700, Path: `C:\Program Files\Google\Chrome\chrome.exe`, SessionID: 1},
		// Una app de la bandeja que quedó sin padre al reiniciar el Explorador: suelta, no de desarrollo.
		Proc{ID: ID{PID: 830, Created: at(50)}, Name: "Discord.exe", Parent: 1, Path: `C:\Users\u\AppData\Local\Discord\Discord.exe`, SessionID: 1},
		// Un binario de `go run`: huérfano de desarrollo por su carpeta.
		Proc{ID: ID{PID: 840, Created: at(340)}, Name: "main.exe", Parent: 901, Path: `C:\Users\u\AppData\Local\Temp\go-build123\b001\exe\main.exe`, SessionID: 1},
		// Una tarea programada (padre svchost vivo): no es huérfana.
		Proc{ID: ID{PID: 850, Created: at(350)}, Name: "node.exe", Parent: 1100, Path: `C:\Program Files\nodejs\node.exe`, SessionID: 1},
		// Un servicio de terceros sin padre vivo en la foto: es un servicio.
		Proc{ID: ID{PID: 860, Created: at(20)}, Name: "postgres.exe", Parent: 1, Path: `C:\Program Files\PostgreSQL\bin\postgres.exe`, SessionID: 0},
		// Otro usuario (otra sesión): no.
		Proc{ID: ID{PID: 870, Created: at(360)}, Name: "node.exe", Parent: 902, Path: `C:\Program Files\nodejs\node.exe`, SessionID: 2},
		// Docker Desktop suelto: un intermediario nunca se ofrece.
		Proc{ID: ID{PID: 880, Created: at(370)}, Name: "com.docker.backend.exe", Parent: 903, Path: `C:\Program Files\Docker\Docker\resources\com.docker.backend.exe`, SessionID: 1},
		// Un programa en la carpeta de Windows sin padre (como csrss): no.
		Proc{ID: ID{PID: 890, Created: at(380)}, Name: "algo.exe", Parent: 904, Path: `C:\Windows\System32\algo.exe`, SessionID: 1},
		// Un programa cualquiera cuya carpeta es un proyecto (lo dice dev).
		Proc{ID: ID{PID: 895, Created: at(390)}, Name: "server.exe", Parent: 905, Path: `C:\dev\shop\bin\server.exe`, SessionID: 1},
	)
	procs = append(procs, Proc{ID: ID{PID: 1100, Created: at(5)}, Name: "svchost.exe", Parent: pidServices, Path: `C:\Windows\System32\svchost.exe`})
	var socks []ports.Socket
	for i, pid := range []uint32{811, 812, 820, 830, 840, 850, 860, 870, 880, 890, 895, 500} {
		socks = append(socks, tcpListen(uint16(3000+i), pid, at(1000)))
	}
	socks = append(socks, ports.Socket{Proto: ports.UDP, Local: tcpListen(5353, 830, at(1000)).Local, PID: 830, Since: at(1000)})
	svc := map[uint32][]Service{860: {{Name: "postgresql-x64"}}}
	cfg := Config{Self: kpSelf(), SystemRoot: `C:\Windows`}
	return procs, socks, svc, cfg
}

func TestOrphans(t *testing.T) {
	procs, socks, svc, cfg := bosqueHuerfanos()
	f := NewForest(procs)
	dev := func(p Proc) bool { return strings.HasPrefix(p.Path, `C:\dev\`) }
	got := map[uint32]Orphan{}
	var orden []uint32
	for _, o := range Orphans(f, socks, svc, cfg, dev) {
		got[o.PID] = o
		orden = append(orden, o.PID)
	}
	want := map[uint32]struct {
		dev bool
		why string
	}{
		812: {true, "su padre (PID 900) ya no existe"},
		820: {true, "su padre (PID 950) ya no existe: ese PID ahora es chrome.exe, que nació después"},
		830: {false, "su padre (PID 1) ya no existe"},
		840: {true, "su padre (PID 901) ya no existe"},
		895: {true, "su padre (PID 905) ya no existe"},
	}
	if len(got) != len(want) {
		t.Fatalf("huérfanos = %v, esperaba %v", orden, want)
	}
	for pid, w := range want {
		o, ok := got[pid]
		if !ok || o.Dev != w.dev || o.Why != w.why {
			t.Errorf("PID %d = %+v, esperaba dev=%v %q", pid, o, w.dev, w.why)
		}
	}
	if fmt.Sprint(orden) != "[812 820 830 840 895]" {
		t.Errorf("orden por puerto = %v", orden)
	}
	// Sin el juez de carpetas, server.exe no parece de desarrollo.
	for _, o := range Orphans(f, socks, svc, cfg, nil) {
		if o.PID == 895 && o.Dev {
			t.Error("sin dev, una imagen cualquiera no es de desarrollo")
		}
	}
}

// Por SSH, killport corre en la sesión 0, la misma que los servicios: ahí el
// filtro de sesión no los separa, y un servicio sin padre vivo en la foto
// sigue sin ser un huérfano (lo cuida el administrador de servicios).
func TestOrphansEnLaSesionDeLosServicios(t *testing.T) {
	procs := append(windows(),
		Proc{ID: ID{PID: 500, Created: at(200)}, Name: "sshd.exe", Parent: pidServices, SessionID: 0},
		Proc{ID: kpSelf(), Name: "killport.exe", Parent: 500, SessionID: 0},
		Proc{ID: ID{PID: 860, Created: at(20)}, Name: "postgres.exe", Parent: 1, Path: `C:\Program Files\PostgreSQL\bin\postgres.exe`, SessionID: 0},
		Proc{ID: ID{PID: 812, Created: at(320)}, Name: "node.exe", Parent: 900, Path: `C:\Program Files\nodejs\node.exe`, SessionID: 0},
	)
	socks := []ports.Socket{tcpListen(5432, 860, at(1000)), tcpListen(3000, 812, at(1000))}
	svc := map[uint32][]Service{860: {{Name: "postgresql-x64"}}}
	got := Orphans(NewForest(procs), socks, svc, Config{Self: kpSelf(), SystemRoot: `C:\Windows`}, nil)
	if len(got) != 1 || got[0].PID != 812 {
		t.Fatalf("huérfanos en la sesión 0 = %+v: solo el node, no el servicio", got)
	}
}

// Contra una implementación ingenua, sobre bosques al azar con PID
// reciclados: un dueño es raíz si ningún proceso con el PID de su padre nació
// antes que él.
func TestOrphansContraIngenua(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for caso := 0; caso < 2000; caso++ {
		n := 2 + rng.Intn(25)
		var procs []Proc
		used := map[uint32]bool{}
		for i := 0; i < n; i++ {
			pid := uint32(1000 + rng.Intn(60))
			if used[pid] {
				continue // en una foto real, un PID vivo es de un solo proceso
			}
			used[pid] = true
			procs = append(procs, Proc{ID: ID{PID: pid, Created: at(rng.Intn(1000))}, Name: "node.exe",
				Parent: uint32(1000 + rng.Intn(70)), SessionID: 1, Path: `C:\n\node.exe`})
		}
		procs = append(procs, Proc{ID: kpSelf(), Name: "killport.exe", Parent: 1, SessionID: 1})
		var socks []ports.Socket
		for _, p := range procs {
			if rng.Intn(2) == 0 && p.PID != kpPID {
				socks = append(socks, tcpListen(3000, p.PID, at(2000)))
			}
		}
		f := NewForest(procs)
		got := map[uint32]bool{}
		for _, o := range Orphans(f, socks, nil, Config{Self: kpSelf(), SystemRoot: `C:\Windows`}, nil) {
			got[o.PID] = true
		}
		for _, s := range socks {
			var p Proc
			for _, q := range procs {
				if q.PID == s.PID {
					p = q
				}
			}
			root := true
			for _, q := range procs {
				if q.PID == p.Parent && q.PID != p.PID && !q.Created.After(p.Created) {
					root = false
				}
			}
			if root != got[p.PID] {
				t.Fatalf("caso %d: PID %d raíz=%v pero Orphans=%v", caso, p.PID, root, got[p.PID])
			}
		}
	}
}

// C-02: el heredero de un socket fantasma se juzga como el dueño. El python
// que heredó el socket de su padre (PID 900, muerto) no tiene filas propias
// en la tabla —el socket figura a nombre del muerto—, y es justo el huérfano
// que hay que ofrecer (el recargador de uvicorn o werkzeug con el padre
// muerto). El hijo del proceso que reusó un PID muerto no es heredero de nada.
func TestOrphansHerederoDeFantasma(t *testing.T) {
	self := ID{PID: 700, Created: at(5)}
	f := NewForest(con(
		Proc{ID: ID{PID: 500, Created: at(1)}, Name: "pwsh.exe", Parent: 0, SessionID: 1},
		Proc{ID: self, Name: "killport.exe", Parent: 500, SessionID: 1},
		Proc{ID: ID{PID: 950, Created: at(400)}, Name: "python.exe", Parent: 900, Path: `C:\Python312\python.exe`, SessionID: 1},
		// El 901 murió después de abrir el 9000 y su PID lo reusó chrome (t=500),
		// que tiene un hijo node (t=600): ese node no heredó nada.
		Proc{ID: ID{PID: 901, Created: at(500)}, Name: "chrome.exe", Parent: 1, Path: `C:\Program Files\Google\Chrome\chrome.exe`, SessionID: 1},
		Proc{ID: ID{PID: 960, Created: at(600)}, Name: "node.exe", Parent: 901, Path: `C:\Program Files\nodejs\node.exe`, SessionID: 1},
	))
	socks := []ports.Socket{tcpListen(8000, 900, at(300)), tcpListen(9000, 901, at(300))}
	os := Orphans(f, socks, nil, Config{Self: self, SystemRoot: winRoot}, nil)
	if len(os) != 1 || os[0].PID != 950 || !os[0].Dev || !strings.Contains(os[0].Why, "PID 900") {
		t.Fatalf("huérfanos = %+v; esperaba solo al python 950 (heredero del 8000)", os)
	}
}
