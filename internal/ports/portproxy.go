package ports

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Reglas de portproxy: por qué un svchost escucha en un puerto de desarrollo.
//
// `netsh interface portproxy add v4tov4 listenport=3000 connectaddress=…` deja
// una regla de reenvío que atiende el servicio IP Helper (iphlpsvc, dentro de
// un svchost): escucha en el puerto de la regla y le pasa cada conexión a otra
// dirección. Es la receta clásica para llegar desde la red a un servidor que
// corre dentro de WSL 2, y las reglas son PERSISTENTES: quedan en el registro
// y vuelven en cada arranque, mucho después de que nadie se acuerde de
// haberlas puesto. El síntoma es un puerto tomado por "svchost.exe".
//
// Las reglas viven en
//
//	HKLM\SYSTEM\CurrentControlSet\Services\PortProxy\<clase>\tcp
//
// una clave por clase (v4tov4, v4tov6, v6tov4, v6tov6) y un valor de texto por
// regla: el nombre es "dirección/puerto" donde ESCUCHA y el dato es
// "dirección/puerto" adonde CONECTA. Se lee el registro y no la salida de
// `netsh interface portproxy show all`: es la fuente, no está traducida y no
// pide administrador.

// ProxyKind es la clase de una regla: en qué familia escucha y a cuál conecta.
type ProxyKind uint8

const (
	V4ToV4 ProxyKind = iota + 1
	V4ToV6
	V6ToV4
	V6ToV6
)

// ProxyKinds son las cuatro clases, en el orden en que las muestra netsh.
var ProxyKinds = [...]ProxyKind{V4ToV4, V4ToV6, V6ToV4, V6ToV6}

// String es el nombre de la clase en netsh y en el registro ("v4tov4").
func (k ProxyKind) String() string {
	switch k {
	case V4ToV4:
		return "v4tov4"
	case V4ToV6:
		return "v4tov6"
	case V6ToV4:
		return "v6tov4"
	case V6ToV6:
		return "v6tov6"
	}
	return "ProxyKind(" + strconv.Itoa(int(k)) + ")"
}

// ParseProxyKind es la inversa de String, sin distinguir mayúsculas.
func ParseProxyKind(s string) (ProxyKind, bool) {
	for _, k := range ProxyKinds {
		if strings.EqualFold(s, k.String()) {
			return k, true
		}
	}
	return 0, false
}

// ListenFamily es la familia en la que escucha una regla de esa clase.
func (k ProxyKind) ListenFamily() Family {
	if k == V6ToV4 || k == V6ToV6 {
		return IPv6
	}
	return IPv4
}

// ProxyRule es una regla de portproxy.
type ProxyRule struct {
	Kind ProxyKind
	// ListenAddr es dónde escucha: "*" (todas las direcciones: lo que queda
	// si no se dio listenaddress), una IP, o un nombre que netsh aceptó.
	ListenAddr string
	ListenPort uint16
	// Target es adonde conecta, tal cual está en el registro
	// ("172.24.1.5/3000"). Es parte de la identidad de la regla: si cambia
	// entre que se la muestra y que se la borra, es otra regla.
	Target string
}

// ParseProxyRule interpreta un valor del registro: name es "dirección/puerto"
// donde escucha y data es adonde conecta. Es estricto con el lado que escucha
// —de ahí sale el comando que la borra— y tolerante con el destino, que solo
// se muestra y se compara.
func ParseProxyRule(kind ProxyKind, name, data string) (ProxyRule, error) {
	r := ProxyRule{Kind: kind}
	if _, ok := ParseProxyKind(kind.String()); !ok {
		return r, fmt.Errorf("clase de regla desconocida (%d)", int(kind))
	}
	i := strings.LastIndexByte(name, '/')
	if i < 0 {
		return r, fmt.Errorf("la regla %q no tiene la forma dirección/puerto", name)
	}
	addr, port := name[:i], name[i+1:]
	if !validProxyHost(addr) {
		return r, fmt.Errorf("la regla %q escucha en una dirección que no entiendo", name)
	}
	if !isDigits(port) {
		return r, fmt.Errorf("la regla %q no da el puerto como número", name)
	}
	v, err := strconv.ParseUint(port, 10, 32)
	if err != nil || v == 0 || v > 65535 {
		return r, fmt.Errorf("la regla %q tiene el puerto fuera de rango", name)
	}
	if !validProxyTarget(data) {
		return r, fmt.Errorf("la regla %q conecta a un destino que no entiendo", name)
	}
	r.ListenAddr, r.ListenPort, r.Target = addr, uint16(v), data
	return r, nil
}

// validProxyHost: "*", o de 1 a 253 caracteres de los que puede tener una IP
// o un nombre de máquina. Nada que netsh pueda leer como otro argumento: ni
// espacios, ni "=", ni comillas.
func validProxyHost(s string) bool {
	if s == "*" {
		return true
	}
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case isLetter(c), c >= '0' && c <= '9', c == '.', c == ':', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// validProxyTarget: como el host, más la barra que separa el puerto.
func validProxyTarget(s string) bool {
	if len(s) == 0 || len(s) > 300 {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part != "" && !validProxyHost(part) {
			return false
		}
	}
	return true
}

// Key es la identidad de la regla: la clase y dónde escucha. No puede haber
// dos reglas con la misma (son el nombre del valor dentro de su clave).
func (r ProxyRule) Key() string {
	return "portproxy/" + r.Kind.String() + "/" + r.ListenAddr + "/" + strconv.Itoa(int(r.ListenPort))
}

// Same dice si o es la MISMA regla y conecta al mismo destino.
func (r ProxyRule) Same(o ProxyRule) bool {
	return r.Kind == o.Kind && r.ListenPort == o.ListenPort &&
		strings.EqualFold(r.ListenAddr, o.ListenAddr) && strings.EqualFold(r.Target, o.Target)
}

// String muestra la regla: "0.0.0.0:3000 → 172.24.1.5:3000".
func (r ProxyRule) String() string {
	return joinHostPort(r.ListenAddr, strconv.Itoa(int(r.ListenPort))) + " → " + r.target()
}

// target muestra el destino como dirección:puerto.
func (r ProxyRule) target() string {
	i := strings.LastIndexByte(r.Target, '/')
	if i < 0 {
		return r.Target
	}
	return joinHostPort(r.Target[:i], r.Target[i+1:])
}

func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return host + ":" + port
}

// Holds dice si ese socket —uno que la tabla le atribuye al IP Helper— es el
// de esta regla: TCP, el mismo puerto, la familia en la que escucha la clase y
// la misma dirección. Una regla que escucha en "*" ata la dirección comodín.
// Si la dirección de la regla es un nombre, no se la puede comparar sin
// resolverlo: alcanza con el puerto y la familia.
func (r ProxyRule) Holds(s Socket) bool {
	if s.Proto != TCP || s.Port() != r.ListenPort || s.Family() != r.Kind.ListenFamily() {
		return false
	}
	local := s.Local.Addr().WithZone("")
	if r.ListenAddr == "*" {
		return local.IsUnspecified()
	}
	addr, err := netip.ParseAddr(r.ListenAddr)
	if err != nil {
		return true
	}
	return addr == local
}

// DeleteArgs son los argumentos de netsh que borran la regla. Sin
// listenaddress cuando escucha en "*": así es como netsh nombra a la regla
// que se agregó sin dirección.
func (r ProxyRule) DeleteArgs() []string {
	args := []string{"interface", "portproxy", "delete", r.Kind.String(), "listenport=" + strconv.Itoa(int(r.ListenPort))}
	if r.ListenAddr != "*" {
		args = append(args, "listenaddress="+r.ListenAddr)
	}
	return append(args, "protocol=tcp")
}

// RegistryPath es la clave de HKLM donde viven las reglas de esa clase.
func (k ProxyKind) RegistryPath() string {
	return `SYSTEM\CurrentControlSet\Services\PortProxy\` + k.String() + `\tcp`
}
