package killport

// alcance.go: dónde escucha cada cosa y quién contesta. La columna DIRECCIÓN
// dice las direcciones de verdad (un socket en 127.0.0.1 no es uno en
// 0.0.0.0, aunque sean de la misma familia); ALCANCE dice qué quiere decir eso
// para el usuario: solo esta PC, toda la red, o una interfaz con su nombre. Y un
// puerto que escuchan dos procesos en direcciones distintas —uno en [::1],
// otro en 0.0.0.0, y ninguno dio error— se marca, con quién contesta cada
// destino: es el "puerto fantasma" del navegador que muestra la app vieja.
//
// Puro: las reglas de quién contesta son las de ports.Answer (medidas).

import (
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/agustinyarrus/killport/internal/ports"
)

// direccionesDe son las direcciones locales distintas de unos sockets: IPv4
// antes que IPv6, y dentro de cada familia en orden.
func direccionesDe(socks []ports.Socket) []netip.Addr {
	var out []netip.Addr
	for _, s := range socks {
		a := s.Local.Addr().Unmap()
		if a.IsValid() && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b netip.Addr) int { return a.Compare(b) })
	return out
}

// textoDireccion escribe una dirección como netstat: la IPv6 entre corchetes.
func textoDireccion(a netip.Addr) string {
	if a.Is6() {
		return "[" + a.String() + "]"
	}
	return a.String()
}

// textoDirecciones es la columna DIRECCIÓN: "∗ v4+v6" si están los dos
// comodines (lo de siempre de un servidor que escucha en todas partes; una
// dirección concreta de más no agrega nada: los comodines ya la cubren, como
// el [::1] de wslrelay en un puerto que publica Docker), y si no, las
// direcciones tal cual: "127.0.0.1", "[::1]", "0.0.0.0 [::1]".
func textoDirecciones(as []netip.Addr) string {
	if slices.Contains(as, netip.IPv4Unspecified()) && slices.Contains(as, netip.IPv6Unspecified()) {
		return "∗ v4+v6"
	}
	parts := make([]string, len(as))
	for i, a := range as {
		parts[i] = textoDireccion(a)
	}
	return strings.Join(parts, " ")
}

// alcance es hasta dónde llega lo que escucha en unas direcciones.
type alcance struct {
	texto      string   // "esta PC", "toda la red (v4)", "Wi-Fi", "esta PC + Wi-Fi"
	clave      string   // para el JSON: esta_pc | red | interfaces
	interfaces []string // los nombres de las interfaces concretas
}

// alcanceDe resume las direcciones de lo que escucha. nombre da el nombre de
// la interfaz de una dirección concreta ("" si no se sabe: se muestra la
// dirección). Lo que escucha SOLO en IPv6 lo dice —"esta PC (v6)"—
// porque es la sorpresa de siempre: `curl 127.0.0.1` o un cliente viejo no le
// llegan, aunque el navegador sí. Solo en IPv4 es lo esperado (el navegador
// cae a 127.0.0.1 si ::1 no contesta) y no se marca: sería ruido en casi
// todas las filas. La dirección exacta la dice DIRECCIÓN.
func alcanceDe(as []netip.Addr, nombre func(netip.Addr) string) alcance {
	var comodin, lo [2]bool // [v4, v6]
	var concretas, cortas []string
	for _, a := range as {
		fam := 0
		if a.Is6() {
			fam = 1
		}
		switch ports.ReachOf(a) {
		case ports.ReachAll:
			comodin[fam] = true
		case ports.ReachLoopback:
			lo[fam] = true
		case ports.ReachIface:
			n := ""
			if nombre != nil {
				n = nombre(a)
			}
			if n == "" {
				n = textoDireccion(a)
			}
			if !slices.Contains(concretas, n) {
				concretas = append(concretas, n)
				cortas = append(cortas, interfazCorta(n))
			}
		}
	}
	familia := func(f [2]bool) string {
		if f[1] && !f[0] {
			return " (v6)"
		}
		return ""
	}
	if comodin[0] || comodin[1] {
		return alcance{texto: "toda la red" + familia(comodin), clave: "red"}
	}
	var parts []string
	if lo[0] || lo[1] {
		parts = append(parts, "esta PC"+familia(lo))
	}
	parts = append(parts, cortas...)
	al := alcance{texto: strings.Join(parts, " + "), clave: "esta_pc", interfaces: concretas}
	if len(concretas) > 0 {
		al.clave = "interfaces"
	}
	return al
}

// interfazCorta es el nombre de una interfaz para una columna: el de un
// adaptador de Hyper-V ("vEthernet (WSL (Hyper-V firewall))") sin el envoltorio
// de "vEthernet (…)", que en esta máquina son casi todos. El JSON lleva el
// nombre entero.
func interfazCorta(n string) string {
	if strings.HasPrefix(n, "vEthernet (") && strings.HasSuffix(n, ")") {
		return n[len("vEthernet (") : len(n)-1]
	}
	return n
}

// ocupante es alguien que escucha un puerto: una fila del listado, o un
// objetivo, una negativa o alguien al margen de un plan.
type ocupante struct {
	nombre  string // para leer: "node.exe (PID 812)", "el contenedor db-1"
	proceso string // el dato: "node.exe", "db-1" (sin el PID)
	pid     uint32 // el que figura en la tabla
	socks   []ports.Socket
	// externo: no es un proceso de Windows sino lo que está detrás de un
	// intermediario (un contenedor, un proceso Linux).
	externo bool
}

// compartido es un puerto TCP que escuchan dos o más ocupantes a la vez.
type compartido struct {
	port      uint16
	ocupantes []ocupante  // los que lo escuchan, en orden (con sus sockets en ese puerto)
	contestan []respuesta // quién contesta cada destino (solo los que contesta alguien)
}

// respuesta agrupa los destinos a los que llega el mismo ocupante (o el mismo
// empate de ocupantes).
type respuesta struct {
	destinos []string // "localhost", "127.0.0.1", "[::1]", "la red por IPv4"…
	quien    []int    // índices en compartido.ocupantes: uno, o varios si Windows reparte sin criterio fijo
}

// compartidos encuentra los puertos TCP que escuchan dos o más ocupantes
// distintos y calcula a quién llega cada destino. Solo TCP: en UDP compartir
// un puerto es lo normal (mDNS en el 5353, SSDP en el 1900: cada uno recibe
// su copia de lo que llega por multidifusión), y marcarlo sería ruido.
// O(sockets) más Answer por destino.
func compartidos(ocs []ocupante) []compartido {
	por := map[uint16][]int{}
	var orden []uint16
	for i, o := range ocs {
		for _, s := range o.socks {
			if s.Proto != ports.TCP || s.State != ports.Listen {
				continue
			}
			p := s.Port()
			if _, ok := por[p]; !ok {
				orden = append(orden, p)
			}
			if !slices.Contains(por[p], i) {
				por[p] = append(por[p], i)
			}
		}
	}
	slices.Sort(orden)
	var out []compartido
	for _, p := range orden {
		if len(por[p]) < 2 {
			continue
		}
		c := compartido{port: p}
		// De quién es cada socket de ese puerto.
		var socks []ports.Socket
		dueño := map[sockID]int{}
		for _, i := range por[p] {
			o := ocupante{nombre: ocs[i].nombre, proceso: ocs[i].proceso, pid: ocs[i].pid, externo: ocs[i].externo}
			for _, s := range ocs[i].socks {
				if s.Proto == ports.TCP && s.Port() == p {
					socks = append(socks, s)
					o.socks = append(o.socks, s)
					dueño[idSock(s)] = len(c.ocupantes)
				}
			}
			c.ocupantes = append(c.ocupantes, o)
		}
		quien := func(who []ports.Socket) []int {
			var out []int
			for _, w := range who {
				if i, ok := dueño[idSock(w)]; ok && !slices.Contains(out, i) {
					out = append(out, i)
				}
			}
			return out
		}
		lh, _ := ports.AnswerDest(ports.Localhost, ports.TCP, p, socks)
		destinos := []struct {
			nombre string
			who    []ports.Socket
		}{
			{"localhost", lh},
			{"127.0.0.1", ports.Answer(netip.AddrFrom4([4]byte{127, 0, 0, 1}), ports.TCP, p, socks)},
			{"[::1]", ports.Answer(netip.IPv6Loopback(), ports.TCP, p, socks)},
			{destRedV4, ports.AnswerNet(ports.IPv4, ports.TCP, p, socks)},
			{destRedV6, ports.AnswerNet(ports.IPv6, ports.TCP, p, socks)},
		}
		for _, d := range destinos {
			q := quien(d.who)
			if len(q) == 0 {
				continue
			}
			k := slices.IndexFunc(c.contestan, func(r respuesta) bool { return slices.Equal(r.quien, q) })
			if k < 0 {
				c.contestan = append(c.contestan, respuesta{quien: q})
				k = len(c.contestan) - 1
			}
			c.contestan[k].destinos = append(c.contestan[k].destinos, d.nombre)
		}
		out = append(out, c)
	}
	return out
}

// Los destinos "de la red", como se leen.
const (
	destRedV4 = "la red por IPv4"
	destRedV6 = "la red por IPv6"
)

// sockID identifica un socket de la foto (dueño, dirección y hora).
type sockID struct {
	pid   uint32
	local netip.AddrPort
	since int64
}

func idSock(s ports.Socket) sockID {
	id := sockID{pid: s.PID, local: s.Local}
	if !s.Since.IsZero() {
		id.since = s.Since.UnixNano()
	}
	return id
}

// lineas cuenta un puerto compartido: quién lo escucha y a quién llega cada
// destino, con la dirección donde escucha cada uno.
func (c compartido) lineas() (titulo string, detalle []string) {
	// Dos contenedores del mismo motor no son dos procesos (los dos sockets
	// son de com.docker.backend): son dos servidores.
	que := "procesos"
	for _, o := range c.ocupantes {
		if o.externo {
			que = "servidores"
		}
	}
	titulo = strconv.Itoa(int(c.port)) + ": lo escuchan " + strconv.Itoa(len(c.ocupantes)) + " " + que + " distintos a la vez, y ninguno dio error"
	for _, r := range c.contestan {
		nombres := make([]string, len(r.quien))
		for i, q := range r.quien {
			o := c.ocupantes[q]
			nombres[i] = o.nombre + " en " + textoDirecciones(direccionesDe(o.socks))
		}
		quien := unirY(nombres)
		if len(r.quien) > 1 {
			quien = "reparto indefinido entre " + quien + " (Windows no promete a cuál)"
		}
		detalle = append(detalle, unirY(r.destinos)+" → "+quien)
	}
	return titulo, detalle
}

// unirY une "a", "a y b", "a, b y c".
func unirY(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " y " + xs[len(xs)-1]
}

// --- JSON (puro: lo usan el listado y el reporte) ---------------------------

// jsonDueño identifica a un proceso en el JSON.
type jsonDueño struct {
	PID     uint32 `json:"pid"`
	Proceso string `json:"proceso,omitempty"`
}

// jsonCompartido es un puerto TCP que escuchan varios procesos a la vez, y a
// quién llega cada destino (localhost, 127.0.0.1, [::1], la red por IPv4 o
// IPv6). indefinido: en esa dirección hay más de uno y Windows no promete a
// cuál llega.
type jsonCompartido struct {
	Puerto    uint16         `json:"puerto"`
	Procesos  []jsonDueño    `json:"procesos"`
	Contestan []jsonContesta `json:"contestan"`
}

// jsonContesta es a quién llegan unos destinos. destinos: localhost,
// 127.0.0.1, ::1 (las direcciones, sin corchetes); red: ipv4 | ipv6 (lo que
// llega de la red por esa familia).
type jsonContesta struct {
	Destinos   []string `json:"destinos"`
	Red        []string `json:"red,omitempty"`
	PIDs       []uint32 `json:"pids"`
	Indefinido bool     `json:"indefinido,omitempty"`
}

func compartidosJSON(cs []compartido) []jsonCompartido {
	var out []jsonCompartido
	for _, c := range cs {
		jc := jsonCompartido{Puerto: c.port, Procesos: []jsonDueño{}, Contestan: []jsonContesta{}}
		for _, o := range c.ocupantes {
			jc.Procesos = append(jc.Procesos, jsonDueño{PID: o.pid, Proceso: o.proceso})
		}
		for _, r := range c.contestan {
			jx := jsonContesta{Destinos: []string{}, Indefinido: len(r.quien) > 1}
			for _, d := range r.destinos {
				switch d {
				case destRedV4:
					jx.Red = append(jx.Red, "ipv4")
				case destRedV6:
					jx.Red = append(jx.Red, "ipv6")
				default:
					jx.Destinos = append(jx.Destinos, strings.Trim(d, "[]"))
				}
			}
			for _, q := range r.quien {
				jx.PIDs = append(jx.PIDs, c.ocupantes[q].pid)
			}
			jc.Contestan = append(jc.Contestan, jx)
		}
		out = append(out, jc)
	}
	return out
}
