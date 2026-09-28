//go:build windows

package killport

// Función 9: que el servicio no vuelva al reiniciar. El administrador de
// servicios es un doble: ninguna de estas pruebas lee ni cambia un servicio de
// la máquina.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

// scmFalso es el administrador de servicios de las pruebas.
type scmFalso struct {
	cfg      map[string]win.ServiceConfig
	estado   map[string]uint32 // 1 detenido, 4 corriendo
	cambios  []string          // los servicios que se dejaron en manual
	niega    bool              // cambiar pide administrador
	sordo    bool              // dice que sí y no cambia nada
	lecturas int
}

func (s *scmFalso) leer(nombre string) (win.ServiceConfig, error) {
	s.lecturas++
	c, ok := s.cfg[nombre]
	if !ok {
		return win.ServiceConfig{}, win.ErrServiceNotFound
	}
	c.Name = nombre
	return c, nil
}

func (s *scmFalso) estadoDe(nombre string) (win.ServiceState, error) {
	e, ok := s.estado[nombre]
	if !ok {
		return win.ServiceState{}, win.ErrServiceNotFound
	}
	return win.ServiceState{State: e}, nil
}

func (s *scmFalso) manual(nombre string) error {
	if s.niega {
		return win.ErrAccessDenied
	}
	s.cambios = append(s.cambios, nombre)
	if !s.sordo {
		c := s.cfg[nombre]
		c.Start, c.Delayed = win.StartDemand, false
		s.cfg[nombre] = c
	}
	return nil
}

func (s *scmFalso) cliente() *servicios {
	return &servicios{leer: s.leer, estado: s.estadoDe, manual: s.manual}
}

const (
	detenido  = 1
	corriendo = 4
)

func terceros(inicio win.StartType, retrasado bool, disparadores int) win.ServiceConfig {
	return win.ServiceConfig{Type: win.ServiceWin32OwnProcess, Start: inicio, Delayed: retrasado, Triggers: disparadores,
		BinaryPath: `"C:\Program Files\MySQL\MySQL Server 8.0\bin\mysqld.exe" MySQL80`}
}

func TestManualReglas(t *testing.T) {
	casos := []struct {
		nombre   string
		cfg      win.ServiceConfig
		existe   bool
		estado   uint32
		niega    bool
		sordo    bool
		fin      finManual
		dice     string
		cambia   bool
		deshacer string
		avisa    string
	}{
		{"automático y detenido: queda en manual", terceros(win.StartAuto, false, 0), true, detenido, false, false,
			manualHecho, "", true, "sc config MySQL80 start= auto", ""},
		{"automático retrasado: se deshace con delayed-auto", terceros(win.StartAuto, true, 0), true, detenido, false, false,
			manualHecho, "", true, "sc config MySQL80 start= delayed-auto", ""},
		{"con disparadores: queda en manual y se avisa que vuelve igual", terceros(win.StartAuto, false, 2), true, detenido, false, false,
			manualHecho, "", true, "sc config MySQL80 start= auto", "2 disparadores"},
		{"ya era manual", terceros(win.StartDemand, false, 0), true, detenido, false, false,
			manualYaEstaba, "ya estaba en inicio manual", false, "", ""},
		{"ya era manual, con disparadores", terceros(win.StartDemand, false, 1), true, detenido, false, false,
			manualYaEstaba, "ya estaba en inicio manual", false, "", "1 disparador"},
		{"deshabilitado: no se HABILITA", terceros(win.StartDisabled, false, 0), true, detenido, false, false,
			manualYaEstaba, "deshabilitado", false, "", ""},
		{"sigue corriendo: no se le toca la configuración", terceros(win.StartAuto, false, 0), true, corriendo, false, false,
			manualSigueVivo, "sigue corriendo", false, "", ""},
		{"se está deteniendo todavía", terceros(win.StartAuto, false, 0), true, 3, false, false,
			manualSigueVivo, "sigue corriendo", false, "", ""},
		{"pide administrador", terceros(win.StartAuto, false, 0), true, detenido, true, false,
			manualDenegado, "administrador", false, "", "sc config MySQL80 start= demand"},
		{"Windows dijo que sí y no cambió nada", terceros(win.StartAuto, false, 0), true, detenido, false, true,
			manualFallo, "no quedó en inicio manual", true, "", ""},
		{"ya no existe", win.ServiceConfig{}, false, detenido, false, false,
			manualFallo, "ya no existe", false, "", ""},
		// Lo que killport no reconfigura JAMÁS, esté como esté.
		{"un servicio de Windows", win.ServiceConfig{Type: win.ServiceWin32OwnProcess, Start: win.StartAuto, BinaryPath: `C:\Windows\System32\spoolsv.exe`}, true, detenido, false, false,
			manualNoSeToca, "servicio de Windows", false, "", ""},
		{"uno dentro de un svchost", win.ServiceConfig{Type: win.ServiceWin32ShareProc, Start: win.StartAuto, BinaryPath: `%SystemRoot%\system32\svchost.exe -k netsvcs`}, true, detenido, false, false,
			manualNoSeToca, "servicio de Windows", false, "", ""},
		{"un controlador (winnat, HTTP)", win.ServiceConfig{Type: win.ServiceKernelDriver, Start: win.StartDemand, BinaryPath: `system32\drivers\winnat.sys`}, true, detenido, false, false,
			manualNoSeToca, "controlador", false, "", ""},
	}
	for _, c := range casos {
		scm := &scmFalso{cfg: map[string]win.ServiceConfig{}, estado: map[string]uint32{"MySQL80": c.estado}, niega: c.niega, sordo: c.sordo}
		if c.existe {
			scm.cfg["MySQL80"] = c.cfg
		}
		r := scm.cliente().dejarEnManual("MySQL80", raizWindows)
		if r.Fin != c.fin || !strings.Contains(r.Motivo, c.dice) || (c.dice == "") != (r.Motivo == "") {
			t.Errorf("%s: %v, motivo %q", c.nombre, r.Fin.clave(), r.Motivo)
		}
		if (len(scm.cambios) > 0) != c.cambia {
			t.Errorf("%s: cambios = %v", c.nombre, scm.cambios)
		}
		if r.Deshacer != c.deshacer {
			t.Errorf("%s: deshacer = %q, esperaba %q", c.nombre, r.Deshacer, c.deshacer)
		}
		if (c.avisa == "") != (r.Aviso == "") || !strings.Contains(r.Aviso, c.avisa) {
			t.Errorf("%s: aviso = %q", c.nombre, r.Aviso)
		}
		if r.Servicio != "MySQL80" {
			t.Errorf("%s: servicio = %q", c.nombre, r.Servicio)
		}
	}
}

// planConServicio arma un plan con un servicio de terceros en el 3306 y un
// proceso común en el 3000.
func planConServicio(t *testing.T) (reaper.Plan, *reaper.Forest, reaper.Proc) {
	t.Helper()
	mysqld := reaper.Proc{ID: reaper.ID{PID: 5120, Created: t0}, Name: "mysqld.exe", Parent: pidServices,
		Path: `C:\Program Files\MySQL\MySQL Server 8.0\bin\mysqld.exe`}
	node := reaper.Proc{ID: reaper.ID{PID: 900, Created: t0}, Name: "node.exe", Parent: 3000, Path: `C:\proy\node.exe`}
	forest := reaper.NewForest(conWindows(mysqld, node))
	svc := map[uint32][]reaper.Service{5120: {{Name: "MySQL80", Display: "MySQL80"}}}
	socks := []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 3306, ports.Listen, 5120, "mysqld.exe"),
		sock(ports.TCP, "0.0.0.0", 3000, ports.Listen, 900, "node.exe"),
	}
	plan := reaper.PlanKill(ports.Of(3000, 3306), socks, forest, svc, cfgC2)
	if len(plan.Targets) != 2 || plan.Targets[1].Kind != reaper.StopThirdSvc || plan.Targets[1].Service != "MySQL80" {
		t.Fatalf("plan = %+v", plan.Targets)
	}
	return plan, forest, mysqld
}

func TestManualDespuesDeLaEscalada(t *testing.T) {
	plan, _, mysqld := planConServicio(t)
	reporte := func(fin reaper.Outcome) reaper.Report {
		return reaper.Report{Victims: []reaper.VictimResult{
			{ID: reaper.ID{PID: 900, Created: t0}, Name: "node.exe", Outcome: reaper.OutcomeClosed, Method: reaper.MethodCtrlC},
			{ID: mysqld.ID, Name: "mysqld.exe", Outcome: fin},
		}}
	}
	nuevo := func(estado uint32) (*scmFalso, *sistema) {
		scm := &scmFalso{cfg: map[string]win.ServiceConfig{"MySQL80": terceros(win.StartAuto, false, 0)}, estado: map[string]uint32{"MySQL80": estado}}
		return scm, &sistema{svc: scm.cliente(), admin: func() bool { return true }}
	}

	// Se detuvo: queda en manual. El proceso común no tiene nada que ver.
	scm, sys := nuevo(detenido)
	rs := aplicarManual(sys, plan, reporte(reaper.OutcomeServiceStopped), cfgC2)
	if len(rs) != 1 || rs[0].Fin != manualHecho || rs[0].Deshacer != "sc config MySQL80 start= auto" || !reflect.DeepEqual(scm.cambios, []string{"MySQL80"}) {
		t.Fatalf("detenido: %+v, cambios %v", rs, scm.cambios)
	}
	if necesitaElevacion(reporte(reaper.OutcomeServiceStopped), rs) {
		t.Error("salió todo bien: no hay nada que elevar")
	}

	// No se pudo detener por falta de permiso: no se le toca la configuración,
	// y el --manual viaja en el trabajo elevado.
	scm, sys = nuevo(corriendo)
	rep := reporte(reaper.OutcomeDenied)
	rs = aplicarManual(sys, plan, rep, cfgC2)
	if len(rs) != 1 || rs[0].Fin != manualDenegado || !rs[0].Elevable || len(scm.cambios) != 0 || rs[0].Antes.Start != win.StartAuto {
		t.Fatalf("denegado: %+v, cambios %v", rs, scm.cambios)
	}
	if !necesitaElevacion(rep, rs) {
		t.Error("un servicio denegado se reintenta elevado")
	}
	job := trabajoDe(plan, rep, rs, opciones{manual: true, timeout: 3 * time.Second})
	if !reflect.DeepEqual(job.Manual, []string{"MySQL80"}) || len(job.Servicios) != 1 || job.Servicios[0].Nombre != "MySQL80" ||
		job.Servicios[0].Owner != mysqld.ID || len(job.Kill) != 0 {
		t.Fatalf("trabajo = %+v", job)
	}
	if texto, err := codificarJob(job); err != nil || !strings.Contains(texto, ";manual=") {
		t.Errorf("trabajo = %q (%v)", texto, err)
	}

	// No se pudo detener por otro motivo: no se toca, y elevar no lo arregla.
	// Tampoco si, para cuando se mira, el servicio está detenido (se venció el
	// plazo y se detuvo después, o lo detuvo otro): killport le cambia el
	// inicio a lo que ÉL vio detenerse.
	for _, estado := range []uint32{corriendo, detenido} {
		scm, sys = nuevo(estado)
		rs = aplicarManual(sys, plan, reporte(reaper.OutcomeError), cfgC2)
		if len(rs) != 1 || rs[0].Fin != manualSigueVivo || len(scm.cambios) != 0 {
			t.Fatalf("con error (estado %d): %+v, cambios %v", estado, rs, scm.cambios)
		}
	}
	if necesitaElevacion(reporte(reaper.OutcomeError), rs) {
		t.Error("un error que no es de permiso no se arregla elevando")
	}

	// El padre lo detuvo pero no le dejaron cambiar el inicio. NO se eleva: el
	// proceso elevado le cambia el inicio solo a lo que él mismo detiene, y
	// este servicio ya está detenido. Se dice el comando.
	scm, sys = nuevo(detenido)
	scm.niega = true
	rep = reporte(reaper.OutcomeServiceStopped)
	rs = aplicarManual(sys, plan, rep, cfgC2)
	if len(rs) != 1 || rs[0].Fin != manualDenegado || rs[0].Elevable || necesitaElevacion(rep, rs) {
		t.Fatalf("detenido y sin permiso de cambiar: %+v", rs)
	}
	if !strings.Contains(rs[0].Aviso, "sc config MySQL80 start= demand") {
		t.Errorf("no dice cómo hacerlo a mano: %q", rs[0].Aviso)
	}
	term0 := tui.Open()
	term0.DisableColor()
	if l := strings.Join(lineasManual(term0, rs, opciones{manual: true}, plan), "\n"); !strings.Contains(l, "No pude dejar MySQL80 en inicio manual") ||
		!strings.Contains(l, "pide administrador") || !strings.Contains(l, "sc config MySQL80 start= demand") {
		t.Errorf("líneas = %q", l)
	}
	// Lo que el padre ya hizo no viaja: ni el servicio detenido, ni su --manual.
	otroDenegado := reaper.Report{Victims: []reaper.VictimResult{
		{ID: reaper.ID{PID: 900, Created: t0}, Name: "node.exe", Outcome: reaper.OutcomeDenied},
		{ID: mysqld.ID, Name: "mysqld.exe", Outcome: reaper.OutcomeServiceStopped},
	}}
	if job := trabajoDe(plan, otroDenegado, rs, opciones{manual: true, timeout: time.Second}); len(job.Manual) != 0 || len(job.Servicios) != 0 ||
		len(job.Kill) != 1 || job.Kill[0].PID != 900 {
		t.Fatalf("trabajo = %+v", job)
	}

	// Un plan sin servicios: --manual no tiene a quién.
	sinServicios := reaper.Plan{Targets: plan.Targets[:1]}
	if rs := aplicarManual(sys, sinServicios, reporte(reaper.OutcomeClosed), cfgC2); len(rs) != 0 {
		t.Errorf("sin servicios: %+v", rs)
	}
	term := tui.Open()
	term.DisableColor()
	if l := strings.Join(lineasManual(term, nil, opciones{manual: true}, sinServicios), "\n"); !strings.Contains(l, "--manual no hizo nada") {
		t.Errorf("líneas = %q", l)
	}
}

func TestManualSeCuenta(t *testing.T) {
	plan, forest, mysqld := planConServicio(t)
	scm := &scmFalso{cfg: map[string]win.ServiceConfig{"MySQL80": terceros(win.StartAuto, false, 0)}, estado: map[string]uint32{"MySQL80": corriendo}}
	sys := &sistema{svc: scm.cliente(), admin: func() bool { return false }}
	term := tui.Open()
	term.DisableColor()

	// Antes de actuar, sin --manual: el bloque dice que vuelve y cómo evitarlo.
	det := detallar(sys, plan, opciones{})
	texto := strings.Join(bloques(term, sys, forest, plan, cfgC2, det), "\n")
	for _, dice := range []string{
		"servicio MySQL80 · inicio automático: vuelve al reiniciar Windows",
		"para que no vuelva: killport 3306 --manual",
		"es MySQL o MariaDB instalado como servicio de Windows",
	} {
		if !strings.Contains(texto, dice) {
			t.Errorf("el bloque no dice %q:\n%s", dice, texto)
		}
	}
	if scm.lecturas != 1 {
		t.Errorf("la configuración se leyó %d veces", scm.lecturas)
	}
	// Con --manual: el bloque dice qué va a pasar y cómo se deshace.
	det = detallar(sys, plan, opciones{manual: true})
	texto = strings.Join(bloques(term, sys, forest, plan, cfgC2, det), "\n")
	if !strings.Contains(texto, "--manual: después de detenerlo queda en inicio manual (se deshace con `sc config MySQL80 start= auto`)") ||
		strings.Contains(texto, "para que no vuelva") {
		t.Errorf("el bloque con --manual:\n%s", texto)
	}

	soloServicio := reaper.Plan{Targets: plan.Targets[1:]}
	for _, c := range []struct {
		manual, admin bool
		want          string
	}{
		{false, false, "¿Detener el servicio MySQL80 y liberar el 3306? [s/N]"},
		{true, false, "¿Detener el servicio MySQL80, dejarlo en inicio manual y liberar el 3306? [s/N]"},
		{true, true, "¿Detener el servicio MySQL80, dejarlo en inicio manual y liberar el 3306? (va a pedir permiso de administrador) [s/N]"},
	} {
		if got := strings.TrimSpace(preguntaCierre(term, soloServicio, c.manual, c.admin)); got != c.want {
			t.Errorf("pregunta = %q, esperaba %q", got, c.want)
		}
	}
	if !planPideAdmin(plan) || planPideAdmin(reaper.Plan{Targets: plan.Targets[:1]}) {
		t.Error("detener un servicio pide administrador; cerrar un proceso común, no")
	}

	// Después: se detuvo y NO se pidió --manual → se avisa que vuelve.
	rep := reaper.Report{Victims: []reaper.VictimResult{{ID: mysqld.ID, Name: "mysqld.exe", Outcome: reaper.OutcomeServiceStopped, Method: reaper.MethodService}}}
	det = detallar(sys, plan, opciones{})
	if l := strings.Join(lineasVuelve(term, plan, rep, nil, det), "\n"); !strings.Contains(l, "El servicio MySQL80 va a volver") ||
		!strings.Contains(l, "vuelve al reiniciar Windows; para que no vuelva: killport 3306 --manual") {
		t.Errorf("aviso = %q", l)
	}
	// Con --manual hecho, no hay nada que avisar; la tarjeta lo cuenta.
	hecho := []resultadoManual{{Servicio: "MySQL80", Fin: manualHecho, Deshacer: "sc config MySQL80 start= auto"}}
	if l := lineasVuelve(term, plan, rep, hecho, det); len(l) != 0 {
		t.Errorf("ya quedó en manual y avisa: %q", l)
	}
	if l := strings.Join(lineasManual(term, hecho, opciones{manual: true}, plan), "\n"); !strings.Contains(l, "El servicio MySQL80 quedó en inicio manual") ||
		!strings.Contains(l, "se deshace con `sc config MySQL80 start= auto`") {
		t.Errorf("líneas = %q", l)
	}
	if c := strings.Join(tarjeta(term, rep, hecho, 0), "\n"); !strings.Contains(c, "En inicio manual") || !strings.Contains(c, "Servicios detenidos") {
		t.Errorf("tarjeta = %q", c)
	}
	// Si no se detuvo, no se avisa que vuelve (nunca se fue).
	noSeDetuvo := reaper.Report{Victims: []reaper.VictimResult{{ID: mysqld.ID, Outcome: reaper.OutcomeDenied}}}
	if l := lineasVuelve(term, plan, noSeDetuvo, nil, det); len(l) != 0 {
		t.Errorf("no se detuvo y avisa: %q", l)
	}
	// Un servicio manual con disparadores vuelve igual: se dice cómo verlos.
	scm.cfg["MySQL80"] = terceros(win.StartDemand, false, 2)
	sys.svc.olvidar("MySQL80")
	det = detallar(sys, plan, opciones{})
	if l := strings.Join(lineasVuelve(term, plan, rep, nil, det), "\n"); !strings.Contains(l, "2 disparadores") || !strings.Contains(l, "sc qtriggerinfo MySQL80") ||
		strings.Contains(l, "--manual") {
		t.Errorf("aviso = %q", l)
	}

	// JSON: el objetivo lleva cómo arranca, y el documento cómo le fue a --manual.
	scm.cfg["MySQL80"] = win.ServiceConfig{Type: win.ServiceWin32OwnProcess, Start: win.StartAuto, Delayed: true, Triggers: 1,
		Recovery: []win.RecoveryAction{win.RecoveryRestart, win.RecoveryNone}, BinaryPath: `C:\mysql\mysqld.exe`}
	sys.svc.olvidar("MySQL80")
	det = detallar(sys, plan, opciones{manual: true})
	doc := construirJSONReporte(rep, plan, false)
	det.anotar(&doc, plan)
	doc.Manual = manualJSON([]resultadoManual{{Servicio: "MySQL80", Fin: manualHecho, Antes: nombrado(scm.cfg["MySQL80"], "MySQL80"), Deshacer: "sc config MySQL80 start= delayed-auto", Aviso: "ojo"}})
	b, _ := json.Marshal(doc)
	for _, dice := range []string{
		`"tipo":"servicio"`, `"servicio":"MySQL80"`,
		`"inicio":{"inicio":"auto-diferido","disparadores":1,"recuperacion":["reiniciar-servicio","nada"],"vuelve_solo":true}`,
		`"conocido":"es MySQL o MariaDB instalado como servicio de Windows"`,
		`"manual":[{"servicio":"MySQL80","resultado":"manual","antes":"auto-diferido","deshacer":"sc config MySQL80 start= delayed-auto","aviso":"ojo"}]`,
	} {
		if !strings.Contains(string(b), dice) {
			t.Errorf("el JSON no dice %s:\n%s", dice, b)
		}
	}
	// El proceso común no lleva nada de servicios.
	var back struct {
		Objetivos []map[string]any `json:"objetivos"`
	}
	json.Unmarshal(b, &back)
	if _, tiene := back.Objetivos[0]["inicio"]; tiene || back.Objetivos[0]["proceso"] != "node.exe" {
		t.Errorf("el objetivo de node.exe = %v", back.Objetivos[0])
	}
}

// Una negativa por un servicio de WINDOWS dice cómo arranca; qué hacer lo dice
// la tabla de sospechosos, si lo conoce. killport no le cambia el inicio ni
// aconseja cambiárselo por su cuenta.
func TestNegativaDeServicioDeWindows(t *testing.T) {
	host := svchostGenuino(1480)
	forest := reaper.NewForest(conWindows(host))
	svc := map[uint32][]reaper.Service{1480: {{Name: "W3SVC"}}}
	socks := []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 80, ports.Listen, 1480, "W3SVC"),
		sock(ports.TCP, "0.0.0.0", 135, ports.Listen, 1480, "W3SVC"), // por un puerto por el que no se lo conoce
	}
	scm := &scmFalso{cfg: map[string]win.ServiceConfig{"W3SVC": {Type: win.ServiceWin32ShareProc, Start: win.StartAuto,
		BinaryPath: `C:\Windows\system32\svchost.exe -k iissvcs`}}, estado: map[string]uint32{"W3SVC": corriendo}}
	sys := &sistema{svc: scm.cliente()}
	term := tui.Open()
	term.DisableColor()

	plan := reaper.PlanKill(ports.Of(80), socks[:1], forest, svc, cfgC2)
	texto := strings.Join(bloques(term, sys, forest, plan, cfgC2, detallar(sys, plan, opciones{manual: true})), "\n")
	for _, dice := range []string{"servicio W3SVC · inicio automático: vuelve al reiniciar Windows", "es IIS, el servidor web de Windows", "sc config W3SVC start= demand"} {
		if !strings.Contains(texto, dice) {
			t.Errorf("la negativa no dice %q:\n%s", dice, texto)
		}
	}
	// --manual no hace nada con una negativa.
	if rs := aplicarManual(sys, plan, reaper.Report{}, cfgC2); len(rs) != 0 || len(scm.cambios) != 0 {
		t.Errorf("--manual sobre un servicio de Windows: %+v, %v", rs, scm.cambios)
	}
	// Y aunque alguien se lo pida directamente, se niega.
	scm.estado["W3SVC"] = detenido
	if r := sys.svc.dejarEnManual("W3SVC", raizWindows); r.Fin != manualNoSeToca || len(scm.cambios) != 0 {
		t.Errorf("dejarEnManual(W3SVC) = %+v", r)
	}

	// Por un puerto por el que no se lo conoce: el inicio sí, el consejo no.
	plan = reaper.PlanKill(ports.Of(135), socks[1:], forest, svc, cfgC2)
	texto = strings.Join(bloques(term, sys, forest, plan, cfgC2, detallar(sys, plan, opciones{})), "\n")
	if !strings.Contains(texto, "servicio W3SVC · inicio automático") || strings.Contains(texto, "sc config") || strings.Contains(texto, "IIS") {
		t.Errorf("la negativa del 135:\n%s", texto)
	}
}

// Un servicio de Windows que corre sin aceptar que lo detengan (RpcSs): el
// consejo de siempre, `net stop`, lo manda al usuario a chocar contra un
// error. Con el estado a la vista, se dice la verdad.
func TestNegativaDeServicioQueNoSeDetiene(t *testing.T) {
	host := svchostGenuino(1772)
	otro := svchostGenuino(3232)
	forest := reaper.NewForest(conWindows(host, otro))
	svc := map[uint32][]reaper.Service{1772: {{Name: "RpcSs"}}, 3232: {{Name: "DoSvc"}}}
	socks := []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 135, ports.Listen, 1772, "RpcSs"),
		sock(ports.TCP, "0.0.0.0", 7680, ports.Listen, 3232, "DoSvc"),
		sock(ports.TCP, "0.0.0.0", 445, ports.Listen, 4, "System"),
	}
	const aceptaDetener = 0x1
	estados := map[string]win.ServiceState{
		"RpcSs": {State: corriendo},                         // corre y no acepta nada
		"DoSvc": {State: corriendo, Accepts: aceptaDetener}, // uno común
	}
	consultas := 0
	sys := &sistema{svc: &servicios{
		leer: func(string) (win.ServiceConfig, error) { return win.ServiceConfig{}, win.ErrServiceNotFound },
		estado: func(n string) (win.ServiceState, error) {
			consultas++
			st, ok := estados[n]
			if !ok {
				return st, win.ErrServiceNotFound
			}
			return st, nil
		},
	}}
	plan := reaper.PlanKill(ports.Of(135, 445, 7680), socks, forest, svc, cfgC2)
	afinado := afinarNegativas(sys.svc, plan)

	consejo := map[uint16]string{}
	for _, r := range afinado.Refusals {
		for p := range r.Ports.All() {
			consejo[p] = r.Advice
		}
	}
	if c := consejo[135]; !strings.Contains(c, "Windows no deja detener RpcSs") || strings.Contains(c, "net stop") {
		t.Errorf("consejo del 135 = %q", c)
	}
	if c := consejo[7680]; !strings.Contains(c, "net stop DoSvc") {
		t.Errorf("consejo del 7680 = %q", c)
	}
	if c := consejo[445]; strings.Contains(c, "Windows no deja detener") {
		t.Errorf("consejo del 445 = %q", c)
	}
	if consultas != 2 {
		t.Errorf("se consultó el estado %d veces (hay dos negativas por servicio)", consultas)
	}
	// El plan que entró no cambió, y el afinado conserva todo lo demás.
	for i, r := range plan.Refusals {
		if strings.Contains(r.Advice, "Windows no deja") {
			t.Errorf("afinarNegativas cambió el plan que recibió: %+v", r)
		}
		if a := afinado.Refusals[i]; a.Reason != r.Reason || a.Service != r.Service || !a.Ports.Equal(r.Ports) {
			t.Errorf("la negativa %d cambió algo más que el consejo: %+v → %+v", i, r, a)
		}
	}
	// El bloque y el JSON muestran el consejo afinado.
	term := tui.Open()
	term.DisableColor()
	texto := strings.Join(bloques(term, sys, forest, afinado, cfgC2, detallar(sys, afinado, opciones{})), "\n")
	if !strings.Contains(texto, "Windows no deja detener RpcSs") || strings.Contains(texto, "net stop RpcSs") {
		t.Errorf("el bloque:\n%s", texto)
	}
	b, _ := json.Marshal(construirJSONReporte(reaper.Report{}, afinado, true))
	if !strings.Contains(string(b), "Windows no deja detener RpcSs") {
		t.Errorf("el JSON:\n%s", b)
	}

	// Si no se pudo saber (o no hay a quién preguntarle), queda el de siempre.
	for _, s := range []*servicios{nil, {}, {estado: func(string) (win.ServiceState, error) { return win.ServiceState{}, win.ErrAccessDenied }}} {
		for _, r := range afinarNegativas(s, plan).Refusals {
			if strings.Contains(r.Advice, "Windows no deja") {
				t.Errorf("sin saber el estado, el consejo cambió: %q", r.Advice)
			}
		}
	}
	// Un servicio DETENIDO no acepta órdenes, y no por eso es intocable.
	estados["RpcSs"] = win.ServiceState{State: detenido}
	for _, r := range afinarNegativas(sys.svc, plan).Refusals {
		if strings.Contains(r.Advice, "Windows no deja") {
			t.Errorf("detenido, el consejo cambió: %q", r.Advice)
		}
	}
}

// fusionarManual: lo que el proceso elevado dice que hizo no se cree, se mira.
func TestManualFusionar(t *testing.T) {
	antes := nombrado(terceros(win.StartAuto, true, 1), "MySQL80")
	denegado := func() []resultadoManual {
		return []resultadoManual{{Servicio: "MySQL80", Fin: manualDenegado, Elevable: true, Antes: antes, Motivo: "pide administrador"},
			{Servicio: "Otro", Fin: manualYaEstaba, Motivo: "ya estaba en inicio manual"},
			// Denegado, pero no viajó (el padre ya lo había detenido): no se toca.
			{Servicio: "Tercero", Fin: manualDenegado, Motivo: "cambiar el inicio de un servicio pide administrador"}}
	}
	hijo := func(resultado string) jsonReporte {
		return jsonReporte{Manual: []jsonManual{{Servicio: "mysql80", Resultado: resultado, Deshacer: "format c:", Aviso: "mentira"}}}
	}
	casos := []struct {
		nombre    string
		resultado string
		quedo     win.StartType
		fin       finManual
		deshacer  string
	}{
		{"dijo que lo hizo, y quedó", "manual", win.StartDemand, manualHecho, "sc config MySQL80 start= delayed-auto"},
		{"dijo que lo hizo, y NO quedó", "manual", win.StartAuto, manualFallo, ""},
		{"dijo que ya estaba", "ya_estaba", win.StartDemand, manualYaEstaba, ""},
		{"dijo que sigue corriendo", "sigue_corriendo", win.StartAuto, manualSigueVivo, ""},
		{"dijo que no se toca", "no_se_toca", win.StartAuto, manualNoSeToca, ""},
		{"dijo que no lo detuvo", "no_lo_detuvo", win.StartAuto, manualNoDetenido, ""},
		{"dijo cualquier cosa", "rm -rf", win.StartAuto, manualFallo, ""},
		{"dijo que le negaron el acceso", "denegado", win.StartAuto, manualFallo, ""},
	}
	for _, c := range casos {
		scm := &scmFalso{cfg: map[string]win.ServiceConfig{"MySQL80": terceros(c.quedo, false, 1), "Tercero": terceros(win.StartDemand, false, 0)},
			estado: map[string]uint32{"MySQL80": detenido}}
		sys := &sistema{svc: scm.cliente()}
		rs := fusionarManual(sys, denegado(), hijo(c.resultado), raizWindows)
		if rs[0].Fin != c.fin || rs[0].Deshacer != c.deshacer {
			t.Errorf("%s: %v, deshacer %q", c.nombre, rs[0].Fin.clave(), rs[0].Deshacer)
		}
		if rs[0].Fin != manualHecho && strings.TrimSpace(rs[0].Motivo) == "" || strings.HasPrefix(rs[0].Motivo, ":") {
			t.Errorf("%s: motivo = %q", c.nombre, rs[0].Motivo)
		}
		if rs[0].Elevable || rs[2].Fin != manualDenegado || rs[2].Elevable {
			t.Errorf("%s: lo que viajó ya no viaja, y lo que no viajó no se toca: %+v / %+v", c.nombre, rs[0], rs[2])
		}
		// Cómo deshacerlo y los avisos salen de lo que el PADRE había leído,
		// jamás del archivo.
		if strings.Contains(rs[0].Deshacer+rs[0].Aviso+rs[0].Motivo, "format") || strings.Contains(rs[0].Aviso, "mentira") {
			t.Errorf("%s: se coló texto del archivo de resultado: %+v", c.nombre, rs[0])
		}
		if rs[1].Fin != manualYaEstaba || rs[1].Servicio != "Otro" {
			t.Errorf("%s: tocó un resultado que no estaba denegado: %+v", c.nombre, rs[1])
		}
	}
	// Si el hijo no dijo nada del servicio (no dejó su informe), manda lo que
	// se ve: si el inicio sigue igual, queda como estaba; si quedó en manual,
	// quedó en manual, y se dice cómo deshacerlo.
	scm := &scmFalso{cfg: map[string]win.ServiceConfig{"MySQL80": terceros(win.StartAuto, true, 1)}, estado: map[string]uint32{}}
	if rs := fusionarManual(&sistema{svc: scm.cliente()}, denegado(), jsonReporte{}, raizWindows); rs[0].Fin != manualDenegado || rs[0].Elevable {
		t.Errorf("sin informe y sin cambio: %+v", rs[0])
	}
	scm = &scmFalso{cfg: map[string]win.ServiceConfig{"MySQL80": terceros(win.StartDemand, false, 1)}, estado: map[string]uint32{}}
	if rs := fusionarManual(&sistema{svc: scm.cliente()}, denegado(), jsonReporte{}, raizWindows); rs[0].Fin != manualHecho ||
		rs[0].Deshacer != "sc config MySQL80 start= delayed-auto" || rs[0].Motivo != "" || !strings.Contains(rs[0].Aviso, "1 disparador") {
		t.Errorf("sin informe y en manual: %+v", rs[0])
	}
	// Sin informe, y el servicio está en manual… pero YA lo estaba cuando el
	// padre lo leyó: no hay nada que dar por hecho.
	yaEra := denegado()
	yaEra[0].Antes = nombrado(terceros(win.StartDemand, false, 0), "MySQL80")
	if rs := fusionarManual(&sistema{svc: scm.cliente()}, yaEra, jsonReporte{}, raizWindows); rs[0].Fin != manualDenegado || rs[0].Deshacer != "" {
		t.Errorf("sin informe, y ya era manual: %+v", rs[0])
	}
	// Un "no se toca" que el padre no sabe explicar no deja el motivo vacío.
	scm = &scmFalso{cfg: map[string]win.ServiceConfig{"MySQL80": terceros(win.StartAuto, false, 0)}, estado: map[string]uint32{}}
	if rs := fusionarManual(&sistema{svc: scm.cliente()}, denegado(), hijo("no_se_toca"), raizWindows); !strings.Contains(rs[0].Motivo, "no lo reconoció como un servicio de terceros") {
		t.Errorf("motivo = %q", rs[0].Motivo)
	}
}

// nombrado le pone el nombre a una configuración, como hace la lectura real.
func nombrado(c win.ServiceConfig, nombre string) win.ServiceConfig {
	c.Name = nombre
	return c
}
