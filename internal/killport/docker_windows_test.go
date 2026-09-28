//go:build windows

package killport

// El cliente de Docker contra un MOTOR FALSO servido en un named pipe de
// verdad (win/pipetest): mismo transporte que con Docker Desktop (pipe
// overlapped, net/http de la stdlib), respuestas grabadas de la API. En esta
// máquina no hay Docker ni Podman (verificado: ningún pipe docker_* ni
// podman-*), así que el motor real no se puede probar; esto prueba todo lo
// demás de punta a punta.

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/win/pipetest"
)

const idDB = "8dfafdbc3a40c8f0f8e24c5a3a2d7c4e1b9e3f6a1c2d3e4f5a6b7c8d9e0f1a2b"

// motorFalso es un motor de contenedores de mentira.
type motorFalso struct {
	path       string
	mu         sync.Mutex
	containers []byte
	inspect    []byte // antes del stop
	inspectFin []byte // después del stop
	stopCode   int
	stopBody   string
	libpod     bool
	stops      []string // cada stop pedido: "id?t=N"
	stopped    bool
	stopDelay  time.Duration // cuánto tarda en contestar el stop (como un contenedor que no cede)
}

func nuevoMotorFalso(t *testing.T, o pipetest.Options) *motorFalso {
	t.Helper()
	m := &motorFalso{
		path:       pipetest.Name("docker"),
		containers: grabado(t, "docker_containers.json"),
		inspect:    grabado(t, "docker_inspect_always.json"),
		inspectFin: []byte(`{"State":{"Running":false,"ExitCode":0,"OOMKilled":false},"HostConfig":{"RestartPolicy":{"Name":"always"}}}`),
		stopCode:   http.StatusNoContent,
	}
	l, err := pipetest.Listen(m.path, o)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: m}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close(); l.Close() })
	return m
}

func (m *motorFalso) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		m.mu.Lock()
		d := m.stopDelay
		m.mu.Unlock()
		time.Sleep(d) // (el contexto del lado servidor no sirve acá: el motor real tampoco se entera)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w.Header().Set("Api-Version", "1.47")
	if m.libpod {
		w.Header().Set("Libpod-Api-Version", "5.2.0")
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/_ping":
		io.WriteString(w, "OK")
	case r.Method == http.MethodGet && r.URL.Path == "/containers/json":
		w.Write(m.containers)
	case r.Method == http.MethodGet && r.URL.Path == "/containers/"+idDB+"/json":
		if m.stopped {
			w.Write(m.inspectFin)
		} else {
			w.Write(m.inspect)
		}
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/stop")
		m.stops = append(m.stops, id+"?t="+r.URL.Query().Get("t"))
		m.stopped = m.stopCode == http.StatusNoContent
		w.WriteHeader(m.stopCode)
		io.WriteString(w, m.stopBody)
	default:
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"page not found"}`)
	}
}

// dockerDe arma el cliente apuntado SOLO a los pipes dados (nunca al motor
// real de la máquina, si lo hubiera).
func dockerDe(pipes ...string) *docker {
	d := nuevoDocker()
	d.pipes = func() ([]string, string) { return pipes, "" }
	return d
}

var viaRelay = reaper.ID{PID: 8124, Created: time.Date(2026, 9, 27, 8, 0, 0, 0, time.UTC)}

func mascara(port uint16, proto ports.Proto) reaper.MaskedPort {
	return reaper.MaskedPort{Via: viaRelay, Name: "com.docker.backend.exe", Relay: reaper.RelayContainer, Port: port, Proto: proto}
}

// El 5432 lo publica shop-db-1 (restart=always: se avisa); el 8888 no lo
// publica nadie (puerto de Docker mismo); el 5353/UDP lo publica web.
func TestDockerDesenmascara(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{})
	d := dockerDe(m.path)
	res := d.desenmascarar(context.Background(), []reaper.MaskedPort{mascara(5432, ports.TCP), mascara(8888, ports.TCP), mascara(5353, ports.UDP)}, reaper.NewForest(nil))
	if len(res) != 3 {
		t.Fatalf("respuestas = %+v", res)
	}
	db := res[0]
	if db.Verdict != reaper.UnmaskFound || len(db.Behind) != 1 || db.Via != viaRelay {
		t.Fatalf("5432 = %+v", db)
	}
	x := db.Behind[0]
	if x.Kind != reaper.ExtContainer || x.Key != idDB || x.Name != "shop-db-1" || !x.Ports.Equal(ports.Of(5432)) || !strings.Contains(x.Caution, "restart=always") {
		t.Fatalf("externo = %+v", x)
	}
	if c := x.Ref.(*contenedor); c.Reinicio != "always" || c.Motor.Pipe != m.path || c.Motor.Server != uint32(os.Getpid()) {
		t.Fatalf("contenedor = %+v / motor %+v", c, c.Motor)
	}
	if res[1].Verdict != reaper.UnmaskEmpty || !strings.Contains(res[1].Reason, "8888") || res[1].KillVia {
		t.Fatalf("8888 = %+v", res[1])
	}
	if res[2].Verdict != reaper.UnmaskFound || res[2].Behind[0].Name != "web" {
		t.Fatalf("5353/udp = %+v", res[2])
	}
	// Una sola consulta por corrida: una segunda pasada no vuelve a listar.
	m.mu.Lock()
	m.containers = []byte(`[]`)
	m.mu.Unlock()
	if again := d.desenmascarar(context.Background(), []reaper.MaskedPort{mascara(5432, ports.TCP)}, reaper.NewForest(nil)); again[0].Verdict != reaper.UnmaskFound {
		t.Fatal("la lista de contenedores se memoiza por corrida")
	}
}

func TestDockerSinMotor(t *testing.T) {
	d := dockerDe(`\\.\pipe\navaja-no-hay-motor-` + time.Now().Format("150405.000000"))
	res := d.desenmascarar(context.Background(), []reaper.MaskedPort{mascara(5432, ports.TCP)}, reaper.NewForest(nil))
	if res[0].Verdict != reaper.UnmaskUnavailable || res[0].Reason == "" || res[0].Advice == "" {
		t.Fatalf("sin motor = %+v", res[0])
	}
}

// El pipe existe pero su ACL no deja (fuera de docker-users).
func TestDockerDenegado(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{SDDL: "D:(D;;GA;;;WD)"})
	res := dockerDe(m.path).desenmascarar(context.Background(), []reaper.MaskedPort{mascara(5432, ports.TCP)}, reaper.NewForest(nil))
	if res[0].Verdict != reaper.UnmaskUnavailable || !strings.Contains(res[0].Advice, "docker-users") {
		t.Fatalf("denegado = %+v", res[0])
	}
}

func TestDockerPodman(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{})
	m.libpod = true
	d := dockerDe(m.path)
	d.consultar(context.Background(), reaper.NewForest(nil))
	if len(d.motores) != 1 || d.motores[0].Nombre != "Podman" {
		t.Fatalf("motores = %+v", d.motores)
	}
}

// Un motor que acepta la conexión y no contesta: el tope de /_ping corta.
func TestDockerMotorColgado(t *testing.T) {
	path := pipetest.Name("colgado")
	l, err := pipetest.Listen(path, pipetest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(io.Discard, c); c.Close() }() // lee y nunca contesta
		}
	}()
	start := time.Now()
	res := dockerDe(path).desenmascarar(context.Background(), []reaper.MaskedPort{mascara(5432, ports.TCP)}, reaper.NewForest(nil))
	if res[0].Verdict != reaper.UnmaskUnavailable {
		t.Fatalf("colgado = %+v", res[0])
	}
	if d := time.Since(start); d > dockerPingTimeout+2*time.Second {
		t.Fatalf("tardó %v: el tope de /_ping es %v", d, dockerPingTimeout)
	}
}

// docker stop por la API: t = los segundos que quedan del plazo (redondeados
// hacia arriba), 0 con --force; y el veredicto según la respuesta y el código
// de salida.
func TestDockerDetener(t *testing.T) {
	for _, tc := range []struct {
		nombre     string
		code       int
		body       string
		inspectFin string
		force      bool
		wantT      string
		want       reaper.Outcome
		method     reaper.Method
		errHas     string
	}{
		{"prolijo", 204, "", `{"State":{"ExitCode":0}}`, false, "3", reaper.OutcomeClosed, reaper.MethodEngine, ""},
		{"forzado por el motor", 204, "", `{"State":{"ExitCode":137,"OOMKilled":false}}`, false, "3", reaper.OutcomeTerminated, reaper.MethodTerminate, ""},
		{"lo mató el OOM, no el plazo", 204, "", `{"State":{"ExitCode":137,"OOMKilled":true}}`, false, "3", reaper.OutcomeClosed, reaper.MethodEngine, ""},
		{"force", 204, "", `{"State":{"ExitCode":137}}`, true, "0", reaper.OutcomeTerminated, reaper.MethodTerminate, ""},
		{"ya detenido", 304, "", "", false, "3", reaper.OutcomeGone, reaper.MethodNone, ""},
		{"no existe", 404, `{"message":"No such container"}`, "", false, "3", reaper.OutcomeGone, reaper.MethodNone, ""},
		{"prohibido", 403, `{"message":"forbidden by policy"}`, "", false, "3", reaper.OutcomeDenied, reaper.MethodNone, "forbidden by policy"},
		{"error del motor", 500, `{"message":"cannot stop container: boom"}`, "", false, "3", reaper.OutcomeError, reaper.MethodNone, "boom"},
	} {
		t.Run(tc.nombre, func(t *testing.T) {
			m := nuevoMotorFalso(t, pipetest.Options{})
			m.stopCode, m.stopBody = tc.code, tc.body
			if tc.inspectFin != "" {
				m.inspectFin = []byte(tc.inspectFin)
			}
			d := dockerDe(m.path)
			d.consultar(context.Background(), reaper.NewForest(nil))
			cs := d.publican(5432, ports.TCP)
			if len(cs) != 1 {
				t.Fatalf("contenedores que publican el 5432: %+v", cs)
			}
			c := cs[0]
			r := d.detener(context.Background(), &c, time.Now().Add(2500*time.Millisecond), tc.force)
			if r.Outcome != tc.want || r.Method != tc.method {
				t.Fatalf("veredicto = %v/%v (%v), esperaba %v/%v", r.Outcome, r.Method, r.Err, tc.want, tc.method)
			}
			if tc.errHas != "" && (r.Err == nil || !strings.Contains(r.Err.Error(), tc.errHas)) {
				t.Fatalf("error = %v, esperaba que diga %q", r.Err, tc.errHas)
			}
			if len(m.stops) != 1 || m.stops[0] != idDB+"?t="+tc.wantT {
				t.Fatalf("stops = %v, esperaba t=%s", m.stops, tc.wantT)
			}
		})
	}
}

// Denegado al detener (el pipe dejó de dejar): Denied con el consejo.
func TestDockerDetenerDenegado(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{SDDL: "D:(D;;GA;;;WD)"})
	c := contenedor{ID: idDB, Nombre: "x", Motor: &motor{Nombre: "Docker", Pipe: m.path}}
	r := dockerDe(m.path).detener(context.Background(), &c, time.Now().Add(time.Second), false)
	if r.Outcome != reaper.OutcomeDenied || !strings.Contains(r.Err.Error(), "docker-users") {
		t.Fatalf("veredicto = %+v", r)
	}
}
