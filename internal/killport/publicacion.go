package killport

// publicacion.go: un puerto que publica Docker Desktop con WSL 2 son TRES
// sockets de DOS dueños: com.docker.backend en 0.0.0.0 y [::], y wslrelay en
// [::1] (visto en la máquina real). Lo de wslrelay en ese puerto es parte de
// la misma publicación. Acá, las reglas puras para reconocerlo.

import (
	"net/netip"
	"slices"
	"strings"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
)

// deLaPublicacion: el socket de wslrelay que Docker Desktop abre para una
// publicación es el de [::1] (el motor tiene 0.0.0.0 y [::]).
func deLaPublicacion(s ports.Socket) bool {
	return s.Local.Addr().WithZone("") == netip.IPv6Loopback()
}

// negativasVistas es lo que se MUESTRA de las negativas de un plan: la del
// [::1] de wslrelay que es parte de la publicación de un contenedor (queda
// negada cuando al motor no se le preguntó: --no-docker) va dentro de la del
// motor en el mismo puerto. Es una sola publicación: un bloque, un dueño para
// la nota de puertos compartidos, no dos negativas con el mismo consejo y una
// falsa alarma de "lo escuchan 2 procesos distintos". El plan no se toca: la
// verificación sigue necesitando a cada socket con su dueño. Puro; conserva el
// orden. O(negativas²), que son pocas.
func negativasVistas(refs []reaper.Refusal) []reaper.Refusal {
	motor := func(r reaper.Refusal, p uint16, proto ports.Proto) bool {
		if r.Relay != reaper.RelayContainer || !r.Ports.Contains(p) {
			return false
		}
		return slices.ContainsFunc(r.Sockets, func(s ports.Socket) bool { return s.Proto == proto && s.Port() == p })
	}
	var out []reaper.Refusal
	var sueltas []reaper.Refusal
	for _, r := range refs {
		if !deLaPublicacionEntera(r) {
			out = append(out, r)
			continue
		}
		sueltas = append(sueltas, r)
	}
	for _, w := range sueltas {
		s0 := w.Sockets[0]
		i := slices.IndexFunc(out, func(r reaper.Refusal) bool { return motor(r, s0.Port(), s0.Proto) })
		if i < 0 {
			out = append(out, w) // no hay negativa del motor al lado: queda como está
			continue
		}
		m := &out[i]
		m.Sockets = append(append([]ports.Socket(nil), m.Sockets...), w.Sockets...)
		if !strings.Contains(m.Name, w.Name) {
			m.Name = nombreVisible(m.Name, m.PID) + " y " + w.Name + " en [::1]"
		}
	}
	return out
}

// deLaPublicacionEntera: la negativa es de wslrelay y todos sus sockets son el
// [::1] con el que Docker Desktop publica un contenedor.
func deLaPublicacionEntera(r reaper.Refusal) bool {
	if r.Relay != reaper.RelayWSL || len(r.Sockets) == 0 || !strings.EqualFold(r.Name, "wslrelay.exe") {
		return false
	}
	for _, s := range r.Sockets {
		if !deLaPublicacion(s) {
			return false
		}
	}
	return true
}
