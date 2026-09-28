//go:build windows

package killport

// Un puerto, dos intermediarios. Docker Desktop con WSL 2 publica cada puerto
// con com.docker.backend (0.0.0.0 y [::]) y además con wslrelay ([::1]): tres
// sockets de dos dueños para un mismo contenedor. Visto en la máquina real:
//
//	[::]:64824    PID 30860  com.docker.backend
//	[::1]:64824   PID 4464   wslrelay
//
// Antes de esta corrección el socket de wslrelay se trataba como un reenvío
// de WSL suelto: fila aparte en el listado, "relay colgado" con --wsl, y con
// --no-docker el consejo de correr `wsl --shutdown`, que apaga también la VM
// de Docker.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win/pipetest"
)

// bosqueDocker: Docker Desktop (8124) y el wslrelay de su VM (9120).
func bosqueDocker() *reaper.Forest {
	return reaper.NewForest([]reaper.Proc{
		{ID: viaRelay, Name: "com.docker.backend.exe", Path: `C:\Program Files\Docker\Docker\resources\com.docker.backend.exe`},
		{ID: viaWSL, Name: "wslrelay.exe", Path: `C:\Program Files\WSL\wslrelay.exe`},
	})
}

// publicado5432 son los tres sockets del 5432 publicado por Docker Desktop.
func publicado5432() []ports.Socket {
	return []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 5432, ports.Listen, viaRelay.PID, ""),
		sock(ports.TCP, "::", 5432, ports.Listen, viaRelay.PID, ""),
		sock(ports.TCP, "::1", 5432, ports.Listen, viaWSL.PID, ""),
	}
}

func planDe(socks []ports.Socket, pedidos ports.Set) reaper.Plan {
	return reaper.PlanKill(pedidos, holding(socks, ports.Filter{}, pedidos), bosqueDocker(), nil, reaper.Config{SystemRoot: `C:\Windows`})
}

// El reparto: lo de wslrelay que comparte puerto Y protocolo con el motor es
// del motor; lo demás es de WSL.
func TestRepartirWSL(t *testing.T) {
	m := func(relay reaper.Relay, via reaper.ID, port uint16, proto ports.Proto) reaper.MaskedPort {
		return reaper.MaskedPort{Via: via, Relay: relay, Port: port, Proto: proto}
	}
	cont := []reaper.MaskedPort{m(reaper.RelayContainer, viaRelay, 5432, ports.TCP), m(reaper.RelayContainer, viaRelay, 5353, ports.UDP)}
	enWSL := []reaper.MaskedPort{
		m(reaper.RelayWSL, viaWSL, 3000, ports.TCP), // un node de Ubuntu
		m(reaper.RelayWSL, viaWSL, 5432, ports.TCP), // el [::1] de la publicación
		m(reaper.RelayWSL, viaWSL, 5353, ports.TCP), // mismo puerto, OTRO protocolo: no es la publicación
		m(reaper.RelayWSL, viaWSL, 5353, ports.UDP),
	}
	delMotor, deWSL := repartirWSL(enWSL, cont)
	puertos := func(ms []reaper.MaskedPort) string {
		var out []string
		for _, x := range ms {
			out = append(out, protoPuerto(x.Proto, x.Port))
		}
		return strings.Join(out, " ")
	}
	if got := puertos(delMotor); got != "TCP/5432 UDP/5353" {
		t.Fatalf("del motor = %q", got)
	}
	if got := puertos(deWSL); got != "TCP/3000 TCP/5353" {
		t.Fatalf("de WSL = %q", got)
	}
	if a, b := repartirWSL(enWSL, nil); len(a) != 0 || len(b) != len(enWSL) {
		t.Fatal("sin relay de contenedores, todo lo de wslrelay es de WSL")
	}
}

// Lo que contesta el motor vale para el socket de wslrelay del mismo puerto,
// con wslrelay como intermediario; y jamás se lo declara cerrable. Desde el
// reparto por socket (C-11), el motor contesta directamente por esos
// sockets (docker.desenmascarar); seguirAlMotor queda para --no-docker.
func TestSeguirAlMotor(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{})
	d := dockerDe(m.path)
	cont := []reaper.MaskedPort{
		{Via: viaRelay, Name: "com.docker.backend.exe", Relay: reaper.RelayContainer, Port: 5432, Proto: ports.TCP},
		{Via: viaRelay, Name: "com.docker.backend.exe", Relay: reaper.RelayContainer, Port: 8888, Proto: ports.TCP},
	}
	loop6 := sock(ports.TCP, "::1", 5432, ports.Listen, viaWSL.PID, "")
	delMotor := []reaper.MaskedPort{
		{Via: viaWSL, Name: "wslrelay.exe", Relay: reaper.RelayWSL, Port: 5432, Proto: ports.TCP, Sockets: []ports.Socket{loop6}},
		{Via: viaWSL, Name: "wslrelay.exe", Relay: reaper.RelayWSL, Port: 8888, Proto: ports.TCP},
	}
	got := d.desenmascarar(context.Background(), delMotor, bosqueDocker())
	if len(got) != 2 {
		t.Fatalf("respuestas = %+v", got)
	}
	for i, u := range got {
		if u.Via != viaWSL || u.Port != delMotor[i].Port || u.Proto != ports.TCP || u.KillVia {
			t.Fatalf("respuesta %d = %+v: el intermediario es wslrelay, y nunca es cerrable", i, u)
		}
	}
	if u := got[0]; u.Verdict != reaper.UnmaskFound || len(u.Behind) != 1 || u.Behind[0].Key != idDB || len(u.Sockets) != 1 || u.Sockets[0] != loop6 {
		t.Fatalf("5432 = %+v: el [::1] de wslrelay es de shop-db-1", u)
	}
	if u := got[1]; u.Verdict != reaper.UnmaskEmpty {
		t.Fatalf("8888 = %+v: ningún contenedor lo publica", u)
	}
	// Sin preguntarle al motor (--no-docker): negado con el motivo de verdad.
	got = seguirAlMotor(delMotor[:1], cont)
	if len(got) != 1 || got[0].Verdict != reaper.UnmaskUnavailable || got[0].KillVia || got[0].Via != viaWSL || len(got[0].Sockets) != 1 ||
		!strings.Contains(got[0].Reason, "com.docker.backend.exe") || !strings.Contains(got[0].Reason, "es parte de esa publicación") {
		t.Fatalf("sin el motor = %+v", got)
	}
	if strings.Contains(got[0].Advice, "wsl --shutdown") || !strings.Contains(got[0].Advice, "docker stop") {
		t.Fatalf("el consejo es detener el contenedor, no apagar WSL: %q", got[0].Advice)
	}
	if seguirAlMotor(nil, cont) != nil {
		t.Fatal("sin nada que seguir, nada que contestar")
	}
}

// De punta a punta con el motor falso: los tres sockets son UN objetivo (el
// contenedor), con el motor como Via; y a WSL NO se le pregunta nada (ni la
// lista de distros): ese socket no es un reenvío del usuario.
func TestDesenmascararPublicacionCompartida(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{})
	f := &wslFalso{running: []string{"docker-desktop"}}
	sys := sistemaFalso(t, m, f)
	plan := planDe(publicado5432(), ports.Of(5432))
	if len(plan.Refusals) != 2 || len(plan.Targets) != 0 {
		t.Fatalf("sin desenmascarar son dos negativas: %+v", plan)
	}
	plan = desenmascarar(context.Background(), sys, plan, publicado5432(), bosqueDocker(), opciones{})
	if len(plan.Targets) != 1 || len(plan.Refusals) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	tg := plan.Targets[0]
	if tg.Kind != reaper.StopExternal || tg.External.Key != idDB || len(tg.Sockets) != 3 || len(tg.Victims) != 0 {
		t.Fatalf("objetivo = %+v", tg)
	}
	if tg.Via != viaRelay || tg.ViaClass != reaper.RelayContainer {
		t.Fatalf("el Via es el motor: %v %v", tg.ViaName, tg.ViaClass)
	}
	if n := len(f.llamadas); n != 0 {
		t.Fatalf("no había nada que mirar dentro de WSL y se llamó %d veces a wsl.exe: %q", n, f.llamadas)
	}
	// El bloque muestra un solo puerto, en las dos familias (los comodines del
	// motor cubren el [::1] de wslrelay).
	got := plano(bloqueExterno(tui.Open(), tg, t0, 0))
	if !contiene(got, "5432 TCP escuchando en ∗ v4+v6 · toda la red") || !contiene(got, "Windows lo ve como com.docker.backend.exe") {
		t.Fatalf("bloque =\n%s", strings.Join(got, "\n"))
	}
}

// Con --no-docker los dos quedan negados, con motivos coherentes, sin mirar
// dentro de WSL y SIN aconsejar `wsl --shutdown` (apagaría la VM de Docker).
func TestDesenmascararPublicacionSinDocker(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{})
	f := &wslFalso{running: []string{"docker-desktop"}}
	sys := sistemaFalso(t, m, f)
	plan := desenmascarar(context.Background(), sys, planDe(publicado5432(), ports.Of(5432)), publicado5432(), bosqueDocker(), opciones{noDocker: true})
	if len(plan.Targets) != 0 || len(plan.Refusals) != 2 {
		t.Fatalf("plan = %+v", plan)
	}
	for _, r := range plan.Refusals {
		if strings.Contains(r.Advice, "wsl --shutdown") || strings.Contains(r.Reason, "relay colgado") {
			t.Fatalf("negativa de %s = %q / %q", r.Name, r.Reason, r.Advice)
		}
		if r.Name == "wslrelay.exe" && !strings.Contains(r.Reason, "es parte de esa publicación") {
			t.Fatalf("negativa de wslrelay = %q", r.Reason)
		}
	}
	if n := len(f.llamadas); n != 0 {
		t.Fatalf("se llamó %d veces a wsl.exe: %q", n, f.llamadas)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.stops) != 0 {
		t.Fatalf("--no-docker y se le habló al motor: %v", m.stops)
	}
}

// Pendiente 1 con --no-docker: lo que se MUESTRA es una sola publicación. Un
// bloque (el motor y wslrelay juntos), "no se toca" una vez por línea, sin la
// falsa alarma de "lo escuchan 2 procesos distintos" (que mandaba a apuntar a
// localhost, otra negativa), y el consejo dice que sin --no-docker se detiene.
func TestPublicacionSinDockerUnBloque(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{})
	sys := sistemaFalso(t, m, &wslFalso{running: []string{"docker-desktop"}})
	plan := desenmascarar(context.Background(), sys, planDe(publicado5432(), ports.Of(5432)), publicado5432(), bosqueDocker(), opciones{noDocker: true})
	plan = reaper.Focus(plan, ports.Whole(ports.Of(5432)), nil)
	got := plano(bloques(tui.Open(), sys, bosqueDocker(), plan, reaper.Config{SystemRoot: `C:\Windows`}, detalle{}))
	texto := strings.Join(got, "\n")
	if n := strings.Count(texto, "● 5432"); n != 1 || !strings.Contains(texto, "com.docker.backend.exe y wslrelay.exe en [::1]") {
		t.Fatalf("bloques (%d) =\n%s", n, texto)
	}
	for _, l := range got {
		if strings.Count(l, "no se toca") > 1 {
			t.Fatalf("'no se toca' dos veces en una línea: %q", l)
		}
	}
	if strings.Contains(texto, "procesos distintos") || strings.Contains(texto, "apuntan solo") {
		t.Fatalf("falsa alarma de puerto compartido:\n%s", texto)
	}
	if !strings.Contains(texto, "sin --no-docker, killport detiene el contenedor") {
		t.Fatalf("el consejo no dice que sin --no-docker se detiene:\n%s", texto)
	}
	// El plan sigue teniendo las dos negativas (la verificación las necesita).
	if len(plan.Refusals) != 2 {
		t.Fatalf("negativas del plan = %d", len(plan.Refusals))
	}
}

// Pendiente 1: `wsl --shutdown` nunca se aconseja sin decir que apaga Docker.
// Si ni siquiera se pudo saber qué distros corren, Docker Desktop puede estar
// entre ellas: se avisa igual.
func TestConsejoApagarAvisaDockerSiNoSeSabe(t *testing.T) {
	f := &wslFalso{listErr: errors.New("wsl.exe no contesta")}
	res := (&wsl{run: f.run}).desenmascarar(context.Background(), []reaper.MaskedPort{mascaraWSL(3000)}, socketsDelRelay(3000), reaper.NewForest(nil), nil, true)
	if u := res[0]; u.Verdict != reaper.UnmaskUnavailable || !strings.Contains(u.Advice, "wsl --shutdown") || !strings.Contains(u.Advice, "Docker Desktop") {
		t.Fatalf("respuesta = %+v", u)
	}
}

// Mezcla: el mismo wslrelay tiene el [::1] de la publicación Y el 3000 de un
// node de Ubuntu. Cada uno a su dominio: dos objetivos, y a WSL se le pregunta
// solo por el 3000.
func TestDesenmascararPublicacionYProcesoLinux(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{})
	f := &wslFalso{running: []string{"Ubuntu", "docker-desktop"}, sondeo: map[string]string{
		"Ubuntu":         string(grabado(t, "wsl_sondeo_ss.txt")),
		"docker-desktop": "@navaja 1\n@ss\ntcp LISTEN 0 4096 *:5432 *:*\n@clock 1790400000 100\n@fin\n",
	}}
	sys := sistemaFalso(t, m, f)
	socks := append(publicado5432(), sock(ports.TCP, "127.0.0.1", 3000, ports.Listen, viaWSL.PID, ""))
	plan := desenmascarar(context.Background(), sys, planDe(socks, ports.Of(3000, 5432)), socks, bosqueDocker(), opciones{})
	if len(plan.Targets) != 2 || len(plan.Refusals) != 0 {
		t.Fatalf("plan = %+v", plan)
	}
	node, db := plan.Targets[0], plan.Targets[1]
	if node.External.Kind != reaper.ExtLinux || node.External.Name != "node" || node.ViaClass != reaper.RelayWSL || !node.Ports.Equal(ports.Of(3000)) {
		t.Fatalf("3000 = %+v", node)
	}
	if db.External.Kind != reaper.ExtContainer || db.ViaClass != reaper.RelayContainer || len(db.Sockets) != 3 {
		t.Fatalf("5432 = %+v", db)
	}
}

// Con el motor de contenedores corriendo en WSL, un relay sin nadie detrás NO
// se ofrece cerrar: el motor publica por ese mismo relay. Y el último recurso
// avisa lo que cuesta.
func TestWSLZombiConMotorCorriendo(t *testing.T) {
	vacio := "@navaja 1\n@ss\n@clock 1790400000 100\n@fin\n"
	f := &wslFalso{running: []string{"docker-desktop"}, sondeo: map[string]string{"docker-desktop": vacio}}
	w := &wsl{run: f.run}
	res := w.desenmascarar(context.Background(), []reaper.MaskedPort{mascaraWSL(3000)}, socketsDelRelay(3000), reaper.NewForest(nil), nil, true)
	u := res[0]
	if u.Verdict != reaper.UnmaskEmpty || u.KillVia {
		t.Fatalf("con el motor corriendo el relay no es cerrable: %+v", u)
	}
	if !strings.Contains(u.Reason, "relay colgado") || !strings.Contains(u.Advice, "docker-desktop") || !strings.Contains(u.Advice, "apaga TODO") {
		t.Fatalf("motivo/consejo = %q / %q", u.Reason, u.Advice)
	}
	// Sin el motor (solo Ubuntu, que tampoco escucha el 5555): cerrable, como siempre.
	f = &wslFalso{running: []string{"Ubuntu"}, sondeo: map[string]string{"Ubuntu": vacio}}
	res = (&wsl{run: f.run}).desenmascarar(context.Background(), []reaper.MaskedPort{mascaraWSL(5555)}, socketsDelRelay(5555), reaper.NewForest(nil), nil, true)
	if u = res[0]; u.Verdict != reaper.UnmaskEmpty || !u.KillVia || strings.Contains(u.Advice, "motor") {
		t.Fatalf("sin motor = %+v", u)
	}
}

// Qué distros son de un motor de contenedores, y qué procesos Linux.
func TestDistroYProcesoDelMotor(t *testing.T) {
	for d, want := range map[string]bool{
		"docker-desktop": true, "docker-desktop-data": true, "Docker-Desktop": true, "rancher-desktop": true,
		"podman-machine-default": true, "Ubuntu": false, "Debian": false, "docker": false, "": false,
	} {
		if got := distroDeMotor(d); got != want {
			t.Errorf("distroDeMotor(%q) = %v", d, got)
		}
	}
	for _, tc := range []struct {
		p    procesoLinux
		want bool
	}{
		{procesoLinux{Distro: "Ubuntu", Comm: "node"}, false},
		{procesoLinux{Distro: "Ubuntu", Comm: "docker-proxy"}, true},
		{procesoLinux{Distro: "docker-desktop", Comm: "node"}, true},
	} {
		if got := tc.p.esDelMotor(); got != tc.want {
			t.Errorf("%s en %s: esDelMotor = %v", tc.p.Comm, tc.p.Distro, got)
		}
	}
}

// El listado: UNA fila por puerto publicado. La de wslrelay se funde en la
// del motor (que suma su familia), venga antes o después; las demás filas de
// wslrelay quedan como estaban, y sin relay de contenedores no cambia nada.
func TestFundirPublicaciones(t *testing.T) {
	cfg := reaper.Config{SystemRoot: `C:\Windows`}
	solo6 := []ports.Socket{
		sock(ports.TCP, "::1", 5432, ports.Listen, viaWSL.PID, ""),       // llega ANTES que la del motor
		sock(ports.TCP, "0.0.0.0", 5432, ports.Listen, viaRelay.PID, ""), // el motor, solo v4
		sock(ports.TCP, "127.0.0.1", 3000, ports.Listen, viaWSL.PID, ""),
		sock(ports.UDP, "::1", 5432, ports.StateNone, viaWSL.PID, ""), // mismo puerto, otro protocolo
	}
	antes := filasDeListado(solo6, bosqueDocker(), nil, cfg)
	if len(antes) != 4 {
		t.Fatalf("filas sin fundir = %d", len(antes))
	}
	filas := fundirPublicaciones(antes)
	if len(filas) != 3 {
		t.Fatalf("filas = %+v", filas)
	}
	var motor, node, udp *fila
	for i := range filas {
		switch f := &filas[i]; {
		case f.Port == 5432 && f.Proto == ports.TCP:
			motor = f
		case f.Port == 3000:
			node = f
		case f.Port == 5432 && f.Proto == ports.UDP:
			udp = f
		}
	}
	if motor == nil || motor.Relay != reaper.RelayContainer || motor.PID != viaRelay.PID || !motor.Familias.v4 || !motor.Familias.v6 {
		t.Fatalf("la fila del 5432 = %+v: es la del motor, con las dos familias", motor)
	}
	if node == nil || node.Relay != reaper.RelayWSL || udp == nil || udp.Relay != reaper.RelayWSL {
		t.Fatalf("las otras filas de wslrelay no se tocan: %+v / %+v", node, udp)
	}
	// Sin relay de contenedores: igual que entró.
	soloWSL := filasDeListado(solo6[2:], bosqueDocker(), nil, cfg)
	if got := fundirPublicaciones(soloWSL); len(got) != len(soloWSL) {
		t.Fatalf("sin motor no se funde nada: %+v", got)
	}
}

// El listado anotado con --wsl: el puerto publicado es una sola fila con su
// contenedor (nunca "relay colgado"), y lo que corre en la distro del motor
// no aparece como "solo dentro de WSL".
func TestAnotarListadoPublicacion(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{})
	f := &wslFalso{running: []string{"docker-desktop"}, sondeo: map[string]string{
		"docker-desktop": "@navaja 1\n@ss\ntcp LISTEN 0 4096 *:5432 *:*\ntcp LISTEN 0 16 127.0.0.1:2375 0.0.0.0:* users:((\"dockerd\",pid=90,fd=7))\n" +
			"@clock 1790400000 100\n@pid 90\nS 90 (dockerd) S 1 90 90 0 -1 4194560 900 0 0 0 3 1 0 0 20 0 7 0 700 1500000000 3000 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 2 0 0 0 0 0 0 0 0 0 0 0 0 0\nU 0 root\nC dockerd\n@fin\n",
	}}
	socks := append(publicado5432(), sock(ports.TCP, "::1", 7777, ports.Listen, viaWSL.PID, ""))
	filas := fundirPublicaciones(filasDeListado(socks, bosqueDocker(), nil, reaper.Config{SystemRoot: `C:\Windows`}))
	for _, o := range []opciones{{}, {wsl: true}} {
		sys := sistemaFalso(t, m, f)
		anotadas, solo, _ := anotarListado(context.Background(), sys, append([]fila(nil), filas...), bosqueDocker(), o)
		var del5432 []fila
		for _, fl := range anotadas {
			if fl.Port == 5432 {
				del5432 = append(del5432, fl)
			}
			if strings.Contains(fl.Comando, "relay colgado") && fl.Port == 5432 {
				t.Fatalf("--wsl=%v: el puerto publicado figura como relay colgado: %+v", o.wsl, fl)
			}
		}
		if len(del5432) != 1 || del5432[0].Contenedor == nil || nombreFila(del5432[0]) != "shop-db-1" || del5432[0].Familias.etiqueta() != "∗ v4+v6" {
			t.Fatalf("--wsl=%v: filas del 5432 = %+v", o.wsl, del5432)
		}
		if len(solo) != 0 {
			t.Fatalf("--wsl=%v: lo de la distro del motor no es del usuario: %+v", o.wsl, solo)
		}
	}
}
