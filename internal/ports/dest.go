package ports

import (
	"net/netip"
	"slices"
	"strings"
)

// A quién apunta un pedido.
//
// "killport 5173" es todo lo que ocupa el 5173. "killport localhost:5173" es
// otra cosa, y la diferencia importa: dos procesos pueden escuchar el mismo
// puerto en direcciones distintas —uno en [::1], otro en 0.0.0.0— sin que
// ninguno dé error, y cada cliente llega a UNO. El navegador prueba ::1
// primero; `curl 127.0.0.1:5173`, al otro. Es el "puerto fantasma" que nadie
// explica: la página muestra la versión vieja y no hubo EADDRINUSE. Así que
// la dirección que escribe el usuario apunta:
//
//   - localhost: al que contesta localhost (::1, y si ahí no hay nadie,
//     127.0.0.1: lo que hacen el navegador y Node ≥ 20);
//   - una IP (127.0.0.1, ::1, la de la red): al que contesta esa dirección;
//   - un comodín (0.0.0.0, [::]): nadie se conecta ahí; es como netstat y el
//     listado nombran a lo que está atado a todas las interfaces, así que
//     elige a lo que está atado EXACTAMENTE ahí;
//   - "*", o nada (":5173"): todo el puerto;
//   - un nombre (api.local): no se resuelve —un nombre puede apuntar a
//     cualquier lado, y killport no pregunta al DNS para decidir qué cerrar—:
//     vale todo el puerto, y el nombre queda anotado para decirlo.
//
// Quién contesta cada dirección lo decide Answer (reach.go).

// DestKind es la clase de dirección de un pedido.
type DestKind uint8

const (
	DestAddr      DestKind = iota + 1 // una dirección concreta, la que usa un cliente para conectarse
	DestLocalhost                     // "localhost": ::1 y, si ahí no contesta nadie, 127.0.0.1
	DestBound                         // un comodín: elige a lo que está atado exactamente ahí
)

// Dest es una dirección de un pedido.
type Dest struct {
	Kind DestKind
	Addr netip.Addr // DestAddr y DestBound; inválida en DestLocalhost
}

// Localhost es el destino "localhost".
var Localhost = Dest{Kind: DestLocalhost}

// DestOf clasifica el host de un ítem: ok=false si no apunta (vacío, "*" o un
// nombre que no es localhost). host es como vino (una IPv6 sin corchetes).
func DestOf(host string) (Dest, bool) {
	switch h := strings.TrimSpace(host); {
	case h == "" || h == "*":
		return Dest{}, false
	case strings.EqualFold(h, "localhost"):
		return Localhost, true
	default:
		a, err := netip.ParseAddr(h)
		if err != nil {
			return Dest{}, false
		}
		a = a.Unmap() // ::ffff:127.0.0.1 es 127.0.0.1 para quien lo atiende
		if a.IsUnspecified() {
			return Dest{Kind: DestBound, Addr: a}, true
		}
		return Dest{Kind: DestAddr, Addr: a}, true
	}
}

// String nombra el destino como lo escribiría el usuario: "localhost",
// "127.0.0.1", "[::1]".
func (d Dest) String() string {
	switch {
	case d.Kind == DestLocalhost:
		return "localhost"
	case !d.Addr.IsValid():
		return ""
	case d.Addr.Is6():
		return "[" + d.Addr.String() + "]"
	}
	return d.Addr.String()
}

// Request es lo que pidió el usuario: los puertos y, de los que vinieron con
// una dirección, a cuál apunta. El valor cero no pide nada; Whole(s) pide los
// puertos de s enteros (lo que era todo pedido antes de que hubiera
// direcciones).
type Request struct {
	Ports Set // todos los puertos pedidos, con dirección o sin ella
	whole Set // los pedidos enteros
	aimed []aimedRange
	// Names son los hosts con nombre que no se resolvieron (se tomó el puerto
	// entero), sin repetir, en el orden en que vinieron: para decírselo al
	// usuario, no para decidir nada.
	Names []string
}

// aimedRange es un rango pedido con una dirección.
type aimedRange struct {
	r Range
	d Dest
}

// Whole es el pedido de los puertos de s, enteros.
func Whole(s Set) Request { return Request{Ports: s, whole: s} }

// ParseRequest interpreta todos los argumentos (como ParseSpecs) y recuerda
// la dirección de cada rango. Pedir un puerto entero y además con dirección
// (`killport 3000 localhost:3000`) es pedirlo entero: gana lo más amplio.
func ParseRequest(args []string) (Request, error) {
	var q Request
	for _, a := range args {
		items, err := parseArg(a)
		if err != nil {
			return Request{}, err
		}
		for _, it := range items {
			q.add(it)
		}
	}
	return q, nil
}

func (q *Request) add(it specItem) {
	span := NewSet(it.r)
	q.Ports = q.Ports.Union(span)
	d, ok := DestOf(it.host)
	if !ok {
		q.whole = q.whole.Union(span)
		if h := strings.TrimSpace(it.host); h != "" && h != "*" && !slices.Contains(q.Names, h) {
			q.Names = append(q.Names, h)
		}
		return
	}
	for _, x := range q.aimed {
		if x.r == it.r && x.d == d {
			return
		}
	}
	q.aimed = append(q.aimed, aimedRange{r: it.r, d: d})
}

// Dests son las direcciones a las que apunta el pedido en el puerto p, en el
// orden en que se escribieron; nil si p se pidió entero (o no se pidió). O(k)
// con k los rangos pedidos con dirección.
func (q Request) Dests(p uint16) []Dest {
	if q.whole.Contains(p) {
		return nil
	}
	var out []Dest
	for _, x := range q.aimed {
		if x.r.Contains(p) && !slices.Contains(out, x.d) {
			out = append(out, x.d)
		}
	}
	return out
}

// Aimed dice si algún puerto del pedido apunta a una dirección.
func (q Request) Aimed() bool {
	for _, x := range q.aimed {
		if !NewSet(x.r).Minus(q.whole).IsEmpty() {
			return true
		}
	}
	return false
}

// Label nombra el puerto p como lo pidió el usuario: "localhost:5173",
// "127.0.0.1:5173 y [::1]:5173", o "puerto 5173" si lo pidió entero.
func (q Request) Label(p uint16) string {
	ds := q.Dests(p)
	if len(ds) == 0 {
		return "puerto " + itoa16(p)
	}
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = d.String() + ":" + itoa16(p)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " y " + parts[len(parts)-1]
}

func itoa16(p uint16) string {
	var b [5]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + p%10)
		p /= 10
		if p == 0 {
			return string(b[i:])
		}
	}
}
