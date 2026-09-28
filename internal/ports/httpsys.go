package ports

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Colas de http.sys: quién está detrás de un puerto que la tabla le atribuye
// a System (PID 4).
//
// http.sys es el servidor HTTP del kernel. Un programa no abre el puerto 80:
// le REGISTRA una URL a http.sys ("http://+:80/ReportServer/") y recibe los
// pedidos por una cola de solicitudes. El socket es del kernel —por eso
// netstat muestra el PID 4— y varios programas distintos pueden compartir el
// mismo puerto con URL distintas. La pregunta "¿quién tiene el 80?" se
// contesta mirando qué colas tienen una URL en ese puerto y qué procesos las
// atienden.
//
// La fuente es
//
//	netsh http show servicestate view=requestq verbose=no
//
// que anda sin administrador (muestra las colas que el usuario puede ver) y
// sale TRADUCIDA. El parser no lee ni una palabra; lee la FORMA, que es la
// misma en todos los idiomas:
//
//   - la línea de guiones que subraya el título: de ahí en adelante hay datos;
//   - una cola es un bloque que empieza en una línea SIN sangría. Ojo: la
//     etiqueta de esa línea ("Nombre de cola de solicitudes") se repite más
//     adentro, con sangría, en cada grupo de URL: lo que marca el comienzo de
//     una cola es la columna 0, no la etiqueta;
//   - una URL es una línea que empieza con http:// o https://;
//   - un proceso es «etiqueta: ENTERO, etiqueta: RUTA» (los Windows actuales)
//     o un entero solo debajo de un título de sección (los anteriores). Colgado
//     de una sección es un proceso que ATIENDE la cola; colgado directamente
//     de la cola es el que la CREÓ (el controlador: en IIS, el servicio WAS).
//
// Todo lo demás —contadores, propiedades, tiempos de espera— se ignora.

// QueueProcess es un proceso adjunto a una cola de solicitudes.
type QueueProcess struct {
	PID   uint32
	Image string // ruta del ejecutable según netsh; "" si no la informó
}

// QueueURL es una URL registrada en http.sys.
type QueueURL struct {
	Raw    string // tal cual la muestra netsh (http.sys la guarda en mayúsculas)
	Secure bool   // https
	Host   string // "+" y "*" son comodines; si no, un nombre o una IP (sin corchetes)
	Port   uint16
	Path   string // desde la barra inicial
	// Bound es la dirección a la que está atada la URL, si se registró con
	// una IP (http://127.0.0.1:8080/): http.sys la guarda y netsh la muestra
	// como un tercer tramo, «host:puerto:ip» (HTTP://127.0.0.1:8080:127.0.0.1/,
	// HTTP://[::1]:8080:[::1]/). "" si la URL no está atada a una dirección.
	Bound string
}

// String la muestra con el esquema y el host en minúsculas, que es como la
// escribe una persona; el camino queda como lo guardó http.sys.
func (u QueueURL) String() string {
	scheme := "http"
	if u.Secure {
		scheme = "https"
	}
	host := strings.ToLower(u.Host)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host + ":" + strconv.Itoa(int(u.Port)) + u.Path
}

// RequestQueue es una cola de solicitudes de http.sys.
type RequestQueue struct {
	// Name es lo que netsh dice del nombre, sin traducir: el de la cola (en
	// IIS, el del grupo de aplicaciones) o la frase con la que avisa que no
	// tiene (un HttpListener de .NET no le pone nombre a la suya).
	Name       string
	Controller QueueProcess   // quien creó la cola, si netsh lo informó (PID 0 si no)
	Processes  []QueueProcess // los que la atienden, en el orden de netsh
	URLs       []QueueURL     // las URL registradas, en el orden de netsh
}

// On devuelve las URL de la cola que están en ese puerto.
func (q RequestQueue) On(port uint16) []QueueURL {
	var out []QueueURL
	for _, u := range q.URLs {
		if u.Port == port {
			out = append(out, u)
		}
	}
	return out
}

// ParseRequestQueues interpreta la vista de colas de netsh. Una vista sin
// colas (nadie registró nada) es una lista vacía, sin error. Sin la línea de
// guiones, error: eso no es la vista (netsh falló, o la salida es otra cosa),
// y tomarlo por "no hay colas" sería mentir.
func ParseRequestQueues(out string) ([]RequestQueue, error) {
	lines := strings.Split(out, "\n")
	start := -1
	for i, l := range lines {
		if f := strings.Fields(l); len(f) > 0 && isRule(f) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("la salida de netsh no tiene la vista de colas de http.sys")
	}

	// level es una línea ya vista de la que pueden colgar otras.
	type level struct {
		indent  int
		section bool // un título sin valor ("Procesos:"): lo que cuelga de él es una lista
	}
	var (
		queues []RequestQueue
		stack  []level // los ancestros de la línea actual, de afuera hacia adentro
	)
	for _, raw := range lines[start:] {
		line := strings.TrimRight(raw, " \t\r")
		text := strings.TrimLeft(line, " \t")
		if text == "" {
			continue
		}
		indent := indentOf(line)
		if indent == 0 {
			queues = append(queues, RequestQueue{Name: valueAfterColon(text)})
			stack = append(stack[:0], level{indent: 0})
			continue
		}
		if len(queues) == 0 {
			continue // texto suelto antes de la primera cola
		}
		q := &queues[len(queues)-1]
		for len(stack) > 1 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1]
		stack = append(stack, level{indent: indent, section: strings.HasSuffix(text, ":")})

		if u, ok := parseQueueURL(text); ok {
			if !hasURL(q.URLs, u) {
				q.URLs = append(q.URLs, u)
			}
			continue
		}
		p, ok := parseQueueProcess(text, parent.section)
		switch {
		case !ok:
		case parent.section:
			if !hasProcess(q.Processes, p.PID) {
				q.Processes = append(q.Processes, p)
			}
		case parent.indent == 0 && q.Controller.PID == 0:
			q.Controller = p
		}
	}
	return queues, nil
}

// indentOf cuenta la sangría de una línea en columnas (un tabulador vale 8,
// como en una consola; netsh usa espacios).
func indentOf(line string) int {
	n := 0
	for _, c := range line {
		switch c {
		case ' ':
			n++
		case '\t':
			n += 8 - n%8
		default:
			return n
		}
	}
	return n
}

// valueAfterColon es lo que sigue al primer ":" de la línea; "" si no hay.
func valueAfterColon(text string) string {
	_, v, ok := strings.Cut(text, ":")
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}

// parseQueueProcess reconoce una línea de proceso. inList dice si cuelga de un
// título de sección: ahí alcanza con un entero solo (el formato viejo) y la
// ruta es opcional; fuera de una lista se exige la ruta, que es lo único que
// distingue a un proceso de un contador cualquiera ("Solicitudes activas: 0").
func parseQueueProcess(text string, inList bool) (QueueProcess, bool) {
	if inList && isDigits(text) {
		pid, ok := parsePID(text)
		return QueueProcess{PID: pid}, ok
	}
	// «etiqueta: ENTERO, etiqueta: VALOR»
	_, rest, ok := strings.Cut(text, ":")
	if !ok {
		return QueueProcess{}, false
	}
	num, tail, ok := strings.Cut(strings.TrimSpace(rest), ",")
	if !ok {
		return QueueProcess{}, false
	}
	pid, ok := parsePID(strings.TrimSpace(num))
	if !ok {
		return QueueProcess{}, false
	}
	image := valueAfterColon(tail)
	if !absoluteWindowsPath(image) {
		if !inList {
			return QueueProcess{}, false
		}
		image = "" // netsh no pudo leer la imagen: queda el PID
	}
	return QueueProcess{PID: pid, Image: image}, true
}

// parsePID lee un PID: solo dígitos, que quepa en 32 bits y que no sea 0.
func parsePID(s string) (uint32, bool) {
	if !isDigits(s) {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil || v == 0 {
		return 0, false
	}
	return uint32(v), true
}

// absoluteWindowsPath dice si s es una ruta absoluta de Windows: con letra de
// unidad, UNC, o de dispositivo.
func absoluteWindowsPath(s string) bool {
	switch {
	case len(s) >= 3 && isLetter(s[0]) && s[1] == ':' && (s[2] == '\\' || s[2] == '/'):
		return true
	case strings.HasPrefix(s, `\\`), strings.HasPrefix(s, `\Device\`):
		return true
	}
	return false
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// parseQueueURL reconoce una URL registrada: esquema://host[:puerto[:ip]]/camino.
// El host puede ser un comodín de http.sys ("+" es "todos los nombres", "*"
// es "los que nadie más pidió"), así que no pasa por net/url. Sin puerto, el
// del esquema. El tercer tramo es la dirección a la que http.sys ata una URL
// registrada con una IP: sin leerlo, el 8080 de un HttpListener en
// http://127.0.0.1:8080/ quedaba sin dueño a la vista.
func parseQueueURL(text string) (QueueURL, bool) {
	if strings.ContainsAny(text, " \t") {
		return QueueURL{}, false
	}
	u := QueueURL{Raw: text}
	var rest string
	switch low := strings.ToLower(text); {
	case strings.HasPrefix(low, "http://"):
		rest = text[len("http://"):]
	case strings.HasPrefix(low, "https://"):
		rest, u.Secure = text[len("https://"):], true
	default:
		return QueueURL{}, false
	}
	authority, path, _ := strings.Cut(rest, "/")
	u.Path = "/" + path
	host, after, ok := cutHost(authority)
	if !ok || host == "" {
		return QueueURL{}, false
	}
	u.Host = host
	port := ""
	switch {
	case after == "":
	case strings.HasPrefix(after, ":"):
		var bound string
		port, bound, ok = strings.Cut(after[1:], ":")
		if ok {
			b, rest, okB := cutHost(bound)
			a, err := netip.ParseAddr(b)
			if !okB || rest != "" || err != nil || a.Zone() != "" {
				return QueueURL{}, false
			}
			u.Bound = a.String()
		}
	default:
		return QueueURL{}, false
	}
	switch {
	case port == "" && u.Secure:
		u.Port = 443
	case port == "":
		u.Port = 80
	default:
		if !isDigits(port) {
			return QueueURL{}, false
		}
		v, err := strconv.ParseUint(port, 10, 32)
		if err != nil || v == 0 || v > 65535 {
			return QueueURL{}, false
		}
		u.Port = uint16(v)
	}
	return u, true
}

// cutHost separa el host del resto de una autoridad: entre corchetes (una
// IPv6) o hasta el primer ":" (un nombre, un comodín o una IPv4, que no
// tienen dos puntos). ok=false si abre un corchete y no lo cierra.
func cutHost(authority string) (host, after string, ok bool) {
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return "", "", false
		}
		return authority[1:end], authority[end+1:], true
	}
	if i := strings.IndexByte(authority, ':'); i >= 0 {
		return authority[:i], authority[i:], true
	}
	return authority, "", true
}

func hasURL(us []QueueURL, u QueueURL) bool {
	for _, x := range us {
		if strings.EqualFold(x.Raw, u.Raw) {
			return true
		}
	}
	return false
}

func hasProcess(ps []QueueProcess, pid uint32) bool {
	for _, p := range ps {
		if p.PID == pid {
			return true
		}
	}
	return false
}
