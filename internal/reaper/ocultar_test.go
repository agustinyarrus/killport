package reaper

import (
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
)

// S-02 (pendiente 2 de la revisión): la carpeta de Windows sola no alcanza
// para esconder un proceso en el listado. Casos de la revisión, hechos en la
// máquina real: (A) un escuchador copiado a C:\Windows\Temp, (B) Windows
// PowerShell 5.1 con un TcpListener, (C) un HttpListener de PowerShell 5.1
// (la fila de System que se desenmascara nombra a powershell.exe).
func TestIsWindowsProcNoEscondeLoDelUsuario(t *testing.T) {
	const ps = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	en := func(pid uint32, name, path string, session uint32) Proc {
		return Proc{ID: ID{PID: pid, Created: at(100)}, Name: name, Path: path, Parent: 1, SessionID: session}
	}
	cfg := Config{SystemRoot: winRoot}
	for _, c := range []struct {
		p      Proc
		oculto bool
	}{
		// A: en una carpeta de Windows donde escribe cualquiera, en cualquier sesión.
		{en(900, "kp-rev-s.exe", `C:\Windows\Temp\kp-rev-s\kp-rev-s.exe`, 1), false},
		{en(901, "kp-rev-s.exe", `C:\Windows\Temp\kp-rev-s\kp-rev-s.exe`, 0), false},
		{en(902, "x.exe", `C:\Windows\System32\spool\drivers\color\x.exe`, 0), false},
		{en(903, "x.exe", `C:\Windows\System32\Tasks\x.exe`, 0), false},
		{en(904, "x.exe", `C:\Windows\tracing\x.exe`, 0), false},
		// B y C: un host de scripts de Windows corre código del usuario.
		{en(910, "powershell.exe", ps, 1), false},
		{en(911, "powershell.exe", ps, 0), false},
		{en(912, "mshta.exe", `C:\Windows\System32\mshta.exe`, 1), false},
		// En la sesión del usuario, la ruta sola no esconde.
		{en(920, "SearchHost.exe", `C:\Windows\SystemApps\X\SearchHost.exe`, 1), false},
		// Una imagen de Windows en la sesión 0 (un servicio del sistema) sí.
		{en(930, "spoolsv.exe", `C:\Windows\System32\spoolsv.exe`, 0), true},
		{en(931, "NVDisplay.Container.exe", `C:\Windows\System32\DriverStore\FileRepository\nv\Display.NvContainer\NVDisplay.Container.exe`, 0), true},
	} {
		f := NewForest(con(c.p))
		if got := IsWindowsProc(c.p, f, nil, cfg); got != c.oculto {
			t.Errorf("%s (%s, sesión %d): oculto=%v, esperaba %v", c.p.Name, c.p.Path, c.p.SessionID, got, c.oculto)
		}
	}
}

// Un huérfano que corre desde C:\Windows\Temp sí se ofrece (con --system: no
// parece de desarrollo); uno de Windows de verdad, no.
func TestOrphanDesdeCarpetaEscribible(t *testing.T) {
	self := ID{PID: 700, Created: at(5)}
	sospechoso := Proc{ID: ID{PID: 950, Created: at(400)}, Name: "x.exe", Parent: 900, Path: `C:\Windows\Temp\x.exe`, SessionID: 1}
	propio := Proc{ID: ID{PID: 951, Created: at(400)}, Name: "algo.exe", Parent: 901, Path: `C:\Windows\System32\algo.exe`, SessionID: 1}
	f := NewForest(con(Proc{ID: self, Name: "killport.exe", SessionID: 1}, sospechoso, propio))
	cfg := Config{Self: self, SystemRoot: winRoot}
	if _, ok := OrphanOf(f, sospechoso, nil, cfg); !ok {
		t.Error("el de C:\\Windows\\Temp sin padre es un huérfano como cualquier otro")
	}
	if _, ok := OrphanOf(f, propio, nil, cfg); ok {
		t.Error("una imagen de System32 no se ofrece como huérfano")
	}
}

// S-07: un .exe de terceros que aloja dos servicios. Se detiene el que tiene
// el puerto (el que informa la pila por socket), no el primero por orden
// alfabético; si no se sabe cuál es, no se detiene ninguno y se nombran.
func TestServicioCompartidoDeTerceros(t *testing.T) {
	host := Proc{ID: ID{PID: 900, Created: at(1)}, Name: "vendorhost.exe", Parent: pidServices, Path: `C:\Program Files\Vendor\vendorhost.exe`}
	f := NewForest(con(host))
	svc := map[uint32][]Service{900: {{Name: "AlphaSync"}, {Name: "ZetaWeb"}}}
	cfg := Config{Self: kpSelf(), SystemRoot: winRoot}
	s := tcpListen(8443, 900, at(10))
	s.Service = "ZetaWeb"
	pl := PlanKill(ports.Of(8443), []ports.Socket{s}, f, svc, cfg)
	if len(pl.Targets) != 1 || pl.Targets[0].Kind != StopThirdSvc || pl.Targets[0].Service != "ZetaWeb" || !strings.Contains(pl.Targets[0].Reason, "aloja también AlphaSync") {
		t.Fatalf("plan = %+v", pl.Targets)
	}
	s.Service = "" // la pila no dijo de quién es
	pl = PlanKill(ports.Of(8443), []ports.Socket{s}, f, svc, cfg)
	if len(pl.Targets) != 0 || len(pl.Refusals) != 1 || !strings.Contains(pl.Refusals[0].Reason, "AlphaSync, ZetaWeb") {
		t.Fatalf("sin saber cuál: %+v / %+v", pl.Targets, pl.Refusals)
	}
	// Con un solo servicio, como siempre.
	pl = PlanKill(ports.Of(8443), []ports.Socket{s}, f, map[uint32][]Service{900: {{Name: "AlphaSync"}}}, cfg)
	if len(pl.Targets) != 1 || pl.Targets[0].Service != "AlphaSync" {
		t.Fatalf("un servicio: %+v", pl.Targets)
	}
}
