package killport

// docker_parse.go: lo PURO de hablar con Docker/Podman —dónde está el motor,
// qué contesta y cómo se lee—, sin tocar el sistema. Se prueba con respuestas
// JSON grabadas (testdata/docker_*.json). El lado que abre el pipe y hace las
// peticiones está en docker.go.
//
// Qué se le pregunta al motor y por qué así:
//
//   - GET /containers/json (sin filtro) y se compara Ports[].PublicPort a
//     mano. El filtro `publish` de la API compara el puerto del CONTENEDOR,
//     no el del host (es la queja de docker/cli#2004: no hay forma de buscar
//     por puerto del host), así que con `-p 15432:5432` filtrar por 15432 no
//     encuentra nada y filtrar por 5432 encuentra al equivocado. Listar todos
//     cuesta una petición igual y no depende de esa semántica ni de qué tan
//     bien la imita Podman.
//   - Rutas SIN versión (/containers/json, no /v1.47/…): el motor responde con
//     la suya y no hay que negociar; solo se leen campos estables desde la API
//     1.24.

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
)

// motor es un motor de contenedores que contestó por un named pipe.
type motor struct {
	Nombre string // "Docker Desktop", "Docker", "Podman"
	Pipe   string // \\.\pipe\…: por donde se lo detiene después
	Server uint32 // PID que sirve el pipe (0 si no se supo)
}

// contenedor es lo que killport necesita saber de un contenedor.
type contenedor struct {
	Motor    *motor
	ID       string
	Nombre   string // sin la "/" que antepone la API
	Imagen   string
	Proyecto string // compose: com.docker.compose.project
	Servicio string // compose: com.docker.compose.service
	Reinicio string // HostConfig.RestartPolicy.Name: "", "no", "always", "unless-stopped", "on-failure"
	Puertos  []publicado
}

// publicado es un puerto que el contenedor publica en el host.
type publicado struct {
	IP      string // "0.0.0.0", "::", "127.0.0.1"
	Privado uint16 // el del contenedor
	Publico uint16 // el del host: el que ocupa el relay
	Proto   ports.Proto
}

// idContenedor dice si s es un id de Docker o Podman: hexadecimal en
// minúsculas, de 12 (abreviado) a 64 (completo) caracteres. Se valida ANTES
// de meterlo en una ruta: el motor es terreno ajeno (y el nombre del pipe lo
// puede ocupar cualquiera; ver win.DialPipe). Un bucle y no una expresión
// regular: era el único uso de regexp del binario (≈260 KB). O(len).
func idContenedor(s string) bool {
	if len(s) < 12 || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// apiContainer es lo que interesa de GET /containers/json.
type apiContainer struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
	Ports  []struct {
		IP          string `json:"IP"`
		PrivatePort uint16 `json:"PrivatePort"`
		PublicPort  uint16 `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

// parseContainers lee la respuesta de GET /containers/json. Descarta los que
// no corren (la lista sin all=1 ya los excluye; se filtra igual por las APIs
// compatibles) y los puertos expuestos sin publicar (PublicPort 0). Un id que
// no es hexadecimal invalida la respuesta entera: algo raro contesta en ese pipe.
func parseContainers(body []byte, m *motor) ([]contenedor, error) {
	var raw []apiContainer
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("respuesta ilegible del motor: %w", err)
	}
	out := make([]contenedor, 0, len(raw))
	for _, c := range raw {
		if !idContenedor(c.ID) {
			return nil, fmt.Errorf("el motor devolvió un id de contenedor inválido (%q)", c.ID)
		}
		if c.State != "" && c.State != "running" {
			continue
		}
		k := contenedor{Motor: m, ID: c.ID, Imagen: c.Image,
			Proyecto: c.Labels["com.docker.compose.project"], Servicio: c.Labels["com.docker.compose.service"]}
		if len(c.Names) > 0 {
			k.Nombre = strings.TrimPrefix(c.Names[0], "/")
		}
		if k.Nombre == "" {
			k.Nombre = c.ID[:12]
		}
		for _, p := range c.Ports {
			if p.PublicPort == 0 {
				continue // expuesto, no publicado: no ocupa nada en el host
			}
			proto := ports.TCP
			switch strings.ToLower(p.Type) {
			case "udp":
				proto = ports.UDP
			case "tcp", "":
			default:
				continue // sctp: no hay tabla de sockets de Windows para eso
			}
			k.Puertos = append(k.Puertos, publicado{IP: p.IP, Privado: p.PrivatePort, Publico: p.PublicPort, Proto: proto})
		}
		// El motor los lista en el orden de su mapa interno, que cambia de una
		// consulta a otra: se ordenan para que lo que se muestra (y el JSON)
		// sea estable.
		slices.SortStableFunc(k.Puertos, func(a, b publicado) int {
			return cmp.Or(cmp.Compare(a.Publico, b.Publico), cmp.Compare(a.Proto, b.Proto), cmp.Compare(a.Privado, b.Privado), cmp.Compare(a.IP, b.IP))
		})
		out = append(out, k)
	}
	return out, nil
}

// apiInspect es lo que interesa de GET /containers/{id}/json.
type apiInspect struct {
	State struct {
		Running   bool `json:"Running"`
		ExitCode  int  `json:"ExitCode"`
		OOMKilled bool `json:"OOMKilled"`
	} `json:"State"`
	HostConfig struct {
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
}

func parseInspect(body []byte) (apiInspect, error) {
	var in apiInspect
	if err := json.Unmarshal(body, &in); err != nil {
		return in, fmt.Errorf("respuesta ilegible del motor: %w", err)
	}
	return in, nil
}

// apiErrorMessage saca el mensaje del cuerpo de un error de la API:
// {"message": "…"}.
func apiErrorMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return e.Message
	}
	return strings.TrimSpace(string(body))
}

// publicaEn dice si el contenedor publica port/proto en el host.
func (c contenedor) publicaEn(port uint16, proto ports.Proto) bool {
	return slices.ContainsFunc(c.Puertos, func(p publicado) bool { return p.Publico == port && p.Proto == proto })
}

// parteDePublicacion son los sockets de un intermediario que atienden los
// mismos contenedores.
type parteDePublicacion struct {
	conts []contenedor
	socks []ports.Socket
}

// repartirPublicacion reparte los sockets de un intermediario en un puerto
// entre los contenedores que lo publican, por la dirección publicada: dos
// contenedores pueden publicar el 8080 en 127.0.0.1 y en 127.0.0.2, y el
// motor abre un socket por dirección. Cada socket va al contenedor que
// publica EXACTAMENTE esa dirección; si ninguno, a los que publican el
// comodín de su familia (0.0.0.0 para IPv4, :: para IPv6); si ninguno, al
// comodín de la otra familia (el [::1] de wslrelay y el [::] del motor siguen
// a una publicación en 0.0.0.0 aunque la API no liste la de ::); y si tampoco,
// a todos (lo de antes: mejor nombrar de más que dejar un socket sin dueño).
// Sin sockets a la vista (una MaskedPort armada a mano), una sola parte con
// todos. Puro; en el orden de los sockets. O(s · c · p).
func repartirPublicacion(m reaper.MaskedPort, conts []contenedor) []parteDePublicacion {
	if len(m.Sockets) == 0 || len(conts) == 1 {
		return []parteDePublicacion{{conts: conts, socks: m.Sockets}}
	}
	var out []parteDePublicacion
	clave := map[string]int{} // ids de los contenedores → índice en out
	for _, s := range m.Sockets {
		cs := atiendenSocket(conts, s, m.Port, m.Proto)
		if len(cs) == 0 {
			cs = conts
		}
		ids := make([]string, len(cs))
		for i, c := range cs {
			ids[i] = c.ID
		}
		k := strings.Join(ids, ",")
		i, ok := clave[k]
		if !ok {
			out = append(out, parteDePublicacion{conts: cs})
			i = len(out) - 1
			clave[k] = i
		}
		out[i].socks = append(out[i].socks, s)
	}
	return out
}

// atiendenSocket son los contenedores que atienden ese socket del
// intermediario (ver repartirPublicacion), por orden de precedencia.
func atiendenSocket(conts []contenedor, s ports.Socket, port uint16, proto ports.Proto) []contenedor {
	addr := s.Local.Addr().WithZone("").Unmap()
	var exacta, familia, otra []contenedor
	for _, c := range conts {
		var e, f, o bool
		for _, p := range c.Puertos {
			if p.Publico != port || p.Proto != proto {
				continue
			}
			ip, err := netip.ParseAddr(p.IP)
			switch {
			case p.IP == "" || err != nil:
				f = true // sin dirección publicada: en todas
			case ip.Unmap() == addr:
				e = true
			case ip.IsUnspecified() && ip.Is4() == addr.Is4():
				f = true
			case ip.IsUnspecified():
				o = true
			}
		}
		switch {
		case e:
			exacta = append(exacta, c)
		case f:
			familia = append(familia, c)
		case o:
			otra = append(otra, c)
		}
	}
	switch {
	case len(exacta) > 0:
		return exacta
	case len(familia) > 0:
		return familia
	}
	return otra
}

// puertosHost son todos los puertos del host que publica (detenerlo los libera todos).
func (c contenedor) puertosHost() ports.Set {
	ps := make([]uint16, len(c.Puertos))
	for i, p := range c.Puertos {
		ps[i] = p.Publico
	}
	return ports.Of(ps...)
}

// cautela es lo que hay que saber antes de detenerlo, según su política de
// reinicio. `docker stop` marca al contenedor como detenido A MANO: ni
// unless-stopped ni on-failure lo vuelven a arrancar; always sí, cuando
// arranca el motor (al reiniciar Docker Desktop o la máquina).
func (c contenedor) cautela() string {
	if c.Reinicio == "always" {
		return "tiene restart=always: vuelve solo la próxima vez que arranque " + c.motorNombre()
	}
	return ""
}

// politicaReinicio dice, para mostrar, la política de reinicio y qué implica
// después de detenerlo; "" si no se supo (el motor no contestó el inspect).
// `docker stop` marca al contenedor como detenido A MANO: solo always lo
// vuelve a levantar, y recién cuando arranca el motor.
func (c contenedor) politicaReinicio() string {
	switch c.Reinicio {
	case "":
		return ""
	case "no":
		return "no (queda detenido)"
	case "always":
		return "always" // qué implica lo dice la advertencia (cautela)
	case "unless-stopped", "on-failure":
		return c.Reinicio + " (detenido a mano, no vuelve solo)"
	}
	return c.Reinicio
}

// publicaciones es la lista host → contenedor de lo que publica, sin repetir
// v4 y v6 del mismo puerto: "5432 → 5432/tcp, 8080 → 80/tcp".
func (c contenedor) publicaciones() string {
	var parts []string
	seen := map[string]bool{}
	for _, p := range c.Puertos {
		s := fmt.Sprintf("%d → %d/%s", p.Publico, p.Privado, strings.ToLower(p.Proto.String()))
		if !seen[s] {
			seen[s] = true
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

func (c contenedor) motorNombre() string {
	if c.Motor != nil && c.Motor.Nombre != "" {
		return c.Motor.Nombre
	}
	return "el motor"
}

// comandoCompose es la forma de hacer lo mismo desde compose, si es de compose.
func (c contenedor) comandoCompose() string {
	if c.Proyecto == "" || c.Servicio == "" {
		return ""
	}
	return "docker compose -p " + c.Proyecto + " stop " + c.Servicio
}

// --- dónde está el motor ------------------------------------------------------

// Pipes de siempre, en el orden en que se prueban si nada dice otra cosa:
// Docker Engine / Docker Desktop (proxy), el motor Linux de Docker Desktop, y
// la máquina por defecto de Podman.
var pipesPorDefecto = []string{
	`\\.\pipe\docker_engine`,
	`\\.\pipe\dockerDesktopLinuxEngine`,
	`\\.\pipe\podman-machine-default`,
}

// pipeDeHost convierte un host de Docker en la ruta de un pipe LOCAL:
// "npipe:////./pipe/docker_engine" → `\\.\pipe\docker_engine`. Cualquier otro
// esquema (tcp://, ssh://, unix://) o un pipe de otra máquina (//srv/pipe/x)
// devuelve false: killport no habla con motores remotos.
func pipeDeHost(host string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(host), "npipe://")
	if !ok {
		return "", false
	}
	rest = strings.ReplaceAll(rest, `\`, "/")
	rest = strings.TrimLeft(rest, "/")
	name, ok := strings.CutPrefix(rest, "./pipe/")
	if !ok || name == "" || strings.ContainsAny(name, `/`) {
		return "", false
	}
	return `\\.\pipe\` + name, true
}

// entorno es lo que la búsqueda del motor lee del sistema (inyectable).
type entorno struct {
	getenv   func(string) string
	readFile func(string) ([]byte, error)
	home     string // %USERPROFILE%
}

// pipesCandidatos arma la lista ordenada y sin repetidos de pipes a probar:
// DOCKER_HOST (si es npipe), el endpoint del contexto actual (DOCKER_CONTEXT
// o currentContext de config.json), y los de siempre. Devuelve también una
// nota si DOCKER_HOST o el contexto apuntan a algo que no se consulta (tcp://,
// ssh://): para decirlo en vez de fingir que no hay motor.
func pipesCandidatos(env entorno) (pipes []string, nota string) {
	add := func(p string) {
		if !slices.ContainsFunc(pipes, func(q string) bool { return strings.EqualFold(p, q) }) {
			pipes = append(pipes, p)
		}
	}
	if h := env.getenv("DOCKER_HOST"); h != "" {
		if p, ok := pipeDeHost(h); ok {
			add(p)
		} else {
			nota = "DOCKER_HOST apunta a " + h + " (no se consulta: solo motores locales por named pipe)"
		}
	}
	if h := hostDelContexto(env); h != "" {
		if p, ok := pipeDeHost(h); ok {
			add(p)
		} else if nota == "" {
			nota = "el contexto de Docker apunta a " + h + " (no se consulta: solo motores locales por named pipe)"
		}
	}
	for _, p := range pipesPorDefecto {
		add(p)
	}
	return pipes, nota
}

// hostDelContexto lee el endpoint del contexto actual de Docker:
// DOCKER_CONTEXT, o "currentContext" de config.json; "default" (o nada) no
// tiene archivo. El meta del contexto vive en
// contexts/meta/<sha256 del nombre>/meta.json. DOCKER_CONFIG cambia la carpeta.
func hostDelContexto(env entorno) string {
	dir := env.getenv("DOCKER_CONFIG")
	if dir == "" {
		if env.home == "" {
			return ""
		}
		dir = env.home + `\.docker`
	}
	name := env.getenv("DOCKER_CONTEXT")
	if name == "" {
		b, err := env.readFile(dir + `\config.json`)
		if err != nil {
			return ""
		}
		var cfg struct {
			CurrentContext string `json:"currentContext"`
		}
		if json.Unmarshal(b, &cfg) != nil {
			return ""
		}
		name = cfg.CurrentContext
	}
	if name == "" || name == "default" {
		return ""
	}
	sum := sha256.Sum256([]byte(name))
	b, err := env.readFile(dir + `\contexts\meta\` + hex.EncodeToString(sum[:]) + `\meta.json`)
	if err != nil {
		return ""
	}
	var meta struct {
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}
	if json.Unmarshal(b, &meta) != nil {
		return ""
	}
	return meta.Endpoints["docker"].Host
}

// nombreMotor decide cómo llamar al motor: Podman se delata con la cabecera
// Libpod-Api-Version de /_ping; Docker Desktop, por su pipe o por quién lo
// sirve; si no, "Docker".
func nombreMotor(pipe, servidor string, libpod bool) string {
	switch {
	case libpod, strings.Contains(strings.ToLower(pipe), "podman"):
		return "Podman"
	case strings.Contains(strings.ToLower(pipe), "dockerdesktop"),
		strings.HasPrefix(strings.ToLower(servidor), "com.docker."),
		strings.EqualFold(servidor, "docker desktop.exe"):
		return "Docker Desktop"
	}
	return "Docker"
}
