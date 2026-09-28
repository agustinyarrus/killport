//go:build windows

package killport

// httpsys.go: mirar DETRÁS de System (PID 4). Cuando la tabla dice que el
// kernel escucha el 80, casi siempre es http.sys atendiendo en nombre de un
// programa que le registró una URL: IIS, Reporting Services, el Agente de Web
// Deploy, WinRM, cualquier HttpListener de .NET. Es la pregunta más repetida
// de Windows ("Port 80 is being used by SYSTEM (PID 4)") y las respuestas son
// una ronda de adivinanzas: detené este servicio, probá con aquel.
//
// Acá no se adivina: se le pregunta a http.sys qué colas de solicitudes tienen
// una URL en ese puerto y qué procesos las atienden (netsh http show
// servicestate, que anda sin administrador). El proceso que aparece entra al
// plan por la misma puerta que cualquier dueño —reaper.Unmask lo pasa por la
// clasificación de siempre—, así que si es un servicio de terceros se detiene
// el servicio, si es un svchost se niega, y si es tu terminal también.
//
// Dos cosas que killport NO hace nunca, aunque los foros las recomienden:
// detener el controlador HTTP (`net stop http`: baja la cola de impresión,
// WinRM y todo lo que dependa de él) y deshabilitarlo (`sc config http start=
// disabled`: deja a la máquina sin servidor HTTP del kernel hasta que alguien
// se acuerde).

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/win"
)

// controladorHTTP es el nombre del controlador de http.sys en el
// administrador de servicios.
const controladorHTTP = "HTTP"

// httpsys es el cliente de una corrida (memoiza lo que vio).
type httpsys struct {
	netsh        netshFn
	dependientes func(servicio string) []string // los servicios activos que dependen de uno
	elevado      func() bool

	once  sync.Once
	colas []ports.RequestQueue
	err   error
}

func nuevoHTTPSys(n netshFn) *httpsys {
	return &httpsys{netsh: n, dependientes: win.ActiveDependents, elevado: win.IsElevated}
}

// consultar le pide a http.sys sus colas, una sola vez por corrida.
func (h *httpsys) consultar(ctx context.Context) {
	h.once.Do(func() {
		out, err := h.netsh(ctx, "http", "show", "servicestate", "view=requestq", "verbose=no")
		if err != nil {
			h.err = err
			return
		}
		h.colas, h.err = ports.ParseRequestQueues(out)
	})
}

// enPuerto son las colas que tienen alguna URL en ese puerto.
func (h *httpsys) enPuerto(port uint16) []ports.RequestQueue {
	var out []ports.RequestQueue
	for _, q := range h.colas {
		if len(q.On(port)) > 0 {
			out = append(out, q)
		}
	}
	return out
}

// duenos son los procesos que MANDAN en una cola: el que la creó, si netsh lo
// informó, y si no los que la atienden. La diferencia importa en IIS: los
// w3wp.exe atienden, pero la cola y sus URL son del servicio WAS, que relanza
// a un w3wp apenas muere. Cerrar al que atiende no libera el puerto; el dueño
// es quien registró.
func duenos(q ports.RequestQueue) []ports.QueueProcess {
	if q.Controller.PID != 0 {
		return []ports.QueueProcess{q.Controller}
	}
	return q.Processes
}

// identidades resuelve los procesos de unas colas a identidades de la
// instantánea. Entre la foto y la respuesta de netsh un proceso pudo morir y
// su PID reciclarse: si netsh informó la imagen y no es la del proceso de la
// foto, no es el mismo y se lo descarta.
func identidades(colas []ports.RequestQueue, forest *reaper.Forest) []reaper.ID {
	var out []reaper.ID
	seen := map[reaper.ID]bool{}
	for _, q := range colas {
		for _, qp := range duenos(q) {
			p, ok := forest.LivePID(qp.PID)
			if !ok || seen[p.ID] {
				continue
			}
			if qp.Image != "" && p.Path != "" && !strings.EqualFold(qp.Image, p.Path) {
				continue
			}
			seen[p.ID] = true
			out = append(out, p.ID)
		}
	}
	return out
}

// desenmascarar contesta, por cada puerto que figura a nombre del kernel,
// quién registró una URL ahí (Found, con los procesos), que hay una URL pero
// nadie a la vista que la atienda, que http.sys no tiene nada en ese puerto
// (Empty), o que no se le pudo preguntar (Unavailable).
func (h *httpsys) desenmascarar(ctx context.Context, masked []reaper.MaskedPort, forest *reaper.Forest) []reaper.Unmasking {
	h.consultar(ctx)
	out := make([]reaper.Unmasking, 0, len(masked))
	for _, m := range masked {
		u := reaper.Unmasking{Via: m.Via, Port: m.Port, Proto: m.Proto}
		puerto := strconv.Itoa(int(m.Port))
		colas := h.enPuerto(m.Port)
		switch ids := identidades(colas, forest); {
		case h.err != nil:
			u.Verdict = reaper.UnmaskUnavailable
			u.Reason = "lo escucha el kernel (System, PID 4) y no pude preguntarle a http.sys de quién es: " + h.err.Error()
			u.Advice = h.consejo("`netsh http show servicestate view=requestq` muestra qué proceso registró la URL")
		case len(ids) > 0:
			u.Verdict, u.Procs = reaper.UnmaskFound, ids
		case len(colas) > 0:
			u.Verdict = reaper.UnmaskEmpty
			u.Reason = "http.sys tiene registrada " + listaURL(colas, m.Port) + ", pero no hay a la vista ningún proceso atendiéndola"
			u.Advice = "si es un sitio de IIS, su grupo de aplicaciones arranca con el primer pedido: se libera deteniendo el sitio (`net stop was /y` baja TODOS los sitios)"
		case h.elevado():
			u.Verdict = reaper.UnmaskEmpty
			u.Reason = "lo escucha el kernel (System, PID 4) y no es http.sys: no tiene ninguna URL registrada en el " + puerto
			u.Advice = "es de otro controlador del kernel: no hay un proceso que cerrar"
		default:
			u.Verdict = reaper.UnmaskEmpty
			u.Reason = "lo escucha el kernel (System, PID 4), y http.sys no tiene a la vista ninguna URL registrada en el " + puerto
			u.Advice = h.consejo("las URL que registraron otros usuarios (los grupos de aplicaciones de IIS) solo se ven como administrador")
		}
		out = append(out, u)
	}
	return out
}

// consejo suma, a lo que haya que decir, los sospechosos: los servicios
// activos que dependen de http.sys. Nunca aconseja detener el controlador.
func (h *httpsys) consejo(base string) string {
	deps := h.dependientes(controladorHTTP)
	if len(deps) == 0 {
		return base
	}
	const tope = 6
	lista := strings.Join(deps[:min(len(deps), tope)], ", ")
	if len(deps) > tope {
		lista += " y " + strconv.Itoa(len(deps)-tope) + " más"
	}
	return base + "; los servicios activos que usan http.sys son " + lista
}

// listaURL nombra las URL de esas colas en ese puerto: "http://+:80/a/ (cola
// DefaultAppPool)". Hasta tres; de ahí en más, cuántas quedan.
func listaURL(colas []ports.RequestQueue, port uint16) string {
	var partes []string
	total := 0
	for _, q := range colas {
		for _, u := range q.On(port) {
			total++
			if len(partes) < 3 {
				partes = append(partes, u.String())
			}
		}
	}
	s := strings.Join(partes, ", ")
	if total > len(partes) {
		s += " y " + strconv.Itoa(total-len(partes)) + " más"
	}
	return s
}

// urlsDe son las URL que ese proceso tiene registradas en esos puertos, en el
// orden de netsh: lo que el bloque del objetivo muestra y el JSON lista.
func (h *httpsys) urlsDe(pid uint32, puertos ports.Set) []string {
	if h == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, q := range h.colas {
		suya := false
		for _, p := range duenos(q) {
			suya = suya || p.PID == pid
		}
		if !suya {
			continue
		}
		for _, u := range q.URLs {
			if s := u.String(); puertos.Contains(u.Port) && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}
