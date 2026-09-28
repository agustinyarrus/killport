//go:build windows

package killport

// Los impostores en la herramienta: un proceso que LLEVA el nombre de uno de Windows y no
// lo es (un svchost.exe en AppData, un lsass.exe lanzado por el explorador) no
// se oculta en el listado, se muestra marcado con el motivo, y llega al bloque
// y al JSON como lo que es. La decisión es del núcleo (reaper/standing.go);
// acá se prueba que lo que se MUESTRA le hace caso.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

const raizWindows = `C:\Windows`

// Los PID del esqueleto de Windows de las pruebas.
const (
	pidSmss     = 380
	pidCsrss    = 520
	pidWininit  = 600
	pidServices = 700
	pidLsass    = 720
)

func deSystem32(pid uint32, nombre string, padre uint32) reaper.Proc {
	return reaper.Proc{ID: reaper.ID{PID: pid, Created: t0.Add(-time.Hour + time.Duration(pid)*time.Millisecond)},
		Name: nombre, Parent: padre, Path: raizWindows + `\System32\` + nombre}
}

// esqueletoWindows son los procesos de sistema de un Windows de verdad, cada
// uno en su lugar y con su linaje (el smss que lanzó a csrss y wininit ya
// salió: su PID, el 400, no está).
func esqueletoWindows() []reaper.Proc {
	return []reaper.Proc{
		{ID: reaper.ID{PID: 4, Created: t0.Add(-time.Hour)}, Name: "System"},
		deSystem32(pidSmss, "smss.exe", 4),
		deSystem32(pidCsrss, "csrss.exe", 400),
		deSystem32(pidWininit, "wininit.exe", 400),
		deSystem32(pidServices, "services.exe", pidWininit),
		deSystem32(pidLsass, "lsass.exe", pidWininit),
	}
}

// conWindows suma procesos al esqueleto.
func conWindows(extra ...reaper.Proc) []reaper.Proc { return append(esqueletoWindows(), extra...) }

// svchostGenuino es un host de servicios de verdad: en System32 e hijo de
// services.exe.
func svchostGenuino(pid uint32) reaper.Proc {
	p := deSystem32(pid, "svchost.exe", pidServices)
	p.ID.Created = t0
	return p
}

var cfgC2 = reaper.Config{SystemRoot: raizWindows}

func TestImpostorListadoNoOcultaImpostores(t *testing.T) {
	impostor := reaper.Proc{ID: reaper.ID{PID: 4100, Created: t0}, Name: "svchost.exe", Parent: 3000,
		Path: `C:\Users\ana\AppData\Roaming\svchost.exe`}
	hueco := deSystem32(4200, "svchost.exe", 3000) // la ruta perfecta y el padre equivocado: el explorador
	hueco.ID.Created = t0
	lsassFalso := reaper.Proc{ID: reaper.ID{PID: 4300, Created: t0}, Name: "lsass.exe", Parent: pidWininit,
		Path: `C:\Windows\Temp\lsass.exe`}
	f := reaper.NewForest(conWindows(
		reaper.Proc{ID: reaper.ID{PID: 3000, Created: t0.Add(-time.Minute)}, Name: "explorer.exe", Path: raizWindows + `\explorer.exe`},
		svchostGenuino(900), impostor, hueco, lsassFalso,
	))
	svc := map[uint32][]reaper.Service{900: {{Name: "RpcSs"}}}
	socks := []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 135, ports.Listen, 900, "RpcSs"),
		sock(ports.TCP, "0.0.0.0", 4444, ports.Listen, 4100, "svchost.exe"),
		sock(ports.TCP, "0.0.0.0", 4445, ports.Listen, 4200, "svchost.exe"),
		sock(ports.TCP, "0.0.0.0", 4446, ports.Listen, 4300, "lsass.exe"),
		sock(ports.TCP, "0.0.0.0", 49664, ports.Listen, pidLsass, "lsass.exe"),
	}
	filas := filasDeListado(socks, f, svc, cfgC2)
	muestra, ocultas := visibles(filas, mostrar{})

	var puertos []string
	for _, fl := range muestra {
		puertos = append(puertos, u32(uint32(fl.Port)))
		if fl.Impostor == "" || fl.EsWindows {
			t.Errorf("%d (%s, PID %d): se muestra y no está marcado: %+v", fl.Port, fl.Name, fl.PID, fl)
		}
		if fl.Servicio != "" {
			t.Errorf("%d: un impostor no es un host de servicios, y dice servicio %q", fl.Port, fl.Servicio)
		}
		if !strings.HasSuffix(nombreFila(fl), " (!)") {
			t.Errorf("%d: la columna PROCESO dice %q", fl.Port, nombreFila(fl))
		}
	}
	if got := strings.Join(puertos, " "); got != "4444 4445 4446" || ocultas.filas != 2 {
		t.Fatalf("visibles = %q, ocultas = %d: se tienen que ver los tres impostores y ocultar el svchost y el lsass de verdad", got, ocultas.filas)
	}
	for puerto, dice := range map[int]string{0: `C:\Users\ana\AppData\Roaming\svchost.exe`, 1: "explorer.exe", 2: `C:\Windows\Temp\lsass.exe`} {
		if !strings.Contains(muestra[puerto].Impostor, dice) {
			t.Errorf("%d: el motivo %q no nombra %q", muestra[puerto].Port, muestra[puerto].Impostor, dice)
		}
	}

	// El genuino sigue siendo lo que era: oculto, sin marca y con su servicio.
	for _, fl := range filas {
		if fl.Port == 135 && (fl.Impostor != "" || !fl.EsWindows || fl.Servicio != "RpcSs" || nombreFila(fl) != "svchost.exe") {
			t.Errorf("el svchost de verdad = %+v", fl)
		}
	}

	term := tui.Open()
	term.DisableColor()
	notas := strings.Join(notasImpostores(term, muestra), "\n")
	for _, dice := range []string{"svchost.exe (PID 4100): posible impostor", "svchost.exe (PID 4200): posible impostor", "lsass.exe (PID 4300): posible impostor", "AppData"} {
		if !strings.Contains(notas, dice) {
			t.Errorf("las notas no dicen %q:\n%s", dice, notas)
		}
	}
	if n := strings.Count(notas, "posible impostor"); n != 3 {
		t.Errorf("%d notas, esperaba una por impostor:\n%s", n, notas)
	}

	b, err := json.Marshal(construirJSONListado(filas, ocultas))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Escuchando []struct {
			Puerto    uint16 `json:"puerto"`
			Impostor  string `json:"impostor"`
			DeWindows bool   `json:"de_windows"`
		} `json:"escuchando"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, e := range doc.Escuchando {
		esImpostor := e.Puerto >= 4444 && e.Puerto <= 4446
		if (e.Impostor != "") != esImpostor || e.DeWindows == esImpostor {
			t.Errorf("JSON del %d: impostor %q, de_windows %v", e.Puerto, e.Impostor, e.DeWindows)
		}
	}
}

func TestImpostorBloquesYJSONDelPlan(t *testing.T) {
	impostor := reaper.Proc{ID: reaper.ID{PID: 4100, Created: t0}, Name: "svchost.exe", Parent: 3000,
		Path: `C:\Users\ana\AppData\Roaming\svchost.exe`}
	f := reaper.NewForest(conWindows(
		reaper.Proc{ID: reaper.ID{PID: 3000, Created: t0.Add(-time.Minute)}, Name: "explorer.exe", Path: raizWindows + `\explorer.exe`},
		svchostGenuino(900), impostor,
	))
	svc := map[uint32][]reaper.Service{900: {{Name: "RpcSs"}}}
	socks := []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 135, ports.Listen, 900, "RpcSs"),
		sock(ports.TCP, "0.0.0.0", 4444, ports.Listen, 4100, "svchost.exe"),
	}
	plan := reaper.PlanKill(ports.Of(135, 4444), socks, f, svc, cfgC2)
	if len(plan.Targets) != 1 || plan.Targets[0].Root.PID != 4100 || plan.Targets[0].Impostor == "" {
		t.Fatalf("el impostor es un objetivo como cualquiera: %+v", plan.Targets)
	}
	if len(plan.Refusals) != 1 || plan.Refusals[0].PID != 900 || plan.Refusals[0].Impostor != "" {
		t.Fatalf("el svchost de verdad se niega: %+v", plan.Refusals)
	}

	term := tui.Open()
	term.DisableColor()
	texto := strings.Join(bloques(term, &sistema{}, f, plan, cfgC2, detalle{}), "\n")
	if !strings.Contains(texto, "posible impostor: se llama svchost.exe pero corre desde "+impostor.Path) {
		t.Errorf("el bloque no avisa del impostor:\n%s", texto)
	}
	if strings.Count(texto, "posible impostor") != 1 {
		t.Errorf("solo el impostor lleva el aviso:\n%s", texto)
	}

	b, err := json.Marshal(construirJSONReporte(reaper.Report{}, plan, true))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Objetivos []struct {
			PID      uint32 `json:"pid"`
			Impostor string `json:"impostor"`
		} `json:"objetivos"`
		Negativas []struct {
			PID      uint32 `json:"pid"`
			Impostor string `json:"impostor"`
			Servicio string `json:"servicio"`
		} `json:"negativas"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Objetivos) != 1 || !strings.Contains(doc.Objetivos[0].Impostor, "AppData") {
		t.Errorf("objetivos = %+v", doc.Objetivos)
	}
	if len(doc.Negativas) != 1 || doc.Negativas[0].Impostor != "" || doc.Negativas[0].Servicio != "RpcSs" {
		t.Errorf("negativas = %+v", doc.Negativas)
	}
}

// La última red: un proceso MARCADO crítico no se abre, se llame como se
// llame. El adaptador lo dice con reaper.ErrCritical y el motor lo deja como
// estaba; en el JSON el destino es "critico".
func TestImpostorDestinoCritico(t *testing.T) {
	rep := reaper.Report{Victims: []reaper.VictimResult{
		{ID: reaper.ID{PID: 4100, Created: t0}, Name: "svchost.exe", Outcome: reaper.OutcomeCritical, Err: reaper.ErrCritical},
	}}
	b, err := json.Marshal(construirJSONReporte(rep, reaper.Plan{}, false))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"destino":"critico"`) {
		t.Errorf("JSON = %s", b)
	}
	if necesitaElevacion(rep, nil) {
		t.Error("a un proceso crítico no lo cierra ni un administrador: no se ofrece elevar")
	}
	// Lo que ve el usuario en la escalada (antes lo probaba textoOutcome, que
	// no se usaba: la escalada no decía nada de un crítico).
	l := lineaEvento(tui.Open(), reaper.Event{Kind: reaper.EvOpenFailed, Victim: rep.Victims[0].ID, Name: "svchost.exe",
		Err: reaper.ErrCritical, Detail: "está marcado como proceso crítico: no lo toco"}, narracion{})
	if !strings.Contains(l, "svchost.exe no se toca") || !strings.Contains(l, "crítico") {
		t.Errorf("línea del crítico = %q", l)
	}
}

// La decisión del adaptador con el proceso ya abierto: la marca de crítico
// manda sobre todo lo demás.
func TestImpostorProcesoAbierto(t *testing.T) {
	casos := []struct {
		nombre string
		a      abierto
		want   error
	}{
		{"un proceso común", abierto{seSupoCritico: true, puedeTerminar: true}, nil},
		{"marcado crítico", abierto{critico: true, seSupoCritico: true, puedeTerminar: true}, reaper.ErrCritical},
		{"marcado crítico y protegido: manda la marca", abierto{critico: true, seSupoCritico: true, protegido: true}, reaper.ErrCritical},
		{"marcado crítico, sin permiso de terminar", abierto{critico: true, seSupoCritico: true}, reaper.ErrCritical},
		{"protegido (PPL)", abierto{seSupoCritico: true, protegido: true}, reaper.ErrProtected},
		{"sin permiso de terminar, pero no protegido: se espera su salida", abierto{seSupoCritico: true}, nil},
		// Si la marca no se pudo leer, el valor que quedó no cuenta.
		{"no se pudo leer la marca", abierto{critico: true, puedeTerminar: true}, nil},
		{"no se pudo leer la marca, y es protegido", abierto{critico: true, protegido: true}, reaper.ErrProtected},
	}
	for _, c := range casos {
		if got := c.a.negativa(); got != c.want {
			t.Errorf("%s: %v, esperaba %v", c.nombre, got, c.want)
		}
	}
}
