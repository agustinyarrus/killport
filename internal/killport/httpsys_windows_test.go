//go:build windows

package killport

// Función 3, primera mitad: detrás de System (PID 4) está http.sys, y detrás
// de http.sys, el proceso que registró la URL. netsh es un doble que contesta
// con las muestras de internal/ports/testdata (las "es" son salida real).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

// muestraNetsh lee una muestra de la salida de netsh.
func muestraNetsh(t *testing.T, nombre string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "ports", "testdata", nombre))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// httpFalso arma el cliente de http.sys con un netsh que contesta eso.
func httpFalso(t *testing.T, salida string, err error, dependen ...string) (*httpsys, *int) {
	t.Helper()
	corridas := 0
	return &httpsys{
		netsh: func(_ context.Context, args ...string) (string, error) {
			corridas++
			if got := strings.Join(args, " "); got != "http show servicestate view=requestq verbose=no" {
				t.Errorf("a netsh se le pidió %q", got)
			}
			return salida, err
		},
		dependientes: func(servicio string) []string {
			if servicio != "HTTP" {
				t.Errorf("se preguntó por los dependientes de %q", servicio)
			}
			return dependen
		},
		elevado: func() bool { return false },
	}, &corridas
}

var sistemaID = reaper.ID{PID: 4, Created: t0.Add(-3600e9)}

const pwshMuestra = `C:\Program Files\WindowsApps\Microsoft.PowerShell_7.6.6.0_x64__8wekyb3d8bbwe\pwsh.exe`

// La muestra real: un pwsh (PID 15152) registró una URL en el 47180.
func TestHTTPSysEncuentraAlQueRegistro(t *testing.T) {
	pwsh := reaper.Proc{ID: reaper.ID{PID: 15152, Created: t0}, Name: "pwsh.exe", Parent: 3000, Path: pwshMuestra}
	forest := reaper.NewForest(conWindows(
		reaper.Proc{ID: reaper.ID{PID: 3000, Created: t0.Add(-60e9)}, Name: "explorer.exe", Path: raizWindows + `\explorer.exe`}, pwsh))
	h, corridas := httpFalso(t, muestraNetsh(t, "netsh_es_requestq_una.txt"), nil)
	sys := &sistema{http: h, admin: func() bool { return false }}
	socks := []ports.Socket{
		sock(ports.TCP, "::", 47180, ports.Listen, 4, "System"),
		sock(ports.TCP, "0.0.0.0", 445, ports.Listen, 4, "System"),
	}
	pedidos := ports.Of(445, 47180)
	plan := reaper.PlanKill(pedidos, socks, forest, nil, cfgC2)
	if len(plan.Targets) != 0 || len(plan.Refusals) != 2 {
		t.Fatalf("antes de mirar, todo negado: %+v", plan)
	}
	plan = desenmascarar(context.Background(), sys, plan, socks, forest, opciones{})
	if *corridas != 1 {
		t.Fatalf("netsh corrió %d veces", *corridas)
	}
	if len(plan.Targets) != 1 || len(plan.Refusals) != 1 {
		t.Fatalf("plan = %+v", plan)
	}
	tg := plan.Targets[0]
	if tg.Kind != reaper.KillProcess || tg.Root != pwsh.ID.Canon() || !tg.Ports.Equal(ports.Of(47180)) ||
		tg.Via.PID != 4 || tg.ViaName != "System" || tg.ViaClass != reaper.RelayHTTP || len(tg.Victims) != 1 {
		t.Fatalf("objetivo = %+v", tg)
	}
	// El 445 es compartir archivos: ni se pregunta por él ni cambia.
	if r := plan.Refusals[0]; !r.Ports.Equal(ports.Of(445)) || !strings.Contains(r.Reason, "SMB") || r.Relay != reaper.RelayNone {
		t.Fatalf("445 = %+v", r)
	}

	det := detallar(sys, plan, opciones{})
	term := tui.Open()
	term.DisableColor()
	texto := strings.Join(bloques(term, sys, forest, plan, cfgC2, det), "\n")
	for _, dice := range []string{
		"pwsh.exe  PID 15152",
		"registró http://localhost:47180/KP-PRUEBA/",
		"Windows lo ve como System (PID 4): es http.sys, el servidor HTTP de Windows, no se toca",
		"445 — System",
	} {
		if !strings.Contains(texto, dice) {
			t.Errorf("el bloque no dice %q:\n%s", dice, texto)
		}
	}
	if q := preguntaCierre(term, plan, false, false); !strings.Contains(q, "¿Cerrar pwsh.exe y liberar el 47180?") {
		t.Errorf("pregunta = %q", q)
	}

	doc := construirJSONReporte(reaper.Report{}, plan, true)
	det.anotar(&doc, plan)
	b, _ := json.Marshal(doc)
	var back struct {
		Objetivos []struct {
			Tipo string   `json:"tipo"`
			PID  uint32   `json:"pid"`
			URLs []string `json:"urls"`
			Via  struct {
				PID     uint32 `json:"pid"`
				Proceso string `json:"proceso"`
			} `json:"via"`
		} `json:"objetivos"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if o := back.Objetivos; len(o) != 1 || o[0].Tipo != "proceso" || o[0].PID != 15152 || o[0].Via.PID != 4 || o[0].Via.Proceso != "System" ||
		len(o[0].URLs) != 1 || o[0].URLs[0] != "http://localhost:47180/KP-PRUEBA/" {
		t.Errorf("JSON = %s", b)
	}
}

// Quién MANDA en una cola: el que la creó. En IIS la cola es del servicio WAS
// (un svchost) y los w3wp.exe solo atienden: cerrar un w3wp no libera nada
// (WAS lo relanza). El objetivo es el dueño, y un svchost se niega.
func TestHTTPSysElDuenoEsElControlador(t *testing.T) {
	was := svchostGenuino(1480)
	w3wp1 := reaper.Proc{ID: reaper.ID{PID: 3844, Created: t0}, Name: "w3wp.exe", Parent: 1480, Path: raizWindows + `\System32\inetsrv\w3wp.exe`}
	w3wp2 := reaper.Proc{ID: reaper.ID{PID: 5120, Created: t0}, Name: "w3wp.exe", Parent: 1480, Path: raizWindows + `\System32\inetsrv\w3wp.exe`}
	winrm := svchostGenuino(4472)
	forest := reaper.NewForest(conWindows(was, w3wp1, w3wp2, winrm))
	svc := map[uint32][]reaper.Service{1480: {{Name: "WAS"}, {Name: "W3SVC"}}, 4472: {{Name: "WinRM"}}}
	h, _ := httpFalso(t, muestraNetsh(t, "netsh_en_requestq_iis.txt"), nil)
	sys := &sistema{http: h, svc: &servicios{leer: func(string) (w win.ServiceConfig, err error) { return w, errors.New("no hay") }}}

	socks := []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 80, ports.Listen, 4, "System"), sock(ports.TCP, "::", 80, ports.Listen, 4, "System"),
		sock(ports.TCP, "0.0.0.0", 5985, ports.Listen, 4, "System"),
		sock(ports.TCP, "0.0.0.0", 8081, ports.Listen, 4, "System"),
		sock(ports.TCP, "0.0.0.0", 9999, ports.Listen, 4, "System"),
	}
	pedidos := ports.Of(80, 5985, 8081, 9999)
	plan := desenmascarar(context.Background(), sys, reaper.PlanKill(pedidos, socks, forest, svc, cfgC2), socks, forest, opciones{tree: true})
	if len(plan.Targets) != 0 {
		t.Fatalf("nadie es objetivo (los w3wp no son los dueños, y el dueño es un svchost): %+v", plan.Targets)
	}
	por := map[uint16]reaper.Refusal{}
	for _, r := range plan.Refusals {
		for p := range r.Ports.All() {
			por[p] = r
		}
	}
	// El 80 y el 8081 (un grupo de aplicaciones sin ningún proceso atendiendo)
	// son de WAS.
	for _, p := range []uint16{80, 8081} {
		r := por[p]
		if r.PID != 1480 || r.Via.PID != 4 || r.ViaName != "System" || !strings.Contains(r.Reason, "W3SVC") || r.Relay.Unmaskable() {
			t.Errorf("%d = %+v", p, r)
		}
	}
	if r := por[5985]; r.PID != 4472 || r.Via.PID != 4 || !strings.Contains(r.Reason, "WinRM") {
		t.Errorf("5985 = %+v", r)
	}
	// El 9999 no tiene ninguna URL: sigue a nombre del kernel, y se dice.
	if r := por[9999]; r.PID != 4 || !r.Via.Zero() || !strings.Contains(r.Reason, "no tiene a la vista ninguna URL registrada en el 9999") ||
		!strings.Contains(r.Advice, "como administrador") {
		t.Errorf("9999 = %+v", r)
	}

	det := detallar(sys, plan, opciones{})
	term := tui.Open()
	term.DisableColor()
	texto := strings.Join(bloques(term, sys, forest, plan, cfgC2, det), "\n")
	for _, dice := range []string{
		"svchost.exe  PID 1480 · a través de System",
		"registró http://*:80/",
		"es IIS, el servidor web de Windows", // la tabla de sospechosos
		"es la administración remota de Windows (WinRM)",
	} {
		if !strings.Contains(texto, dice) {
			t.Errorf("los bloques no dicen %q:\n%s", dice, texto)
		}
	}
	for _, malo := range []string{"net stop http", "sc config http", "start= disabled"} {
		if strings.Contains(strings.ToLower(texto), malo) {
			t.Errorf("los bloques aconsejan %q:\n%s", malo, texto)
		}
	}
}

func TestHTTPSysIdentidades(t *testing.T) {
	forest := reaper.NewForest(conWindows(
		reaper.Proc{ID: reaper.ID{PID: 7000, Created: t0}, Name: "a.exe", Path: `C:\apps\a.exe`},
		reaper.Proc{ID: reaper.ID{PID: 7001, Created: t0}, Name: "b.exe", Path: `C:\apps\b.exe`},
		reaper.Proc{ID: reaper.ID{PID: 7002, Created: t0}, Name: "sinruta.exe"},
	))
	cola := func(ctl uint32, ps ...ports.QueueProcess) ports.RequestQueue {
		return ports.RequestQueue{Controller: ports.QueueProcess{PID: ctl}, Processes: ps, URLs: []ports.QueueURL{{Port: 80}}}
	}
	pids := func(ids []reaper.ID) string {
		var out []string
		for _, id := range ids {
			out = append(out, u32(id.PID))
		}
		return strings.Join(out, ",")
	}
	casos := []struct {
		nombre string
		colas  []ports.RequestQueue
		want   string
	}{
		{"los que atienden, si nadie figura como creador", []ports.RequestQueue{cola(0, ports.QueueProcess{PID: 7000, Image: `C:\apps\a.exe`}, ports.QueueProcess{PID: 7001})}, "7000,7001"},
		{"el creador, si figura", []ports.RequestQueue{cola(7001, ports.QueueProcess{PID: 7000})}, "7001"},
		{"sin distinguir mayúsculas en la ruta", []ports.RequestQueue{cola(0, ports.QueueProcess{PID: 7000, Image: `c:\APPS\A.EXE`})}, "7000"},
		// netsh dice que el PID 7000 es otra imagen: el proceso que vio netsh
		// murió y el número lo reusa otro. No es el mismo.
		{"un PID reciclado", []ports.RequestQueue{cola(0, ports.QueueProcess{PID: 7000, Image: `C:\otra\cosa.exe`})}, ""},
		{"un proceso que ya no está", []ports.RequestQueue{cola(0, ports.QueueProcess{PID: 9999, Image: `C:\x.exe`})}, ""},
		{"sin ruta en la foto no se puede desmentir", []ports.RequestQueue{cola(0, ports.QueueProcess{PID: 7002, Image: `C:\x\sinruta.exe`})}, "7002"},
		{"el mismo en dos colas, una vez", []ports.RequestQueue{cola(0, ports.QueueProcess{PID: 7000}), cola(0, ports.QueueProcess{PID: 7000}, ports.QueueProcess{PID: 7001})}, "7000,7001"},
		{"sin colas", nil, ""},
	}
	for _, c := range casos {
		if got := pids(identidades(c.colas, forest)); got != c.want {
			t.Errorf("%s: %q, esperaba %q", c.nombre, got, c.want)
		}
	}
}

// Cuando no se pudo mirar, o no hay nada: se dice qué pasó y quiénes son los
// sospechosos, y NUNCA se aconseja tocar el controlador HTTP.
func TestHTTPSysSinRespuesta(t *testing.T) {
	forest := reaper.NewForest(esqueletoWindows())
	masked := []reaper.MaskedPort{{Via: sistemaID, Name: "System", Relay: reaper.RelayHTTP, Port: 80, Proto: ports.TCP}}
	muchos := []string{"W3SVC", "WinRM", "Spooler", "SSDPSRV", "upnphost", "MsDepSvc", "WMPNetworkSvc", "PeerDistSvc"}

	casos := []struct {
		nombre  string
		salida  string
		err     error
		elevado bool
		deps    []string
		veredic reaper.UnmaskVerdict
		motivo  string
		consejo string
	}{
		{"netsh falló", "", &errorNetsh{Codigo: 1, Dijo: "El servicio no responde."}, false, []string{"W3SVC", "WinRM"},
			reaper.UnmaskUnavailable, "no pude preguntarle a http.sys de quién es: netsh: El servicio no responde.", "los servicios activos que usan http.sys son W3SVC, WinRM"},
		{"netsh contestó otra cosa", "Comando no encontrado.\n", nil, false, nil,
			reaper.UnmaskUnavailable, "no tiene la vista de colas", "netsh http show servicestate"},
		{"sin colas, sin administrador", muestraNetsh(t, "netsh_es_requestq_vacia.txt"), nil, false, muchos,
			reaper.UnmaskEmpty, "http.sys no tiene a la vista ninguna URL registrada en el 80", "W3SVC, WinRM, Spooler, SSDPSRV, upnphost, MsDepSvc y 2 más"},
		{"sin colas, como administrador: no es http.sys", muestraNetsh(t, "netsh_es_requestq_vacia.txt"), nil, true, muchos,
			reaper.UnmaskEmpty, "no es http.sys", "otro controlador del kernel"},
		{"la URL es de otro puerto", muestraNetsh(t, "netsh_es_requestq_una.txt"), nil, false, nil,
			reaper.UnmaskEmpty, "ninguna URL registrada en el 80", "solo se ven como administrador"},
		// Hay una URL en el 80 pero el proceso que la registró no está en la
		// foto (murió, o es de otra sesión).
		{"hay URL y nadie a la vista", muestraNetsh(t, "netsh_es_requestq_dos_urls.txt"), nil, false, nil,
			reaper.UnmaskEmpty, "http.sys tiene registrada http://+:80/TEMPORARY_LISTEN_ADDRESSES/KP-PRUEBA/, pero no hay a la vista ningún proceso", "IIS"},
	}
	for _, c := range casos {
		h, _ := httpFalso(t, c.salida, c.err, c.deps...)
		h.elevado = func() bool { return c.elevado }
		us := h.desenmascarar(context.Background(), masked, forest)
		if len(us) != 1 {
			t.Fatalf("%s: %d respuestas", c.nombre, len(us))
		}
		u := us[0]
		if u.Verdict != c.veredic || !strings.Contains(u.Reason, c.motivo) || !strings.Contains(u.Advice, c.consejo) ||
			len(u.Procs) != 0 || len(u.Behind) != 0 || u.KillVia || u.Via != sistemaID || u.Port != 80 {
			t.Errorf("%s:\n  %+v", c.nombre, u)
		}
		for _, malo := range []string{"net stop http", "sc config http", "start= disabled"} {
			if strings.Contains(strings.ToLower(u.Reason+" "+u.Advice), malo) {
				t.Errorf("%s aconseja %q: %+v", c.nombre, malo, u)
			}
		}
	}
}

// El listado general mira detrás de System ANTES de ocultar lo de Windows: el
// servidor de un programa del usuario no queda escondido.
func TestHTTPSysEnElListado(t *testing.T) {
	pwsh := reaper.Proc{ID: reaper.ID{PID: 38840, Created: t0}, Name: "pwsh.exe", Parent: 3000, Path: pwshMuestra}
	forest := reaper.NewForest(conWindows(
		reaper.Proc{ID: reaper.ID{PID: 3000, Created: t0.Add(-60e9)}, Name: "explorer.exe", Path: raizWindows + `\explorer.exe`}, pwsh))
	h, corridas := httpFalso(t, muestraNetsh(t, "netsh_es_requestq_dos_urls.txt"), nil)
	sys := &sistema{http: h}
	socks := []ports.Socket{
		sock(ports.TCP, "::", 80, ports.Listen, 4, "System"),
		sock(ports.TCP, "::", 47181, ports.Listen, 4, "System"), sock(ports.TCP, "0.0.0.0", 47181, ports.Listen, 4, "System"),
		sock(ports.TCP, "0.0.0.0", 445, ports.Listen, 4, "System"),
		sock(ports.TCP, "0.0.0.0", 5357, ports.Listen, 4, "System"), // del kernel, sin URL a la vista
		sock(ports.UDP, "0.0.0.0", 4500, ports.StateNone, 4, "System"),
	}
	filas := anotarWindows(context.Background(), sys, filasDeListado(socks, forest, nil, cfgC2), forest, nil, cfgC2)
	if *corridas != 1 {
		t.Fatalf("netsh corrió %d veces para todo el listado", *corridas)
	}
	muestra, ocultas := visibles(filas, mostrar{})
	if len(muestra) != 2 || ocultas.filas != 3 {
		t.Fatalf("visibles = %+v (ocultas %d)", muestra, ocultas.filas)
	}
	for i, puerto := range []uint16{80, 47181} {
		f := muestra[i]
		if f.Port != puerto || nombreFila(f) != "http.sys · pwsh.exe" || f.Name != "System" || pidFila(f) != 38840 || f.PID != 4 || f.Via != "System" ||
			f.Relay != reaper.RelayHTTP || f.EsWindows || f.HTTP == nil || len(f.HTTP.URLs) != 1 {
			t.Errorf("fila del %d = %+v", puerto, f)
		}
	}
	if muestra[0].Comando != "http://+:80/TEMPORARY_LISTEN_ADDRESSES/KP-PRUEBA/" || muestra[1].Comando != "http://localhost:47181/KP-DOS/" {
		t.Errorf("comandos = %q / %q", muestra[0].Comando, muestra[1].Comando)
	}
	// Lo que sigue siendo de Windows no dice ser de un intermediario.
	for _, f := range filas {
		if (f.Port == 445 || f.Port == 5357 || f.Port == 4500) && (f.Relay != reaper.RelayNone || !f.EsWindows || f.HTTP != nil) {
			t.Errorf("fila del %d = %+v", f.Port, f)
		}
	}

	b, _ := json.Marshal(construirJSONListado(filas, ocultas))
	var doc struct {
		Escuchando []map[string]any `json:"escuchando"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, e := range doc.Escuchando {
		puerto := uint16(e["puerto"].(float64))
		_, tieneHTTP := e["http"]
		if esHTTP := puerto == 80 || puerto == 47181; tieneHTTP != esHTTP || (e["intermediario"] == "http.sys") != esHTTP || (e["via"] == "System") != esHTTP {
			t.Errorf("JSON del %d = %v", puerto, e)
		}
	}

	// Dos procesos con una URL en el MISMO puerto, y el primero que nombra
	// http.sys es de Windows: la fila no se oculta (el segundo es del usuario),
	// lo nombra a él y dice que hay uno más.
	host := svchostGenuino(1480)
	mixto := reaper.NewForest(conWindows(host,
		reaper.Proc{ID: reaper.ID{PID: 3000, Created: t0.Add(-60e9)}, Name: "explorer.exe", Path: raizWindows + `\explorer.exe`}, pwsh))
	url := func(camino string) []ports.QueueURL {
		return []ports.QueueURL{{Raw: "HTTP://+:80/" + camino, Host: "+", Port: 80, Path: "/" + camino}}
	}
	compartido := &httpsys{netsh: func(context.Context, ...string) (string, error) { return "", nil }, dependientes: func(string) []string { return nil },
		elevado: func() bool { return false }}
	compartido.consultar(context.Background()) // gasta la única consulta: las colas se fijan a mano
	compartido.colas, compartido.err = []ports.RequestQueue{
		{Controller: ports.QueueProcess{PID: 1480}, URLs: url("IIS/")},
		{Processes: []ports.QueueProcess{{PID: 38840, Image: pwshMuestra}}, URLs: url("MIO/")},
	}, nil
	filas = anotarWindows(context.Background(), &sistema{http: compartido}, filasDeListado(socks[:1], mixto, map[uint32][]reaper.Service{1480: {{Name: "WAS"}}}, cfgC2),
		mixto, map[uint32][]reaper.Service{1480: {{Name: "WAS"}}}, cfgC2)
	if len(filas) != 1 {
		t.Fatalf("filas = %+v", filas)
	}
	if f := filas[0]; f.EsWindows || nombreFila(f) != "http.sys · pwsh.exe y 1 más" || pidFila(f) != 38840 || f.Comando != "http://+:80/MIO/" ||
		f.HTTP == nil || len(f.HTTP.Otros) != 1 || f.HTTP.Otros[0].PID != 1480 || len(f.HTTP.Otros[0].URLs) != 1 {
		t.Errorf("fila compartida = %+v (http %+v)", f, f.HTTP)
	}
	if muestra, ocultas := visibles(filas, mostrar{}); len(muestra) != 1 || ocultas.filas != 0 {
		t.Errorf("la fila compartida quedó oculta")
	}
	b, _ = json.Marshal(construirJSONListado(filas, sinMostrar{}))
	if !strings.Contains(string(b), `"http":{"pid":38840,"proceso":"pwsh.exe","urls":["http://+:80/MIO/"],"otros":[{"pid":1480,"proceso":"svchost.exe","urls":["http://+:80/IIS/"]}]}`) {
		t.Errorf("JSON de la fila compartida:\n%s", b)
	}

	// Detrás de http.sys, un POSIBLE IMPOSTOR: un "svchost.exe" que corre desde
	// AppData registró una URL en el mismo puerto que el servidor del usuario. No
	// es el que nombra la fila (es el segundo), pero la fila lo marca igual: se
	// miran todos los que están detrás, no solo el principal.
	falso := reaper.Proc{ID: reaper.ID{PID: 6666, Created: t0}, Name: "svchost.exe", Parent: 3000, Path: `C:\Users\u\AppData\Roaming\svchost.exe`}
	conFalso := reaper.NewForest(conWindows(
		reaper.Proc{ID: reaper.ID{PID: 3000, Created: t0.Add(-60e9)}, Name: "explorer.exe", Path: raizWindows + `\explorer.exe`}, pwsh, falso))
	compartido.colas = []ports.RequestQueue{
		{Processes: []ports.QueueProcess{{PID: 38840, Image: pwshMuestra}}, URLs: url("MIO/")},
		{Processes: []ports.QueueProcess{{PID: 6666}}, URLs: url("OTRO/")},
	}
	filas = anotarWindows(context.Background(), &sistema{http: compartido}, filasDeListado(socks[:1], conFalso, nil, cfgC2), conFalso, nil, cfgC2)
	if len(filas) != 1 || nombreFila(filas[0]) != "http.sys · pwsh.exe y 1 más (!)" || filas[0].EsWindows ||
		filas[0].impPID != 6666 || !strings.Contains(filas[0].Impostor, "AppData") {
		t.Errorf("fila con un posible impostor detrás = %+v", filas)
	}

	// Todos los que están detrás son de Windows (WAS y WinRM en el mismo 80): la
	// fila nombra al PRIMERO en el orden de netsh —determinista—, dice que hay
	// otro, y sigue oculta.
	dosWindows := reaper.NewForest(conWindows(svchostGenuino(1480), svchostGenuino(4472)))
	svcDos := map[uint32][]reaper.Service{1480: {{Name: "WAS"}}, 4472: {{Name: "WinRM"}}}
	compartido.colas = []ports.RequestQueue{
		{Controller: ports.QueueProcess{PID: 1480}, URLs: url("IIS/")},
		{Controller: ports.QueueProcess{PID: 4472}, URLs: url("wsman/")},
	}
	filas = anotarWindows(context.Background(), &sistema{http: compartido}, filasDeListado(socks[:1], dosWindows, svcDos, cfgC2), dosWindows, svcDos, cfgC2)
	if len(filas) != 1 || nombreFila(filas[0]) != "http.sys · svchost.exe y 1 más" || pidFila(filas[0]) != 1480 || !filas[0].EsWindows ||
		filas[0].HTTP == nil || len(filas[0].HTTP.Otros) != 1 || filas[0].HTTP.Otros[0].PID != 4472 {
		t.Errorf("fila con dos de Windows detrás = %+v", filas)
	}

	// Si el que está detrás es de Windows (un svchost), la fila se nombra igual
	// pero sigue oculta.
	h2, _ := httpFalso(t, muestraNetsh(t, "netsh_en_requestq_iis.txt"), nil)
	bosque := reaper.NewForest(conWindows(svchostGenuino(1480), svchostGenuino(4472)))
	svc := map[uint32][]reaper.Service{1480: {{Name: "WAS"}}, 4472: {{Name: "WinRM"}}}
	filas = anotarWindows(context.Background(), &sistema{http: h2}, filasDeListado(socks[:1], bosque, svc, cfgC2), bosque, svc, cfgC2)
	if len(filas) != 1 || nombreFila(filas[0]) != "http.sys · svchost.exe" || !filas[0].EsWindows || pidFila(filas[0]) != 1480 {
		t.Errorf("fila = %+v", filas)
	}
}
