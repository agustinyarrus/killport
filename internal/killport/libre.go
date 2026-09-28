package killport

// libre.go: `killport --free`, el puerto libre DE VERDAD (ports.FindFree): el
// primero que no escucha nadie en ninguna dirección, que no tapa WinNAT, que
// no está en el rango que Windows reparte solo y que se deja atar en
// exclusiva en IPv4 y en IPv6. Y la sugerencia que usa lo mismo: cuando un
// puerto pedido no se pudo liberar, cuál está libre cerca.
//
// Puro: armar la consulta a partir de la línea de comandos, contar por qué se
// salteó cada tramo y el documento JSON. Lo que lee el sistema está en
// libre_windows.go.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Dónde busca --free sin posicional: los puertos "registrados" de la IANA,
// arriba de los de sistema (que piden administrador en otros sistemas y
// algunos programas rechazan) y debajo del rango dinámico de fábrica.
const (
	libreDesde = 1024
	libreHasta = 49151
)

// consultaLibre arma la búsqueda de --free. pos es el posicional (a lo sumo
// uno): "3000" busca del 3000 para arriba (el final es implícito, así que el
// rango dinámico se saltea), "3000-3999" busca ahí y nada más (explícito: si
// cae en el rango dinámico, vale, porque se pidió). tcp/udp son los flags:
// ninguno = TCP (lo que usa un servidor de desarrollo), los dos = libre en
// los dos.
func consultaLibre(pos []string, tcp, udp bool, count int) (ports.FreeQuery, error) {
	q := ports.FreeQuery{Within: ports.Range{Lo: libreDesde, Hi: libreHasta}, Count: max(count, 1)}
	q.Protos = protosPedidos(tcp, udp, ports.TCP)
	switch len(pos) {
	case 0:
		return q, nil
	case 1:
	default:
		return q, errors.New("--free lleva a lo sumo un inicio o un rango: killport --free 3000 o killport --free 3000-3999")
	}
	arg := strings.TrimSpace(pos[0])
	req, err := ports.ParseRequest([]string{arg})
	if err != nil {
		return q, err
	}
	if req.Aimed() || len(req.Names) > 0 || strings.Contains(arg, ":") {
		return q, fmt.Errorf("%q: --free busca un puerto libre en TODAS las direcciones; pasale solo el número (killport --free 3000)", arg)
	}
	rs := req.Ports.Ranges()
	if len(rs) != 1 {
		return q, fmt.Errorf("%q: --free lleva un inicio o un rango, no una lista (killport --free 3000-3999)", arg)
	}
	q.Within = rs[0]
	q.Explicit = strings.ContainsAny(arg, "-–")
	if !q.Explicit {
		q.Within.Hi = 65535
	}
	return q, nil
}

// protosTexto nombra los protocolos de una búsqueda: "TCP", "TCP y UDP".
func protosTexto(ps []ports.Proto) string {
	var out []string
	for _, p := range ps {
		out = append(out, p.String())
	}
	return unirY(out)
}

// listaPuertosTexto escribe "3002", "3002 y 3003", "3002, 3003 y 3005".
func listaPuertosTexto(ps []uint16) string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = strconv.Itoa(int(p))
	}
	return unirY(out)
}

// textoSalteo cuenta por qué se salteó un tramo. quien nombra al dueño de un
// PID ("node.exe (PID 812 · shop-web)"); nil = solo el PID.
func textoSalteo(s ports.Skip, quien func(pid uint32) string) string {
	nombre := func(pid uint32) string {
		if quien != nil {
			if q := quien(pid); q != "" {
				return q
			}
		}
		return "el PID " + strconv.FormatUint(uint64(pid), 10)
	}
	switch s.Why {
	case ports.SkipTaken:
		x := s.Socket
		switch {
		case x.PID == 0:
			return "lo tiene el sistema (sin proceso dueño)"
		case x.Proto == ports.UDP || x.State == ports.Listen:
			return "lo escucha " + nombre(x.PID)
		}
		return "lo usa " + nombre(x.PID) + " en una conexión (" + x.State.Label() + ")"
	case ports.SkipWinNAT:
		return "lo reservó WinNAT para Hyper-V, WSL o Docker (bloque " + s.Excluded.Range.String() + ")"
	case ports.SkipAdministered:
		return "exclusión administrada " + s.Excluded.Range.String() + ": alguien lo apartó a mano; se puede usar, pero no lo ofrezco"
	case ports.SkipDynamic:
		return "rango dinámico " + s.Dynamic.Range().String() + ": Windows lo reparte a las conexiones salientes"
	case ports.SkipProbe:
		donde := s.Proto.String() + " " + familiaTexto(s.Family)
		switch s.Bind {
		case ports.BindInUse:
			return "alguien lo tiene atado sin escuchar (" + donde + ")"
		case ports.BindForbidden:
			return "Windows no deja atarlo (" + donde + ")"
		}
		return "no lo pude sondear (" + donde + ")"
	}
	return ""
}

func familiaTexto(f ports.Family) string {
	if f == ports.IPv6 {
		return "IPv6"
	}
	return "IPv4"
}
