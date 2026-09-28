//go:build windows

package killport

// docker.go: el cliente de los motores de contenedores (Docker Desktop,
// Docker Engine, Podman) por su API HTTP en un named pipe, con el cliente
// HTTP/1.1 mínimo de http1.go (una conexión por petición) y el pipe
// overlapped de win.DialPipe (deadlines de verdad: un motor colgado no cuelga
// a killport). Sin el CLI de docker: no hace falta que esté instalado ni en el
// PATH, y no hay que parsear texto.
//
// Una corrida consulta cada motor UNA vez (la lista de contenedores se
// memoiza) y solo si hace falta: si ningún puerto pedido lo sostiene un relay
// de contenedores, ni se abre un pipe.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

// Topes de cada petición. La lista es chica y local: si tarda más, el motor
// está trabado y es mejor decirlo que esperarlo.
const (
	dockerPingTimeout    = 1500 * time.Millisecond
	dockerListTimeout    = 3 * time.Second
	dockerInspectTimeout = 2 * time.Second
	dockerStopMargin     = 15 * time.Second // después de t: el SIGKILL y la respuesta
	dockerMaxBody        = 8 << 20
)

// docker es el cliente de una corrida.
type docker struct {
	env   entorno
	dial  func(ctx context.Context, pipe string) (net.Conn, uint32, error)
	pipes func() ([]string, string) // candidatos y nota (nil = pipesCandidatos(env)); las pruebas lo fijan

	once    sync.Once
	motores []*motor
	conts   []contenedor
	fallas  map[string]error // pipe → por qué no contestó
	nota    string           // DOCKER_HOST/contexto que no se consulta
	mu      sync.Mutex       // el Server de cada motor (dos detenciones a la vez lo pueden anotar)
}

func nuevoDocker() *docker {
	home, _ := os.UserHomeDir()
	return &docker{
		env: entorno{getenv: os.Getenv, readFile: os.ReadFile, home: home},
		dial: func(ctx context.Context, pipe string) (net.Conn, uint32, error) {
			c, err := win.DialPipe(ctx, pipe)
			if err != nil {
				return nil, 0, err
			}
			return c, c.ServerPID(), nil
		},
	}
}

// servidor es el PID que sirve el pipe del motor (0 si no se supo).
func (d *docker) servidor(m *motor) uint32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return m.Server
}

// pedir hace una petición al motor con su tope y devuelve estado, cabeceras
// y cuerpo. Una conexión por petición (las de una corrida son un puñado): no
// queda ningún handle abierto al terminar. El plazo es el del pipe (deadline
// de lectura y escritura), y cancelar ctx lo corta al instante.
func (d *docker) pedir(ctx context.Context, m *motor, method, path string, timeout time.Duration) (int, cabeceras, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, server, err := d.dial(ctx, m.Pipe)
	if err != nil {
		return 0, nil, nil, unwrapDial(err)
	}
	defer c.Close()
	if server != 0 {
		d.mu.Lock()
		if m.Server == 0 {
			m.Server = server
		}
		d.mu.Unlock()
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	corta := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
	defer corta()
	r, err := pedirHTTP(c, method, "docker", path, dockerMaxBody)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, nil, fmt.Errorf("el motor no contestó a tiempo: %w", ctx.Err())
		}
		return 0, nil, nil, err
	}
	return r.Code, r.Header, r.Body, nil
}

// unwrapDial deja los errores del pipe en sus centinelas, para poder
// distinguir "no hay motor" de "no me deja".
func unwrapDial(err error) error {
	switch {
	case errors.Is(err, win.ErrPipeNotFound):
		return win.ErrPipeNotFound
	case errors.Is(err, win.ErrAccessDenied):
		return win.ErrAccessDenied
	}
	return err
}

// consultar pregunta a cada pipe candidato, una sola vez por corrida: /_ping
// para saber que hay un motor y cuál es, y la lista de contenedores. Los
// contenedores se deduplican por id (docker_engine y dockerDesktopLinuxEngine
// suelen ser el MISMO motor por dos puertas).
func (d *docker) consultar(ctx context.Context, forest *reaper.Forest) {
	d.once.Do(func() {
		var pipes []string
		var nota string
		if d.pipes != nil {
			pipes, nota = d.pipes()
		} else {
			pipes, nota = pipesCandidatos(d.env)
		}
		d.nota = nota
		d.fallas = map[string]error{}
		seen := map[string]bool{}
		for _, pipe := range pipes {
			m := &motor{Pipe: pipe}
			code, hdr, _, err := d.pedir(ctx, m, "GET", "/_ping", dockerPingTimeout)
			if err == nil && code != 200 {
				err = fmt.Errorf("el motor contestó %d a /_ping", code)
			}
			if err != nil {
				d.fallas[pipe] = err
				continue
			}
			servidor := ""
			if pid := d.servidor(m); pid != 0 {
				if p, ok := forest.LivePID(pid); ok {
					servidor = p.Name
				}
			}
			m.Nombre = nombreMotor(pipe, servidor, hdr.Get("Libpod-Api-Version") != "")
			code, _, body, err := d.pedir(ctx, m, "GET", "/containers/json", dockerListTimeout)
			if err == nil && code != 200 {
				err = fmt.Errorf("el motor contestó %d: %s", code, apiErrorMessage(body))
			}
			var conts []contenedor
			if err == nil {
				conts, err = parseContainers(body, m)
			}
			if err != nil {
				d.fallas[pipe] = err
				continue
			}
			d.motores = append(d.motores, m)
			for _, c := range conts {
				if !seen[c.ID] {
					seen[c.ID] = true
					d.conts = append(d.conts, c)
				}
			}
		}
	})
}

// publican devuelve los contenedores que publican port/proto en el host.
func (d *docker) publican(port uint16, proto ports.Proto) []contenedor {
	var out []contenedor
	for _, c := range d.conts {
		if c.publicaEn(port, proto) {
			out = append(out, c)
		}
	}
	return out
}

// porqueNoContesta resume por qué ningún motor contestó, en español.
func (d *docker) porqueNoContesta() (reason, advice string) {
	denied := false
	for _, err := range d.fallas {
		if errors.Is(err, win.ErrAccessDenied) {
			denied = true
		}
	}
	switch {
	case denied:
		return "el motor de contenedores no me deja preguntarle (acceso denegado a su pipe)",
			"tenés que estar en el grupo docker-users (y volver a iniciar sesión), o correr killport como administrador"
	case d.nota != "":
		return "no encontré un motor de contenedores local: " + d.nota,
			"`docker ps` muestra qué contenedor publica el puerto y `docker stop NOMBRE` lo libera"
	}
	return "lo sirve el motor de contenedores, pero no contesta en ninguno de sus pipes",
		"¿está arrancando o trabado Docker Desktop/Podman? `docker ps` muestra qué contenedor publica el puerto"
}

// desenmascarar contesta, para cada puerto que sostiene un relay de
// contenedores, qué contenedor lo publica (Found), que no lo publica ninguno
// (Empty: un puerto de Docker Desktop mismo) o que no se pudo preguntar
// (Unavailable).
func (d *docker) desenmascarar(ctx context.Context, masked []reaper.MaskedPort, forest *reaper.Forest) []reaper.Unmasking {
	d.consultar(ctx, forest)
	out := make([]reaper.Unmasking, 0, len(masked))
	for _, m := range masked {
		u := reaper.Unmasking{Via: m.Via, Port: m.Port, Proto: m.Proto, Sockets: m.Sockets}
		switch conts := d.publican(m.Port, m.Proto); {
		case len(conts) > 0:
			for _, parte := range repartirPublicacion(m, conts) {
				v := u
				v.Verdict, v.Sockets = reaper.UnmaskFound, parte.socks
				for _, c := range parte.conts {
					v.Behind = append(v.Behind, d.externo(ctx, c))
				}
				out = append(out, v)
			}
			continue
		case len(d.motores) == 0:
			u.Verdict = reaper.UnmaskUnavailable
			u.Reason, u.Advice = d.porqueNoContesta()
		default:
			u.Verdict = reaper.UnmaskEmpty
			u.Reason = "ningún contenedor publica el " + strconv.Itoa(int(m.Port)) + ": es un puerto de " + d.motores[0].Nombre + " mismo"
			u.Advice = "no hay contenedor que detener; mirá qué función de " + d.motores[0].Nombre + " lo usa (Settings) o reinicialo"
		}
		out = append(out, u)
	}
	return out
}

// conReinicio completa la política de reinicio de un contenedor (la lista de
// contenedores no la trae: una petición más, solo para los que se muestran).
func (d *docker) conReinicio(ctx context.Context, c contenedor) contenedor {
	if c.Reinicio == "" && c.Motor != nil {
		if code, _, body, err := d.pedir(ctx, c.Motor, "GET", "/containers/"+c.ID+"/json", dockerInspectTimeout); err == nil && code == 200 {
			if in, err := parseInspect(body); err == nil {
				c.Reinicio = in.HostConfig.RestartPolicy.Name
			}
		}
	}
	return c
}

// externo arma el objetivo externo de un contenedor, completando su política
// de reinicio.
func (d *docker) externo(ctx context.Context, c contenedor) reaper.External {
	c = d.conReinicio(ctx, c)
	cp := c
	return reaper.External{
		Kind: reaper.ExtContainer, Where: c.motorNombre(), Key: c.ID, Name: c.Nombre,
		Ports: c.puertosHost(), Caution: c.cautela(), Ref: &cp,
	}
}

// detener hace `docker stop` por la API: POST /containers/{id}/stop?t=N, con
// N = los segundos que quedan del plazo (0 con --force). El motor hace la
// escalera: la señal de parada del contenedor (SIGTERM salvo que la imagen
// diga otra), espera N segundos y después SIGKILL. Al volver, el código de
// salida dice cuál de las dos fue (137 = 128+SIGKILL, salvo que lo haya
// matado el OOM killer).
func (d *docker) detener(ctx context.Context, c *contenedor, deadline time.Time, force bool) reaper.ExternalStop {
	t := 0
	if !force {
		t = int(math.Ceil(time.Until(deadline).Seconds()))
		t = max(t, 0)
	}
	code, _, body, err := d.pedir(ctx, c.Motor, "POST", "/containers/"+c.ID+"/stop?t="+strconv.Itoa(t),
		time.Duration(t)*time.Second+dockerStopMargin)
	switch {
	case errors.Is(err, win.ErrAccessDenied):
		return reaper.ExternalStop{Outcome: reaper.OutcomeDenied, Err: errors.New("tenés que estar en el grupo docker-users, o correr killport como administrador")}
	case errors.Is(err, win.ErrPipeNotFound):
		return reaper.ExternalStop{Outcome: reaper.OutcomeError, Err: errors.New(c.motorNombre() + " dejó de contestar")}
	case err != nil:
		return reaper.ExternalStop{Outcome: reaper.OutcomeError, Err: err}
	}
	switch code {
	case 204:
		res := reaper.ExternalStop{Outcome: reaper.OutcomeClosed, Method: reaper.MethodEngine}
		if code, _, body, err := d.pedir(ctx, c.Motor, "GET", "/containers/"+c.ID+"/json", dockerInspectTimeout); err == nil && code == 200 {
			if in, err := parseInspect(body); err == nil && in.State.ExitCode == 137 && !in.State.OOMKilled {
				res = reaper.ExternalStop{Outcome: reaper.OutcomeTerminated, Method: reaper.MethodTerminate}
			}
		}
		return res
	case 304, 404:
		return reaper.ExternalStop{Outcome: reaper.OutcomeGone} // ya estaba detenido / ya no existe
	case 401, 403:
		return reaper.ExternalStop{Outcome: reaper.OutcomeDenied, Err: errors.New(apiErrorMessage(body))}
	}
	return reaper.ExternalStop{Outcome: reaper.OutcomeError, Err: fmt.Errorf("el motor contestó %d: %s", code, apiErrorMessage(body))}
}

// --- el contenedor como objetivo externo (ver externo.go) ---------------------

// detener: `docker stop` por la API del motor que lo listó.
func (c *contenedor) detener(ctx context.Context, s *sistema, deadline time.Time, force bool) (reaper.ExternalStop, bool) {
	return s.docker.detener(ctx, c, deadline, force), ctx.Err() == nil
}

// presentar: nombre, imagen y compose en el título; qué publica (host →
// contenedor), qué MÁS libera detenerlo —se detiene entero, no solo el puerto
// pedido—, su política de reinicio y el comando de compose equivalente.
func (c *contenedor) presentar(t *tui.Term, tg reaper.Target, _ time.Time) (string, []string) {
	titulo := t.Paint(tui.Faint, "contenedor ") + t.Paint(tui.Text, c.Nombre) +
		t.Paint(tui.Faint, "  ") + t.Paint(tui.Subtle, c.describir())
	lineas := []string{t.Paint(tui.Faint, "publica "+c.publicaciones())}
	if otros := c.puertosHost().Minus(tg.Ports); !otros.IsEmpty() {
		lineas = append(lineas, t.Paint(tui.Peach, "se detiene entero: libera también "+otros.String()))
	}
	if r := c.politicaReinicio(); r != "" {
		lineas = append(lineas, t.Paint(tui.Faint, "reinicio: "+r))
	}
	if cmd := c.comandoCompose(); cmd != "" {
		lineas = append(lineas, t.Paint(tui.Faint, "es lo mismo que `"+cmd+"`"))
	}
	return titulo, lineas
}

// anotar: el objeto "contenedor" del JSON.
func (c *contenedor) anotar(o *jsonObjetivo) { o.Contenedor = contenedorJSON(c) }

// describir es la línea de un contenedor en el listado y en los bloques:
// "postgres:16 · compose shop/db".
func (c contenedor) describir() string {
	parts := []string{c.Imagen}
	if c.Proyecto != "" {
		s := "compose " + c.Proyecto
		if c.Servicio != "" {
			s += "/" + c.Servicio
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " · ")
}
