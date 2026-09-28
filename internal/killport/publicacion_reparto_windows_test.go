//go:build windows

package killport

// Varios intermediarios en un mismo puerto: dos contenedores en el mismo puerto y distinta dirección (S-04 y C-10),
// y un puerto donde conviven la publicación de un contenedor y un proceso de
// Ubuntu (C-11).

import (
	"context"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/win/pipetest"
)

const (
	idA = "aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111"
	idB = "bbbbbbbbbbbb2222222222222222222222222222222222222222222222222222"
)

// dosContenedores8797: A publica 127.0.0.1:8797:80 y B 127.0.0.2:8797:80
// (lo que se armó en la máquina real de la revisión).
const dosContenedores8797 = `[
 {"Id": "` + idA + `", "Names": ["/kp-prueba-a"], "Image": "nginx:alpine", "State": "running",
  "Ports": [{"IP": "127.0.0.1", "PrivatePort": 80, "PublicPort": 8797, "Type": "tcp"}], "Labels": {}},
 {"Id": "` + idB + `", "Names": ["/kp-prueba-b"], "Image": "nginx:alpine", "State": "running",
  "Ports": [{"IP": "127.0.0.2", "PrivatePort": 80, "PublicPort": 8797, "Type": "tcp"}], "Labels": {}}
]`

// S-04 / C-10: `killport 127.0.0.1:8797` detiene SOLO al contenedor que
// publica esa dirección; el otro queda al margen. Antes cada contenedor se
// llevaba los dos sockets del motor y los dos eran objetivo.
func TestDosContenedoresMismoPuertoDistintaDireccion(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{})
	m.containers = []byte(dosContenedores8797)
	sys := sistemaFalso(t, m, &wslFalso{})
	socks := []ports.Socket{
		sock(ports.TCP, "127.0.0.1", 8797, ports.Listen, viaRelay.PID, ""),
		sock(ports.TCP, "127.0.0.2", 8797, ports.Listen, viaRelay.PID, ""),
	}
	for _, c := range []struct{ pedido, queda, margen string }{
		{"127.0.0.1:8797", "kp-prueba-a", "el contenedor kp-prueba-b"},
		{"127.0.0.2:8797", "kp-prueba-b", "el contenedor kp-prueba-a"},
	} {
		req, err := ports.ParseRequest([]string{c.pedido})
		if err != nil {
			t.Fatal(err)
		}
		plan := desenmascarar(context.Background(), sys, planDe(socks, req.Ports), socks, bosqueDocker(), opciones{})
		if len(plan.Targets) != 2 {
			t.Fatalf("%s: sin enfocar, los dos contenedores: %+v", c.pedido, plan.Targets)
		}
		for _, tg := range plan.Targets {
			if len(tg.Sockets) != 1 {
				t.Fatalf("%s: %s se llevó %d sockets (uno por dirección)", c.pedido, tg.External.Name, len(tg.Sockets))
			}
		}
		plan = reaper.Focus(plan, req, nil)
		if len(plan.Targets) != 1 || plan.Targets[0].External.Name != c.queda || len(plan.Bystanders) != 1 || plan.Bystanders[0].Name != c.margen {
			t.Fatalf("%s: objetivos %+v, al margen %+v", c.pedido, plan.Targets, plan.Bystanders)
		}
		// La nota del puerto compartido dice la verdad: dos servidores (no
		// dos procesos: los dos sockets son del motor), cada uno en lo suyo.
		cs := compartidos(ocupantesDelPlan(plan))
		if len(cs) != 1 {
			t.Fatalf("%s: compartidos = %+v", c.pedido, cs)
		}
		titulo, detalle := cs[0].lineas()
		if !strings.Contains(titulo, "2 servidores distintos") || !strings.Contains(strings.Join(detalle, "\n"), "el contenedor kp-prueba-a en 127.0.0.1") ||
			strings.Contains(strings.Join(detalle, "\n"), "127.0.0.1 127.0.0.2") {
			t.Fatalf("%s: nota = %q / %q", c.pedido, titulo, detalle)
		}
	}
	// El listado: una fila por contenedor, cada una con su dirección.
	filas := fundirPublicaciones(filasDeListado(socks, bosqueDocker(), nil, reaper.Config{SystemRoot: `C:\Windows`}))
	anotadas, _, _ := anotarListado(context.Background(), sys, filas, bosqueDocker(), opciones{})
	if len(anotadas) != 2 {
		t.Fatalf("filas = %+v", anotadas)
	}
	for _, f := range anotadas {
		want := map[string]string{"kp-prueba-a": "127.0.0.1", "kp-prueba-b": "127.0.0.2"}[f.Contenedor.Nombre]
		if got := direccionFila(f); got != want {
			t.Fatalf("la fila de %s dice %q, esperaba %q", f.Contenedor.Nombre, got, want)
		}
	}
}

// El reparto por dirección, puro: exacta, comodín de la familia, comodín de
// la otra (el [::] del motor y el [::1] de wslrelay siguen a una publicación
// en 0.0.0.0), y si nadie, todos.
func TestRepartirPublicacion(t *testing.T) {
	pub := func(id, ip string) contenedor {
		return contenedor{ID: id, Nombre: id, Puertos: []publicado{{IP: ip, Publico: 8080, Privado: 80, Proto: ports.TCP}}}
	}
	todo, local := pub("todo", "0.0.0.0"), pub("local", "127.0.0.1")
	m := reaper.MaskedPort{Port: 8080, Proto: ports.TCP, Sockets: []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 8080, ports.Listen, 1, ""),
		sock(ports.TCP, "::", 8080, ports.Listen, 1, ""),
		sock(ports.TCP, "127.0.0.1", 8080, ports.Listen, 1, ""),
		sock(ports.TCP, "::1", 8080, ports.Listen, 2, ""),
	}}
	partes := repartirPublicacion(m, []contenedor{todo, local})
	got := map[string][]string{}
	for _, p := range partes {
		var ids []string
		for _, c := range p.conts {
			ids = append(ids, c.ID)
		}
		for _, s := range p.socks {
			got[s.Local.Addr().String()] = ids
		}
	}
	want := map[string]string{"0.0.0.0": "todo", "::": "todo", "127.0.0.1": "local", "::1": "todo"}
	for addr, id := range want {
		if strings.Join(got[addr], ",") != id {
			t.Errorf("%s → %v, esperaba %s", addr, got[addr], id)
		}
	}
	// Nadie publica esa dirección ni un comodín: todos (no queda sin dueño).
	raro := reaper.MaskedPort{Port: 8080, Proto: ports.TCP, Sockets: []ports.Socket{sock(ports.TCP, "10.0.0.5", 8080, ports.Listen, 1, "")}}
	if p := repartirPublicacion(raro, []contenedor{local, pub("otro", "127.0.0.2")}); len(p) != 1 || len(p[0].conts) != 2 {
		t.Fatalf("partes = %+v", p)
	}
}

// C-11: en el 8782 conviven un proceso de Ubuntu en 0.0.0.0 (wslrelay lo
// reenvía en 127.0.0.1) y un contenedor publicado (el motor en 0.0.0.0/[::]
// y wslrelay en [::1]). El [::1] es de la publicación; el 127.0.0.1 va a WSL:
// se mira adentro y el proceso de Ubuntu es objetivo junto al contenedor.
// Antes el reenvío de la distro se atribuía al contenedor.
func TestRepartirWSLPorSocket(t *testing.T) {
	loop4 := sock(ports.TCP, "127.0.0.1", 8782, ports.Listen, viaWSL.PID, "")
	loop6 := sock(ports.TCP, "::1", 8782, ports.Listen, viaWSL.PID, "")
	enWSL := []reaper.MaskedPort{{Via: viaWSL, Name: "wslrelay.exe", Relay: reaper.RelayWSL, Port: 8782, Proto: ports.TCP, Sockets: []ports.Socket{loop4, loop6}}}
	cont := []reaper.MaskedPort{{Via: viaRelay, Name: "com.docker.backend.exe", Relay: reaper.RelayContainer, Port: 8782, Proto: ports.TCP,
		Sockets: []ports.Socket{sock(ports.TCP, "0.0.0.0", 8782, ports.Listen, viaRelay.PID, ""), sock(ports.TCP, "::", 8782, ports.Listen, viaRelay.PID, "")}}}
	delMotor, deWSL := repartirWSL(enWSL, cont)
	if len(delMotor) != 1 || len(delMotor[0].Sockets) != 1 || delMotor[0].Sockets[0] != loop6 {
		t.Fatalf("del motor = %+v", delMotor)
	}
	if len(deWSL) != 1 || len(deWSL[0].Sockets) != 1 || deWSL[0].Sockets[0] != loop4 {
		t.Fatalf("de WSL = %+v", deWSL)
	}

	// De punta a punta: Ubuntu tiene un python en 0.0.0.0:8782; el motor
	// publica shop-db en el mismo puerto.
	m := nuevoMotorFalso(t, pipetest.Options{})
	m.containers = []byte(strings.ReplaceAll(string(grabado(t, "docker_containers.json")), `"PublicPort": 5432`, `"PublicPort": 8782`))
	ubuntu := sondeoDe([]string{`tcp LISTEN 0 5 0.0.0.0:8782 0.0.0.0:* users:(("python3",pid=661,fd=3))`},
		[]string{filaNet(8782, 4242)}, procDe(661, "python3", "1000", "mauro", "python3 -m http.server 8782", "4242"))
	sys := sistemaFalso(t, m, &wslFalso{running: []string{"Ubuntu", "docker-desktop"}, sondeo: map[string]string{"Ubuntu": ubuntu}})
	socks := append(append([]ports.Socket(nil), cont[0].Sockets...), loop4, loop6)
	plan := desenmascarar(context.Background(), sys, planDe(socks, ports.Of(8782)), socks, bosqueDocker(), opciones{})
	var db, py *reaper.Target
	for i := range plan.Targets {
		switch tg := &plan.Targets[i]; tg.External.Kind {
		case reaper.ExtContainer:
			db = tg
		case reaper.ExtLinux:
			py = tg
		}
	}
	if db == nil || py == nil || len(plan.Refusals) != 0 {
		t.Fatalf("plan = %+v / negativas %+v", plan.Targets, plan.Refusals)
	}
	if len(db.Sockets) != 3 || len(py.Sockets) != 1 || py.Sockets[0] != loop4 || py.External.Name != "python3" {
		t.Fatalf("contenedor %+v · python %+v", db.Sockets, py)
	}

	// Y el listado deja la fila del 127.0.0.1 como reenvío de WSL (no la
	// funde en la del motor).
	filas := fundirPublicaciones(filasDeListado(socks, bosqueDocker(), nil, reaper.Config{SystemRoot: `C:\Windows`}))
	wslFila := false
	for _, f := range filas {
		if f.Relay == reaper.RelayWSL {
			wslFila = len(f.socks) == 1 && f.socks[0] == loop4
		}
	}
	if len(filas) != 2 || !wslFila {
		t.Fatalf("filas = %+v", filas)
	}
}
