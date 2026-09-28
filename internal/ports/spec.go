package ports

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// Gramática de lo que se puede pedir (una o varias cosas por argumento):
//
//	argumento := ítem { separador ítem }        separador := "," | espacio
//	ítem      := url | [host] ":" rango | "[" ipv6 "]" ":" rango | rango
//	rango     := puerto [ ("-" | "–") puerto ]   (dado vuelta se acepta)
//	puerto    := dígitos ASCII, de 1 a 65535
//	url       := esquema "://" [usuario "@"] host [":" puerto] [camino]
//
// Una URL se come todo hasta el próximo espacio, comas incluidas: la coma es
// legal en el camino de una URL (RFC 3986), así que cortarla ahí partiría
// "http://x/a,b" en dos. Sin puerto explícito vale el del esquema (http 80,
// https 443, …).
//
// El host, si viene, APUNTA (ver dest.go): "localhost:5173" es el servidor que
// contesta localhost, no todo lo que escucha el 5173. Un puerto puede tener
// dos dueños en direcciones distintas —uno en [::1] y otro en 0.0.0.0, y
// ninguno dio error— y el que el usuario ve en el navegador es uno solo.
// ParseSpec y ParseSpecs dan solo los puertos; ParseRequest, también a quién.

// defaultPorts son los puertos implícitos de los esquemas que un desarrollador
// suele pegar desde el navegador o de una cadena de conexión.
var defaultPorts = map[string]uint16{
	"http": 80, "https": 443, "ws": 80, "wss": 443,
	"ftp": 21, "ssh": 22,
	"postgres": 5432, "postgresql": 5432, "mysql": 3306,
	"redis": 6379, "mongodb": 27017, "amqp": 5672,
}

const ejemplos = "ej.: 3000, 5000-5003, :8080, localhost:3000, http://localhost:5173/"

// ParseSpec interpreta un argumento de la línea de comandos. Errores en
// español que dicen qué parte está mal y cómo se escribe bien.
func ParseSpec(arg string) (Set, error) {
	items, err := parseArg(arg)
	if err != nil {
		return Set{}, err
	}
	rs := make([]Range, len(items))
	for i, it := range items {
		rs[i] = it.r
	}
	return NewSet(rs...), nil
}

// specItem es un pedazo de un argumento: un rango y el host que lo precedía
// ("" si no había).
type specItem struct {
	r    Range
	host string
}

// parseArg parte un argumento en sus ítems.
func parseArg(arg string) ([]specItem, error) {
	var out []specItem
	for _, field := range strings.Fields(arg) {
		rest := field
		for rest != "" {
			item, tail, _ := strings.Cut(rest, ",")
			if strings.Contains(item, "://") {
				item, tail = rest, "" // la URL llega hasta el espacio
			}
			rest = tail
			if item == "" {
				continue // "3000,,3001" o una coma final: no es ambiguo, se tolera
			}
			r, host, err := parseItem(item)
			if err != nil {
				return nil, err
			}
			out = append(out, specItem{r: r, host: host})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%q no dice ningún puerto (%s)", arg, ejemplos)
	}
	return out, nil
}

// ParseSpecs une lo pedido en todos los argumentos. Cada error ya cita el
// fragmento culpable entre comillas.
func ParseSpecs(args []string) (Set, error) {
	var all Set
	for _, a := range args {
		s, err := ParseSpec(a)
		if err != nil {
			return Set{}, err
		}
		all = all.Union(s)
	}
	return all, nil
}

// parseItem interpreta un ítem y devuelve, además del rango, el host que lo
// precedía ("" si no había; una IPv6 sin corchetes).
func parseItem(item string) (Range, string, error) {
	switch {
	case strings.Contains(item, "://"):
		return parseURL(item)
	case strings.HasPrefix(item, "["):
		end := strings.Index(item, "]")
		if end < 0 {
			return Range{}, "", fmt.Errorf("%q: falta el ] que cierra la dirección IPv6", item)
		}
		host := item[1:end]
		if a, err := netip.ParseAddr(host); err != nil || !a.Is6() {
			return Range{}, "", fmt.Errorf("%q: %q no es una dirección IPv6", item, host)
		}
		after := item[end+1:]
		if !strings.HasPrefix(after, ":") || len(after) == 1 {
			return Range{}, "", fmt.Errorf("%q no dice el puerto (ej.: [::1]:8080)", item)
		}
		r, err := parseRangeWithPath(item, after[1:])
		return r, host, err
	case strings.Count(item, ":") > 1:
		return Range{}, "", fmt.Errorf("%q: una dirección IPv6 va entre corchetes, como [::1]:8080", item)
	case strings.Contains(item, ":"):
		host, port, _ := strings.Cut(item, ":")
		if err := checkHost(item, host); err != nil {
			return Range{}, "", err
		}
		if port == "" {
			return Range{}, "", fmt.Errorf("%q no dice el puerto (ej.: %s3000)", item, host+":")
		}
		r, err := parseRangeWithPath(item, port)
		return r, host, err
	}
	if p, suf, ok := strings.Cut(item, "/"); ok {
		switch strings.ToLower(suf) {
		case "tcp", "udp":
			return Range{}, "", fmt.Errorf("%q: el protocolo va aparte, con --%s (%s %s)", item, strings.ToLower(suf), p, "--"+strings.ToLower(suf))
		}
		return Range{}, "", fmt.Errorf("%q no es un puerto (%s)", item, ejemplos)
	}
	if looksLikeHost(item) {
		return Range{}, "", fmt.Errorf("%q no dice el puerto (ej.: %s:3000)", item, item)
	}
	r, err := parseRange(item, item)
	return r, "", err
}

// looksLikeHost reconoce un host pelado ("localhost", "127.0.0.1",
// "api.local") para decir que falta el puerto en vez de "no es un puerto".
func looksLikeHost(s string) bool {
	if a, err := netip.ParseAddr(s); err == nil && a.Is4() {
		return true
	}
	if s == "localhost" {
		return true
	}
	hasLetter := strings.IndexFunc(s, unicode.IsLetter) >= 0
	return hasLetter && strings.Contains(s, ".") && isHostName(s)
}

// parseRangeWithPath acepta "3000/app" después de un host (lo que queda al
// copiar la barra de direcciones sin el esquema): el camino no importa.
func parseRangeWithPath(item, s string) (Range, error) {
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return parseRange(item, s)
}

func parseRange(item, s string) (Range, error) {
	a, b, isRange := cutDash(s)
	if !isRange {
		p, err := parsePort(item, s)
		return Range{p, p}, err
	}
	switch {
	case a == "" && b == "":
		return Range{}, fmt.Errorf("%q no es un rango (ej.: 5000-5003)", item)
	case a == "":
		return Range{}, fmt.Errorf("%q: al rango le falta el principio (ej.: 5000-5003)", item)
	case b == "":
		return Range{}, fmt.Errorf("%q: al rango le falta el final (ej.: 5000-5003)", item)
	}
	lo, err := parsePort(item, a)
	if err != nil {
		return Range{}, err
	}
	hi, err := parsePort(item, b)
	if err != nil {
		return Range{}, err
	}
	return R(lo, hi), nil
}

// cutDash parte en el guion o la raya corta (la que escribe Set.String).
func cutDash(s string) (a, b string, ok bool) {
	if a, b, ok = strings.Cut(s, "-"); ok {
		return a, b, true
	}
	return strings.Cut(s, "–")
}

func parsePort(item, s string) (uint16, error) {
	if !isDigits(s) {
		if item == s {
			return 0, fmt.Errorf("%q no es un puerto (%s)", item, ejemplos)
		}
		return 0, fmt.Errorf("%q: %q no es un número de puerto", item, s)
	}
	// Solo dígitos ASCII: strconv aceptaría "+3000", y un signo acá es un error
	// de tipeo, no una intención.
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n > 65535 {
		return 0, fmt.Errorf("%s está fuera de rango: los puertos van del 1 al 65535", s)
	}
	if n == 0 {
		return 0, errors.New("el puerto 0 no existe: pedirlo es pedirle al sistema «cualquiera libre»; los puertos van del 1 al 65535")
	}
	return uint16(n), nil
}

func parseURL(item string) (Range, string, error) {
	u, err := url.Parse(item)
	if err != nil {
		return Range{}, "", fmt.Errorf("%q no es una URL válida", item)
	}
	scheme := strings.ToLower(u.Scheme)
	if u.Host == "" {
		return Range{}, "", fmt.Errorf("%q: a la URL le falta el host (ej.: http://localhost:5173/)", item)
	}
	host := u.Hostname()
	if err := checkHost(item, host); err != nil {
		return Range{}, "", err
	}
	if ps := u.Port(); ps != "" {
		p, err := parsePort(item, ps)
		return Range{p, p}, host, err
	}
	p, ok := defaultPorts[scheme]
	if !ok {
		return Range{}, "", fmt.Errorf("%q: no sé qué puerto usa %s://; escribilo en la URL (ej.: %s://%s:PUERTO/)", item, scheme, scheme, host)
	}
	return Range{p, p}, host, nil
}

// checkHost valida lo que va antes de ":puerto". Vacío (":3000") y "*" (como
// lo escribe netstat) valen; una IP tiene que ser una IP; un nombre, letras,
// dígitos, puntos, guiones y guiones bajos.
func checkHost(item, host string) error {
	switch {
	case host == "" || host == "*":
		return nil
	case isDigitsAndDots(host):
		if _, err := netip.ParseAddr(host); err != nil {
			return fmt.Errorf("%q: %q no es una dirección IPv4", item, host)
		}
		return nil
	case strings.Contains(host, ":"):
		if _, err := netip.ParseAddr(host); err != nil {
			return fmt.Errorf("%q: %q no es una dirección IPv6", item, host)
		}
		return nil
	case isHostName(host):
		return nil
	}
	return fmt.Errorf("%q: %q no es un nombre de host", item, host)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isDigitsAndDots(s string) bool {
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && s[i] != '.' {
			return false
		}
	}
	return true
}

func isHostName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
