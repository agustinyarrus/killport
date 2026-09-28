package killport

// tapados.go: los puertos que no se liberan aunque se cierre a todos los
// objetivos. En el 5353 escuchan a la vez el servicio de red de Chrome, el
// overlay de NVIDIA, adb… y Dnscache, que es de Windows y no se toca: cerrar a
// los cuatro primeros no libera nada, porque Dnscache sigue ahí. Preguntar
// "¿Cerrar 4 procesos y liberar el 5353?" es prometer algo que no va a pasar,
// y con --yes, cerrar cuatro programas por nada.
//
// Puro.

import (
	"slices"
	"strings"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
)

// tapado es un puerto de los objetivos que una negativa sigue ocupando.
type tapado struct {
	port  uint16
	proto ports.Proto
	quien string // quién lo sigue ocupando: el servicio, o el proceso
}

// tapadosDe son los puertos de los objetivos que, aunque se los cierre a
// todos, siguen ocupados por una negativa del mismo pedido (después de Focus:
// las negativas que quedan son las que contestan lo pedido). Una negativa
// cuenta en el puerto y protocolo de sus sockets. No cuenta la de un dueño que
// murió sin herederos: ese socket se suelta solo. En orden de puerto.
// O(sockets).
func tapadosDe(plan reaper.Plan) []tapado {
	type pp struct {
		port  uint16
		proto ports.Proto
	}
	quien := map[pp]string{}
	for _, r := range negativasVistas(plan.Refusals) {
		if r.Owner.Zero() && r.PID > 4 {
			continue // un fantasma sin heredero: se libera solo
		}
		nombre := r.Service
		if nombre == "" {
			nombre = nombreVisible(r.Name, r.PID)
		}
		for _, s := range r.Sockets {
			k := pp{s.Port(), s.Proto}
			if _, ok := quien[k]; !ok {
				quien[k] = nombre
			}
		}
	}
	var out []tapado
	visto := map[pp]bool{}
	for _, tg := range plan.Targets {
		for _, s := range tg.Sockets {
			k := pp{s.Port(), s.Proto}
			if q, ok := quien[k]; ok && !visto[k] && tg.Ports.Contains(k.port) {
				visto[k] = true
				out = append(out, tapado{port: k.port, proto: k.proto, quien: q})
			}
		}
	}
	slices.SortFunc(out, func(a, b tapado) int { return int(a.port) - int(b.port) })
	return out
}

// puertosDeTapados es el conjunto de puertos tapados.
func puertosDeTapados(ts []tapado) ports.Set {
	var ps []uint16
	for _, t := range ts {
		ps = append(ps, t.port)
	}
	return ports.Of(ps...)
}

// textoTapados: "el 5353 sigue ocupado por Dnscache", "el 5353 y el 5355
// siguen ocupados por Dnscache".
func textoTapados(ts []tapado) string {
	por := map[string][]string{}
	var orden []string
	for _, t := range ts {
		if _, ok := por[t.quien]; !ok {
			orden = append(orden, t.quien)
		}
		por[t.quien] = append(por[t.quien], "el "+u32(uint32(t.port)))
	}
	var partes []string
	for _, q := range orden {
		sigue := "sigue ocupado"
		if len(por[q]) > 1 {
			sigue = "siguen ocupados"
		}
		partes = append(partes, unirY(por[q])+" "+sigue+" por "+q)
	}
	return strings.Join(partes, "; ")
}

// multidifusion nombra los puertos UDP de multidifusión: los comparten
// varios programas a propósito (SO_REUSEADDR) y cada uno recibe su copia de
// lo que llega. Nadie necesita tenerlos solo. "" si no es uno de esos.
func multidifusion(port uint16, proto ports.Proto) string {
	if proto != ports.UDP {
		return ""
	}
	switch port {
	case 5353:
		return "mDNS"
	case 5355:
		return "LLMNR"
	case 1900:
		return "SSDP"
	case 3702:
		return "WS-Discovery"
	}
	return ""
}
