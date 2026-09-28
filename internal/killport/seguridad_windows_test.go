//go:build windows

package killport

// Corrección y seguridad: lo que no se puede dar por sabido (servicios sin
// leer, PID reciclados, herederos de otra sesión, la memoria de un proceso de
// Windows, la raíz como proyecto). Cada una falla con el código de antes de su
// arreglo. Las que
// tocan el sistema de verdad solo LEEN (tablas, procesos, servicios).

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

// C-04: si no se puede leer la lista de servicios, no se sigue como si no
// hubiera ninguno (un postgres se terminaría como proceso y el administrador
// de servicios lo relanzaría).
func TestRecolectarSinServiciosFalla(t *testing.T) {
	sys := &sistema{listaServicios: func() ([]win.Service, error) { return nil, errors.New("la lista no dejó de crecer") }}
	_, _, _, _, err := recolectar(context.Background(), sys, opciones{})
	if err == nil || !strings.Contains(err.Error(), "servicios") {
		t.Fatalf("err = %v", err)
	}
}

// C-05: --timeout 0 no es un plazo (el motor lo tomaba como "no dado" y
// esperaba 3 s). Se rechaza y se sugiere --force.
func TestTimeoutCeroSeRechaza(t *testing.T) {
	for _, v := range []string{"0", "0s", "0ms"} {
		salida, code := capturar(t, func(term *tui.Term) int {
			return Main(term, "test", []string{"3000", "--timeout", v, "--list"})
		})
		if code != 2 || !strings.Contains(salida, "--force") {
			t.Fatalf("--timeout %s: código %d\n%s", v, code, salida)
		}
	}
	if d, err := plazoProlijo("500ms"); err != nil || d != 500*time.Millisecond {
		t.Fatalf("500ms = %v, %v", d, err)
	}
}

// sockDesde es un socket creado en un instante dado (para los fantasmas: un
// dueño que nació después de su socket no pudo crearlo).
func sockDesde(addr string, port uint16, pid uint32, desde time.Time) ports.Socket {
	s := sock(ports.TCP, addr, port, ports.Listen, pid, "")
	s.Since = desde
	return s
}

// C-06: el listado resuelve el dueño con la misma regla que la acción. El 3000
// es del PID 900, que murió; el 900 lo reusó chrome (nacido después del
// socket) y el socket lo sostiene node, que lo heredó. La fila es de node (con
// el PID muerto al lado), no de chrome. Y si el que reusó el PID es un
// svchost genuino, la fila no se oculta como "de Windows".
func TestListadoPIDReciclado(t *testing.T) {
	f := reaper.NewForest(conWindows(
		reaper.Proc{ID: reaper.ID{PID: 900, Created: t0.Add(5 * time.Second)}, Name: "chrome.exe", Parent: 1, Path: `C:\Program Files\Google\Chrome\chrome.exe`, SessionID: 1},
		reaper.Proc{ID: reaper.ID{PID: 950, Created: t0.Add(400 * time.Millisecond)}, Name: "node.exe", Parent: 900, Path: `C:\dev\node.exe`, SessionID: 1},
	))
	socks := []ports.Socket{sockDesde("127.0.0.1", 3000, 900, t0.Add(300*time.Millisecond))}
	filas := filasDeListado(socks, f, nil, cfgC2)
	if len(filas) != 1 {
		t.Fatalf("filas = %+v", filas)
	}
	fl := filas[0]
	if fl.Name != "node.exe" || fl.PID != 950 || fl.Owner.PID != 950 || fl.Heredado != 900 || fl.EsWindows {
		t.Fatalf("fila = %+v", fl)
	}
	if n := nombreFila(fl); !strings.Contains(n, "heredado del PID 900") {
		t.Fatalf("PROCESO = %q", n)
	}
	doc := construirJSONListado(filas, sinMostrar{})
	if doc.Escuchando[0].PID != 950 || doc.Escuchando[0].HeredadoDe != 900 {
		t.Fatalf("JSON = %+v", doc.Escuchando[0])
	}

	// El PID muerto lo reusó un svchost de verdad y nadie heredó el socket.
	sv := svchostGenuino(900)
	sv.ID.Created = t0.Add(5 * time.Second)
	f = reaper.NewForest(conWindows(sv))
	filas = filasDeListado(socks, f, nil, cfgC2)
	if len(filas) != 1 || filas[0].EsWindows || filas[0].Name != "PID 900 (ya no está)" {
		t.Fatalf("fila = %+v", filas)
	}
}

// C-02 y S-08: los selectores. -n elige al heredero de un socket cuyo dueño
// murió (no tiene filas propias: el socket figura a nombre del muerto), y se
// quedan en la sesión de killport salvo --system.
func TestElegirHerederoYSesion(t *testing.T) {
	self := reaper.ID{PID: 700, Created: t0.Add(-time.Hour)}
	f := reaper.NewForest(conWindows(
		reaper.Proc{ID: reaper.ID{PID: 500, Created: t0.Add(-2 * time.Hour)}, Name: "pwsh.exe", SessionID: 1},
		reaper.Proc{ID: self, Name: "killport.exe", Parent: 500, SessionID: 1},
		// El recargador de uvicorn con el padre (900) muerto: sostiene el 8000.
		reaper.Proc{ID: reaper.ID{PID: 950, Created: t0.Add(400 * time.Millisecond)}, Name: "python.exe", Parent: 900, Path: `C:\Python312\python.exe`, SessionID: 1},
		// Un node de otra sesión (otro usuario, o un servicio) y uno de la tuya.
		reaper.Proc{ID: reaper.ID{PID: 960, Created: t0.Add(-time.Hour)}, Name: "node.exe", Parent: 1, Path: `C:\nodejs\node.exe`, SessionID: 0},
		reaper.Proc{ID: reaper.ID{PID: 970, Created: t0.Add(-time.Hour)}, Name: "node.exe", Parent: 500, Path: `C:\nodejs\node.exe`, SessionID: 1},
	))
	cfg := reaper.Config{Self: self, SystemRoot: raizWindows}
	socks := []ports.Socket{
		sockDesde("127.0.0.1", 8000, 900, t0.Add(300*time.Millisecond)),
		sock(ports.TCP, "0.0.0.0", 3000, ports.Listen, 960, ""),
		sock(ports.TCP, "0.0.0.0", 3001, ports.Listen, 970, ""),
	}
	picked, ajenos := elegir(&sistema{}, opciones{sel: seleccion{nombres: []string{"python"}}}, f, socks, nil, cfg)
	if got := puertosElegidos(socks, ports.Filter{}, picked); !got.Equal(ports.Of(8000)) || ajenos != 0 {
		t.Fatalf("-n python eligió %v (ajenos %d)", got, ajenos)
	}
	plan := reaper.Focus(reaper.PlanKill(ports.Of(8000), socks[:1], f, nil, cfg), ports.Whole(ports.Of(8000)), picked)
	if len(plan.Targets) != 1 || plan.Targets[0].Root.PID != 950 || plan.Targets[0].Kind != reaper.KillPhantom {
		t.Fatalf("plan = %+v", plan.Targets)
	}

	picked, ajenos = elegir(&sistema{}, opciones{sel: seleccion{nombres: []string{"node"}}}, f, socks, nil, cfg)
	if got := puertosElegidos(socks, ports.Filter{}, picked); !got.Equal(ports.Of(3001)) || ajenos != 1 {
		t.Fatalf("-n node eligió %v (ajenos %d): el de la sesión 0 no es tuyo", got, ajenos)
	}
	picked, _ = elegir(&sistema{}, opciones{system: true, sel: seleccion{nombres: []string{"node"}}}, f, socks, nil, cfg)
	if got := puertosElegidos(socks, ports.Filter{}, picked); !got.Equal(ports.Of(3000, 3001)) {
		t.Fatalf("-n node --system eligió %v", got)
	}
	// Si lo único que cumple está en otra sesión, se dice (y cómo incluirlo).
	salida, code := capturar(t, func(term *tui.Term) int {
		return nadaElegido(term, opciones{sel: seleccion{nombres: []string{"node"}}}, 1)
	})
	if code != 0 || !strings.Contains(salida, "otras sesiones") || !strings.Contains(salida, "--system") {
		t.Fatalf("código %d\n%s", code, salida)
	}
}

// C-09: un puerto que un supervisor está relanzando no se da por libre, y se
// dice así (el relanzado todavía no escucha: "lo retomó" sería mentir).
func TestPuertoRelanzandose(t *testing.T) {
	p := reaper.PortResult{Port: 47351, Status: reaper.PortRespawned, Starting: true,
		Owner: reaper.ID{PID: 101}, OwnerName: "node.exe", Supervisor: reaper.ID{PID: 10}, SupervisorName: "node.exe"}
	if k, txt := textoPuertoDe(p, "puerto 47351"); k != tui.Warn || txt != "puerto 47351 lo están relanzando" {
		t.Fatalf("texto = %v %q", k, txt)
	}
	doc := construirJSONReporte(reaper.Report{Ports: []reaper.PortResult{p}}, reaper.Plan{}, false)
	if jp := doc.Puertos[0]; jp.Estado != "respawn" || !jp.Arrancando || jp.Supervisor == nil || jp.Supervisor.PID != 10 {
		t.Fatalf("JSON = %+v", jp)
	}
}

// S-10: la carpeta de trabajo (PROCESS_VM_READ + leer el PEB) solo se pide
// para lo que puede ser del usuario: nunca para lsass, services, svchost…
// (imágenes de Windows), un proceso crítico o uno cuya ruta no se pudo leer.
func TestConCarpeta(t *testing.T) {
	quiere := conCarpeta(`C:\Windows`)
	for _, c := range []struct {
		path    string
		critico bool
		leer    bool
	}{
		{`C:\Windows\System32\lsass.exe`, false, false},
		{`C:\Windows\System32\services.exe`, false, false},
		{`C:\Windows\System32\svchost.exe`, false, false},
		{`C:\Windows\System32\wininit.exe`, true, false},
		{`C:\Program Files\nodejs\node.exe`, true, false},
		{``, false, false},
		{`C:\Program Files\nodejs\node.exe`, false, true},
		{`C:\Windows\Temp\x\server.exe`, false, true},
		{`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, false, true},
	} {
		if got := quiere(c.path, c.critico); got != c.leer {
			t.Errorf("conCarpeta(%q, crítico %v) = %v, esperaba %v", c.path, c.critico, got, c.leer)
		}
	}
}

// S-10 contra el sistema real: el detalle de un programa de System32 (un ping,
// lanzado sin ventana desde una carpeta conocida) sale SIN carpeta —no se le
// pidió VM_READ ni se leyó su memoria—; el de un programa del usuario (esta
// prueba), con carpeta.
func TestDetailNoLeeMemoriaDeWindows(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "ping.exe"), "-n", "30", "127.0.0.1")
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	var id reaper.ID
	procs, err := win.AllProcesses()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.PID == uint32(cmd.Process.Pid) {
			id = reaper.ID{PID: p.PID, Created: p.Created}
		}
	}
	sys := &sistema{}
	d, err := sys.Detail(id)
	if err != nil || d.Cwd != "" || !strings.Contains(d.CommandLine, "ping") {
		t.Fatalf("ping de System32: %+v, %v", d, err)
	}
	yo := uint32(os.Getpid())
	for _, p := range procs {
		if p.PID == yo {
			id = reaper.ID{PID: p.PID, Created: p.Created}
		}
	}
	wd, _ := os.Getwd()
	if d, err := sys.Detail(id); err != nil || !strings.EqualFold(filepath.Clean(d.Cwd), filepath.Clean(wd)) {
		t.Fatalf("esta prueba: carpeta %q (esperaba %q), %v", d.Cwd, wd, err)
	}
}

// S-01: `--project` con la raíz de una unidad o el perfil sale con código 2
// (antes elegía media máquina y con -y la cerraba sin preguntar).
func TestProjectRaizSeRechaza(t *testing.T) {
	for _, dir := range []string{os.Getenv("SystemDrive") + `\`, perfil()} {
		salida, code := capturar(t, func(term *tui.Term) int {
			return Main(term, "test", []string{"--project", dir, "--list"})
		})
		if code != 2 || !strings.Contains(salida, "no es un proyecto") {
			t.Fatalf("--project %s: código %d\n%s", dir, code, salida)
		}
	}
}
