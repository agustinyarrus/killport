//go:build windows

package killport

// La abstracción "objetivo que no es un proceso de Windows" del lado de la
// herramienta (externo.go): el adaptador, los bloques, la narración, la
// tarjeta y el JSON no conocen los dominios por nombre. Se prueba con un
// dominio DE MENTIRA que no es ni contenedor ni proceso Linux —una regla de
// reenvío, como las que suma la próxima tanda—: si esto anda sin tocar una
// línea del adaptador, un dominio nuevo se enchufa escribiendo solo su tipo.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

// extRegla es una clase que el vocabulario no conoce.
const extRegla = reaper.ExtKind(200)

// reglaFalsa es un externo de un dominio nuevo.
type reglaFalsa struct {
	pedidos  atomic.Int32
	force    atomic.Bool
	deadline atomic.Int64
	fin      reaper.ExternalStop
	sinFin   bool // se cancela antes de saber cómo terminó
}

func (r *reglaFalsa) detener(ctx context.Context, s *sistema, deadline time.Time, force bool) (reaper.ExternalStop, bool) {
	r.pedidos.Add(1)
	r.force.Store(force)
	r.deadline.Store(deadline.UnixNano())
	return r.fin, !r.sinFin
}

func (r *reglaFalsa) presentar(t *tui.Term, tg reaper.Target, _ time.Time) (string, []string) {
	return "regla de reenvío " + tg.External.Name, []string{"conecta a 10.0.0.5:80"}
}

func (r *reglaFalsa) anotar(o *jsonObjetivo) { o.Servicio = "iphlpsvc" }

func externoRegla(r *reglaFalsa) reaper.External {
	return reaper.External{Kind: extRegla, Where: "portproxy", Key: "v4tov4/tcp/0.0.0.0:8080", Name: "0.0.0.0:8080", Ports: ports.Of(8080), Ref: r}
}

// plano quita el margen y los espacios de sobra de las líneas de un bloque.
func plano(lines []string) []string {
	var out []string
	for _, l := range lines {
		if s := strings.TrimSpace(l); s != "" {
			out = append(out, strings.Join(strings.Fields(s), " "))
		}
	}
	return out
}

func contiene(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

// Un dominio nuevo se detiene por el adaptador de siempre: StopExternal no
// sabe qué es (el sistema de la prueba ni siquiera tiene cliente de Docker ni
// de WSL), le pasa el plazo y el --force, y entrega su veredicto.
func TestExternoDominioNuevoSeDetiene(t *testing.T) {
	sys := &sistema{}
	r := &reglaFalsa{fin: reaper.ExternalStop{Outcome: reaper.OutcomeClosed, Method: reaper.MethodEngine}}
	deadline := time.Now().Add(3 * time.Second)
	var got []reaper.ExternalStop
	sys.StopExternal(context.Background(), []reaper.External{externoRegla(r)}, deadline, true, func(i int, v reaper.ExternalStop) {
		if i != 0 {
			t.Errorf("índice %d", i)
		}
		got = append(got, v)
	})
	if len(got) != 1 || got[0].Outcome != reaper.OutcomeClosed || got[0].Method != reaper.MethodEngine {
		t.Fatalf("veredictos = %+v", got)
	}
	if r.pedidos.Load() != 1 || !r.force.Load() || r.deadline.Load() != deadline.UnixNano() {
		t.Fatalf("al dominio le llegó pedidos=%d force=%v: tiene que recibir el plazo y el --force tal cual", r.pedidos.Load(), r.force.Load())
	}
}

// Sin veredicto (se canceló adentro del dominio) el motor no recibe nada: lo
// deja pendiente. Y un externo cuyo Ref no sabe detenerse es un error con
// veredicto, no un silencio ni un pánico.
func TestExternoSinVeredictoYSinDominio(t *testing.T) {
	sys := &sistema{}
	r := &reglaFalsa{sinFin: true}
	llamado := false
	sys.StopExternal(context.Background(), []reaper.External{externoRegla(r)}, time.Now().Add(time.Second), false,
		func(int, reaper.ExternalStop) { llamado = true })
	if llamado || r.pedidos.Load() != 1 {
		t.Fatalf("sin veredicto no se llama a done (llamado=%v, pedidos=%d)", llamado, r.pedidos.Load())
	}

	var got []reaper.ExternalStop
	sys.StopExternal(context.Background(), []reaper.External{{Kind: extRegla, Name: "huérfano"}, {Kind: reaper.ExtContainer, Name: "otro", Ref: 42}},
		time.Now().Add(time.Second), false, func(_ int, v reaper.ExternalStop) { got = append(got, v) })
	if len(got) != 2 {
		t.Fatalf("veredictos = %+v", got)
	}
	for _, v := range got {
		if v.Outcome != reaper.OutcomeError || !errors.Is(v.Err, errSinDominio) {
			t.Fatalf("un Ref que no sabe detenerse = %+v", v)
		}
	}
}

// El vocabulario: cada clase conocida tiene el suyo, y una desconocida cae en
// el genérico sin quedar muda.
func TestDominioVocabulario(t *testing.T) {
	db := reaper.External{Kind: reaper.ExtContainer, Where: "Docker Desktop", Name: "db-1"}
	node := reaper.External{Kind: reaper.ExtLinux, Where: "Ubuntu", Name: "node"}
	regla := externoRegla(&reglaFalsa{})
	for _, tc := range []struct {
		x                    reaper.External
		tipo, verbo, nombre  string
		pedido, pedidoEnSeco string
	}{
		{db, "contenedor", "Detener", "el contenedor db-1", "Deteniendo el contenedor db-1", "Deteniendo el contenedor db-1|en seco: sin esperar a que cierre"},
		{node, "linux", "Cerrar", "node en Ubuntu", "SIGINT a node en Ubuntu|si no cede: SIGTERM y después SIGKILL", "SIGKILL a node en Ubuntu"},
		{regla, "externo", "Detener", "0.0.0.0:8080", "Deteniendo 0.0.0.0:8080", "Deteniendo 0.0.0.0:8080"},
	} {
		d := dominioDe(tc.x.Kind)
		if d.tipo != tc.tipo || tipoExterno(tc.x.Kind) != tc.tipo || d.verbo != tc.verbo || nombreExterno(tc.x) != tc.nombre {
			t.Errorf("%s: tipo %q verbo %q nombre %q", tc.nombre, d.tipo, d.verbo, nombreExterno(tc.x))
		}
		junta := func(enSeco bool) string {
			a, b := d.pedido(nombreExterno(tc.x), enSeco)
			return strings.TrimSuffix(a+"|"+b, "|")
		}
		if got := junta(false); got != tc.pedido {
			t.Errorf("%s: pedido = %q, esperaba %q", tc.nombre, got, tc.pedido)
		}
		if got := junta(true); got != tc.pedidoEnSeco {
			t.Errorf("%s: pedido en seco = %q, esperaba %q", tc.nombre, got, tc.pedidoEnSeco)
		}
		if d.tarjeta == "" || !d.punto.IsSet() {
			t.Errorf("%s: sin renglón de tarjeta", tc.nombre)
		}
	}
	// Las claves del JSON son un contrato con los scripts: no se renombran.
	if tipoExterno(reaper.ExtContainer) != "contenedor" || tipoExterno(reaper.ExtLinux) != "linux" {
		t.Fatal("las claves estables del JSON cambiaron")
	}
}

// El bloque del contenedor muestra TODO lo que el contrato pide antes de
// preguntar: nombre, imagen, compose (proyecto y servicio), política de
// reinicio y todos los puertos que publica; que se detiene entero; y quién lo
// sirve en Windows, que no se toca.
func TestBloqueContenedor(t *testing.T) {
	term := tui.Open()
	c := &contenedor{ID: idDB, Nombre: "shop-db-1", Imagen: "postgres:16", Proyecto: "shop", Servicio: "db", Reinicio: "unless-stopped",
		Motor: &motor{Nombre: "Docker Desktop"},
		Puertos: []publicado{
			{IP: "0.0.0.0", Privado: 5432, Publico: 5432, Proto: ports.TCP}, {IP: "::", Privado: 5432, Publico: 5432, Proto: ports.TCP},
			{IP: "0.0.0.0", Privado: 80, Publico: 8080, Proto: ports.TCP}, {IP: "0.0.0.0", Privado: 53, Publico: 5353, Proto: ports.UDP},
		}}
	tg := reaper.Target{Kind: reaper.StopExternal, Name: c.Nombre, Ports: ports.Of(5432),
		External: reaper.External{Kind: reaper.ExtContainer, Where: "Docker Desktop", Key: c.ID, Name: c.Nombre, Ports: c.puertosHost(), Ref: c},
		Sockets:  []ports.Socket{sock(ports.TCP, "0.0.0.0", 5432, ports.Listen, 8124, ""), sock(ports.TCP, "::", 5432, ports.Listen, 8124, "")},
		Via:      viaRelay, ViaName: "com.docker.backend.exe", ViaClass: reaper.RelayContainer}
	got := plano(bloqueExterno(term, tg, t0, 0))
	want := []string{
		"● contenedor shop-db-1 postgres:16 · compose shop/db",
		"5432 TCP escuchando en ∗ v4+v6 · toda la red",
		"publica 5432 → 5432/tcp, 8080 → 80/tcp, 5353 → 53/udp",
		"se detiene entero: libera también 5353, 8080",
		"reinicio: unless-stopped (detenido a mano, no vuelve solo)",
		"es lo mismo que `docker compose -p shop stop db`",
		"Windows lo ve como com.docker.backend.exe (PID 8124): es Docker Desktop, no se toca",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("bloque =\n%s\n\nesperaba\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// restart=always: la política se nombra y la advertencia dice qué implica.
	c.Reinicio = "always"
	tg.Caution = c.cautela()
	got = plano(bloqueExterno(term, tg, t0, 0))
	if !contiene(got, "reinicio: always") || !contiene(got, "ojo: tiene restart=always: vuelve solo la próxima vez que arranque Docker Desktop") {
		t.Fatalf("bloque con always =\n%s", strings.Join(got, "\n"))
	}
	// Sin inspect (el motor no contestó): no se inventa una política.
	c.Reinicio = ""
	tg.Caution = ""
	if got = plano(bloqueExterno(term, tg, t0, 0)); contiene(got, "reinicio") {
		t.Fatalf("sin dato de reinicio no se dice nada:\n%s", strings.Join(got, "\n"))
	}
}

// El bloque del proceso Linux: nombre, PID y distro, desde cuándo, su línea
// de comandos y su usuario; y el relay de WSL que lo reenvía.
func TestBloqueLinux(t *testing.T) {
	term := tui.Open()
	p := &procesoLinux{Distro: "Ubuntu", PID: 812, Arranque: 523400, Comm: "node", Cmdline: "node server.js --port 3000", Usuario: "agus", Desde: t0.Add(-90 * time.Minute)}
	tg := reaper.Target{Kind: reaper.StopExternal, Name: "node", Ports: ports.Of(3000),
		External: reaper.External{Kind: reaper.ExtLinux, Where: "Ubuntu", Key: p.clave(), Name: "node", Ports: ports.Of(3000), Ref: p},
		Sockets:  []ports.Socket{sock(ports.TCP, "127.0.0.1", 3000, ports.Listen, 9120, "")},
		Via:      viaWSL, ViaName: "wslrelay.exe", ViaClass: reaper.RelayWSL}
	got := plano(bloqueExterno(term, tg, t0, 1))
	want := []string{
		"● node PID 812 en Ubuntu · " + tui.Ago(p.Desde, t0),
		"3000 TCP escuchando en 127.0.0.1 · esta PC",
		"node server.js --port 3000",
		"usuario: agus (dentro de WSL)",
		"Windows lo ve como wslrelay.exe (PID 9120): es el reenvío de WSL, no se toca",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("bloque =\n%s\n\nesperaba\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Un dominio nuevo se presenta solo, y uno sin Ref no deja el bloque vacío.
func TestBloqueDominioNuevo(t *testing.T) {
	term := tui.Open()
	x := externoRegla(&reglaFalsa{})
	tg := reaper.Target{Kind: reaper.StopExternal, Name: x.Name, Ports: ports.Of(8080), External: x, Caution: "pide administrador"}
	got := plano(bloqueExterno(term, tg, t0, 0))
	want := []string{"● regla de reenvío 0.0.0.0:8080", "conecta a 10.0.0.5:80", "ojo: pide administrador"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("bloque =\n%s", strings.Join(got, "\n"))
	}
	x.Ref = nil
	tg.External = x
	if got = plano(bloqueExterno(term, tg, t0, 0)); len(got) == 0 || got[0] != "● 0.0.0.0:8080" {
		t.Fatalf("bloque sin Ref =\n%s", strings.Join(got, "\n"))
	}
}

// La pregunta nombra lo que se va a detener con el verbo de su dominio.
func TestPreguntaCierre(t *testing.T) {
	term := tui.Open()
	db := reaper.Target{Kind: reaper.StopExternal, Ports: ports.Of(5432), External: reaper.External{Kind: reaper.ExtContainer, Name: "db-1"}}
	node := reaper.Target{Kind: reaper.StopExternal, Ports: ports.Of(3000), External: reaper.External{Kind: reaper.ExtLinux, Name: "node", Where: "Ubuntu"}}
	proc := reaper.Target{Kind: reaper.KillProcess, Ports: ports.Of(8080), Name: "node.exe", Root: reaper.ID{PID: 100}}
	for _, tc := range []struct {
		targets []reaper.Target
		want    string
	}{
		{[]reaper.Target{db}, "¿Detener el contenedor db-1 y liberar el 5432? [s/N]"},
		{[]reaper.Target{node}, "¿Cerrar node en Ubuntu y liberar el 3000? [s/N]"},
		{[]reaper.Target{proc}, "¿Cerrar node.exe y liberar el 8080? [s/N]"},
		{[]reaper.Target{db, node}, "¿Cerrar 2 objetivos y liberar 3000, 5432? [s/N]"},
		{[]reaper.Target{proc, proc}, "¿Cerrar 2 procesos y liberar 8080? [s/N]"},
	} {
		if got := strings.TrimSpace(preguntaCierre(term, reaper.Plan{Targets: tc.targets}, false, false)); got != tc.want {
			t.Errorf("pregunta = %q, esperaba %q", got, tc.want)
		}
	}
}

// La narración de los externos: qué se pide (y con --force no se promete un
// SIGINT que nadie manda), cómo terminó, y por qué no se pudo.
func TestLineaEventoExternos(t *testing.T) {
	term := tui.Open()
	db := reaper.External{Kind: reaper.ExtContainer, Name: "db-1"}
	node := reaper.External{Kind: reaper.ExtLinux, Name: "node", Where: "Ubuntu"}
	reglaDe, _ := ports.ParseProxyRule(ports.V4ToV4, "0.0.0.0/3000", "172.24.1.5/3000")
	regla := externoDeRegla(reglaDe)
	// Sin ser administrador, lo que pide administrador no es una falla: es el
	// paso previo al pedido. Siéndolo, un acceso denegado es el final.
	for _, tc := range []struct {
		e     reaper.Event
		admin bool
		want  []string
		no    string
	}{
		{reaper.Event{Kind: reaper.EvExternalFailed, External: regla, Outcome: reaper.OutcomeDenied, Detail: errPideAdmin.Error()}, false,
			[]string{"Borrar la regla de portproxy 0.0.0.0:3000 → 172.24.1.5:3000 pide administrador"}, "No pude"},
		{reaper.Event{Kind: reaper.EvExternalFailed, External: regla, Outcome: reaper.OutcomeError, Detail: "netsh: Acceso denegado."}, true,
			[]string{"No pude borrar la regla de portproxy 0.0.0.0:3000 → 172.24.1.5:3000", "netsh: Acceso denegado."}, "detener"},
		{reaper.Event{Kind: reaper.EvExternalFailed, External: node, Outcome: reaper.OutcomeError, Detail: "no contesta"}, false,
			[]string{"No pude cerrar node en Ubuntu", "no contesta"}, "detener"},
		{reaper.Event{Kind: reaper.EvServiceFailed, Name: "MySQL80", Err: reaper.ErrServiceAccessDenied, Detail: "necesita administrador"}, false,
			[]string{"Detener el servicio MySQL80 pide administrador"}, "No pude"},
		{reaper.Event{Kind: reaper.EvServiceFailed, Name: "MySQL80", Err: reaper.ErrServiceAccessDenied, Detail: "necesita administrador"}, true,
			[]string{"No pude detener MySQL80", "aun como administrador"}, "pide administrador"},
		{reaper.Event{Kind: reaper.EvServiceFailed, Name: "MySQL80", Err: reaper.ErrServiceTimeout, Detail: "no llegó a detenerse"}, false,
			[]string{"No pude detener MySQL80", "no llegó a detenerse"}, "administrador"},
	} {
		got := lineaEvento(term, tc.e, narracion{admin: tc.admin})
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%v (admin %v) = %q: falta %q", tc.e.Kind, tc.admin, got, w)
			}
		}
		if tc.no != "" && strings.Contains(got, tc.no) {
			t.Errorf("%v (admin %v) = %q: no puede decir %q", tc.e.Kind, tc.admin, got, tc.no)
		}
	}
	for _, tc := range []struct {
		e      reaper.Event
		enSeco bool
		want   []string // lo que la línea tiene que decir
		no     string   // lo que NO puede decir
	}{
		{reaper.Event{Kind: reaper.EvExternalStopping, External: node}, false, []string{"SIGINT a node en Ubuntu", "SIGTERM y después SIGKILL"}, ""},
		{reaper.Event{Kind: reaper.EvExternalStopping, External: node, Method: reaper.MethodTerminate}, true, []string{"SIGKILL a node en Ubuntu"}, "SIGINT"},
		{reaper.Event{Kind: reaper.EvExternalStopping, External: db}, false, []string{"Deteniendo el contenedor db-1"}, "en seco"},
		{reaper.Event{Kind: reaper.EvExternalStopping, External: db, Method: reaper.MethodTerminate}, true, []string{"Deteniendo el contenedor db-1", "en seco"}, ""},
		{reaper.Event{Kind: reaper.EvExternalStopped, External: db, Outcome: reaper.OutcomeClosed, Method: reaper.MethodEngine, Elapsed: 1200 * time.Millisecond}, false,
			[]string{"el contenedor db-1 se detuvo", "stop del motor", tui.Duration(1200 * time.Millisecond)}, ""},
		{reaper.Event{Kind: reaper.EvExternalStopped, External: node, Outcome: reaper.OutcomeClosed, Method: reaper.MethodSigInt, Elapsed: 300 * time.Millisecond}, false,
			[]string{"node en Ubuntu se detuvo", "SIGINT"}, ""},
		{reaper.Event{Kind: reaper.EvExternalStopped, External: node, Outcome: reaper.OutcomeTerminated, Method: reaper.MethodTerminate}, false,
			[]string{"node en Ubuntu terminado en seco", "no cedió a tiempo", "SIGKILL"}, ""},
		{reaper.Event{Kind: reaper.EvExternalStopped, External: node, Outcome: reaper.OutcomeTerminated, Method: reaper.MethodTerminate}, true,
			[]string{"node en Ubuntu terminado en seco", "SIGKILL"}, "no cedió"},
		{reaper.Event{Kind: reaper.EvExternalStopped, External: db, Outcome: reaper.OutcomeGone}, false, []string{"el contenedor db-1 ya no estaba"}, ""},
		{reaper.Event{Kind: reaper.EvExternalFailed, External: db, Outcome: reaper.OutcomeDenied, Detail: "tenés que estar en docker-users"}, false,
			[]string{"No pude detener el contenedor db-1", "docker-users"}, ""},
	} {
		got := lineaEvento(term, tc.e, narracion{enSeco: tc.enSeco})
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%v (en seco %v) = %q: falta %q", tc.e.Kind, tc.enSeco, got, w)
			}
		}
		if tc.no != "" && strings.Contains(got, tc.no) {
			t.Errorf("%v (en seco %v) = %q: no puede decir %q", tc.e.Kind, tc.enSeco, got, tc.no)
		}
	}
}

// La tarjeta final cuenta los externos por clase (en el orden de las
// clases), suma los forzados a "en seco" y no cuenta lo que no se detuvo.
func TestItemsExternos(t *testing.T) {
	x := func(k reaper.ExtKind, o reaper.Outcome) reaper.ExternalResult {
		return reaper.ExternalResult{External: reaper.External{Kind: k}, Outcome: o}
	}
	items, enSeco := itemsExternos([]reaper.ExternalResult{
		x(extRegla, reaper.OutcomeClosed),
		x(reaper.ExtLinux, reaper.OutcomeClosed), x(reaper.ExtLinux, reaper.OutcomeGone), x(reaper.ExtLinux, reaper.OutcomeTerminated),
		x(reaper.ExtContainer, reaper.OutcomeClosed), x(reaper.ExtContainer, reaper.OutcomeDenied), x(reaper.ExtContainer, reaper.OutcomeError),
		x(reaper.ExtContainer, reaper.OutcomePending), x(reaper.ExtContainer, reaper.OutcomeTerminated),
	})
	if enSeco != 2 {
		t.Fatalf("en seco = %d, esperaba 2", enSeco)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Label+"="+it.Value)
	}
	if want := "Contenedores detenidos=1|Procesos Linux cerrados=2|Otros detenidos=1"; strings.Join(got, "|") != want {
		t.Fatalf("renglones = %q, esperaba %q", strings.Join(got, "|"), want)
	}
	if items, enSeco = itemsExternos(nil); len(items) != 0 || enSeco != 0 {
		t.Fatal("sin externos no hay renglones")
	}

	// Y en la tarjeta entera, junto a los procesos.
	card := strings.Join(tarjeta(tui.Open(), reaper.Report{
		Victims:   []reaper.VictimResult{{ID: reaper.ID{PID: 1}, Outcome: reaper.OutcomeTerminated}},
		Externals: []reaper.ExternalResult{x(reaper.ExtContainer, reaper.OutcomeClosed), x(reaper.ExtLinux, reaper.OutcomeTerminated)},
		Ports:     []reaper.PortResult{{Port: 5432, Status: reaper.PortFree}},
	}, nil, 0), "\n")
	for _, w := range []string{"Contenedores detenidos", "En seco", "Puertos libres"} {
		if !strings.Contains(card, w) {
			t.Fatalf("la tarjeta no dice %q:\n%s", w, card)
		}
	}
	if strings.Contains(card, "Procesos Linux cerrados") {
		t.Fatalf("un proceso Linux forzado va con los 'en seco', no con los cerrados:\n%s", card)
	}
}

// El JSON de un dominio nuevo: su clase, lo que él mismo anota, y sin PID de
// Windows propio. Las claves de los dominios conocidos no cambian.
func TestJSONDominioNuevo(t *testing.T) {
	x := externoRegla(&reglaFalsa{})
	plan := reaper.Plan{Targets: []reaper.Target{{Kind: reaper.StopExternal, Name: x.Name, External: x, Ports: ports.Of(8080),
		Via: reaper.ID{PID: 1234}, ViaName: "svchost.exe"}}}
	rep := reaper.Report{Externals: []reaper.ExternalResult{{External: x, Outcome: reaper.OutcomeClosed, Method: reaper.MethodEngine}}}
	b, err := json.Marshal(construirJSONReporte(rep, plan, false))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Objetivos []map[string]any `json:"objetivos"`
		Externos  []map[string]any `json:"externos"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	o := doc.Objetivos[0]
	if o["tipo"] != "externo" || o["servicio"] != "iphlpsvc" || o["via"].(map[string]any)["proceso"] != "svchost.exe" {
		t.Fatalf("objetivo = %v", o)
	}
	if _, tiene := o["pid"]; tiene {
		t.Fatalf("un externo no tiene PID de Windows propio: %v", o)
	}
	if e := doc.Externos[0]; e["tipo"] != "externo" || e["clave"] != x.Key || e["destino"] != "cerrado" {
		t.Fatalf("externo = %v", e)
	}
}

// El trabajo elevado: el relay de WSL que el padre declaró colgado (y no pudo
// cerrar por falta de permiso) vuelve a ser objetivo en el hijo; el motor de
// contenedores NUNCA, aunque alguien lo cuele en el trabajo; y sin pedido, el
// relay sigue negado.
func TestElevadoRelayColgado(t *testing.T) {
	wslrelay := reaper.Proc{ID: reaper.ID{PID: 9120, Created: t0}, Name: "wslrelay.exe", Path: `C:\Program Files\WSL\wslrelay.exe`}
	backend := reaper.Proc{ID: reaper.ID{PID: 8124, Created: t0}, Name: "com.docker.backend.exe", Path: `C:\Program Files\Docker\Docker\resources\com.docker.backend.exe`}
	code := reaper.Proc{ID: reaper.ID{PID: 7000, Created: t0}, Name: "Code.exe", Path: `C:\Users\u\AppData\Local\Programs\Microsoft VS Code\Code.exe`}
	forest := reaper.NewForest([]reaper.Proc{wslrelay, backend, code})
	socks := []ports.Socket{
		sock(ports.TCP, "127.0.0.1", 3000, ports.Listen, 9120, ""), sock(ports.TCP, "::1", 3000, ports.Listen, 9120, ""),
		sock(ports.TCP, "127.0.0.1", 3001, ports.Listen, 9120, ""),
		sock(ports.TCP, "0.0.0.0", 5432, ports.Listen, 8124, ""),
		sock(ports.TCP, "127.0.0.1", 5500, ports.Listen, 7000, ""),
	}
	pedidos := ports.Of(3000, 5432, 5500)
	planear := func(job trabajo) reaper.Plan {
		return planElevado(job, holding(socks, ports.Filter{}, pedidos), forest, nil, reaper.Config{SystemRoot: `C:\Windows`}, detras{})
	}

	// Lo que el padre pidió: el wslrelay, con su identidad exacta (que viajó
	// por argumentos: otra representación de la misma hora).
	viajo := decodeIDs(idsEncoded([]reaper.ID{wslrelay.ID, backend.ID, code.ID}))
	plan := planear(trabajo{Puertos: pedidos, Kill: viajo})
	if len(plan.Targets) != 1 {
		t.Fatalf("objetivos = %+v", plan.Targets)
	}
	tg := plan.Targets[0]
	if tg.Kind != reaper.KillProcess || tg.Root.PID != 9120 || len(tg.Victims) != 1 || tg.Victims[0].PID != 9120 || !tg.Ports.Equal(ports.Of(3000)) {
		t.Fatalf("objetivo = %+v: tenía que ser el wslrelay, por el puerto pedido", tg)
	}
	for _, r := range plan.Refusals {
		if r.PID == 9120 {
			t.Fatalf("el wslrelay pedido quedó negado: %+v", r)
		}
	}
	negados := map[uint32]bool{}
	for _, r := range plan.Refusals {
		negados[r.PID] = true
	}
	if !negados[8124] || !negados[7000] {
		t.Fatalf("el motor de contenedores y VS Code no se tocan ni elevado: %+v", plan.Refusals)
	}

	// Mismo PID, otra hora de creación: es OTRO proceso (PID reciclado).
	otro := reaper.ID{PID: 9120, Created: t0.Add(time.Second)}
	if plan = planear(trabajo{Puertos: pedidos, Kill: []reaper.ID{otro}}); len(plan.Targets) != 0 {
		t.Fatalf("una identidad que no coincide no abre la puerta: %+v", plan.Targets)
	}
	// Sin pedido: todo negado, como siempre.
	if plan = planear(trabajo{Puertos: pedidos}); len(plan.Targets) != 0 || len(plan.Refusals) != 3 {
		t.Fatalf("sin pedido = %+v", plan)
	}
}
