package killport

// exclusiones.go: DE QUIÉN es una exclusión de puertos que no agregó un
// administrador. La tabla de netsh no lo dice; lo dice la forma de la fila y
// lo que se ve de http.sys en el mismo momento (ports.ExcludedRange.Origin):
//
//   - un bloque de exactamente 100 puertos es de WinNAT (Hyper-V, WSL,
//     Docker), y es el ÚNICO que recibe las salidas de WinNAT: --reserve, que
//     lo detiene, y reiniciarlo;
//   - una fila de UN puerto TCP donde http.sys tiene algo —una URL registrada
//     en una cola, el kernel escuchándolo, una URL reservada— es el puerto que
//     http.sys aparta mientras lo atiende: el 2869 de SSDP y el 5357 de WSD
//     aparecen y desaparecen así. Se dice para qué es (la tabla de
//     sospechosos de Windows) y a nombre de quién;
//   - lo demás es la reserva que pidió un programa mientras corre.
//
// Esta parte es pura (sin build tag, se prueba en cualquier plataforma): la
// lectura de netsh, de los sockets y de los servicios está en
// exclusiones_windows.go.

import (
	"slices"
	"strconv"
	"strings"

	"github.com/agustinyarrus/killport/internal/ports"
)

// evidenciaHTTP es lo que se leyó de http.sys para decir si una exclusión es
// suya.
type evidenciaHTTP struct {
	colas    []ports.RequestQueue   // las colas a la vista (netsh http show servicestate)
	reservas []ports.URLReservation // las reservas de URL (netsh http show urlacl)
	kernel   ports.Set              // los puertos TCP que escucha el kernel (System, PID 4)
	// serviciosDe son los servicios que corren en un proceso (nil = no se sabe).
	serviciosDe func(pid uint32) []string
	// servicioDeSID es el nombre del servicio de un SID de servicio
	// (S-1-5-80-…); "" si no es uno o no se pudo resolver.
	servicioDeSID func(sid string) string
}

// candidataHTTP dice si una fila puede ser de http.sys: sin "*", TCP y de un
// solo puerto (http.sys aparta cada puerto por separado; dos vecinos son dos
// filas).
func candidataHTTP(x ports.ExcludedRange) bool {
	return !x.Administered && x.Proto == ports.TCP && x.Lo == x.Hi
}

// atribuirHTTP completa, en las filas que pueden ser de http.sys, lo que se
// sabe de él en ese puerto. Las demás quedan como estaban. No toca xs.
func atribuirHTTP(xs ports.Exclusions, ev evidenciaHTTP) ports.Exclusions {
	out := slices.Clone(xs)
	for i, x := range out {
		if candidataHTTP(x) {
			out[i].HTTP = ev.en(x.Lo)
		}
	}
	return out
}

// en arma la prueba de que http.sys atiende p, de lo más directo a lo menos:
// una cola con una URL ahí (la URL y el servicio que la atiende), el kernel
// escuchándolo, y la URL reservada ahí (para qué es y a nombre de quién, aunque
// la cola no esté a la vista: la de otro usuario solo la ve un administrador).
func (ev evidenciaHTTP) en(p uint16) ports.HTTPHold {
	var h ports.HTTPHold
	var servicios []string
	for _, q := range ev.colas {
		us := q.On(p)
		if len(us) == 0 {
			continue
		}
		h.Held = true
		if h.URL == "" {
			h.URL = us[0].String()
		}
		for _, qp := range q.Owners() {
			if ev.serviciosDe != nil {
				servicios = append(servicios, ev.serviciosDe(qp.PID)...)
			}
		}
	}
	h.Held = h.Held || ev.kernel.Contains(p)

	// La reserva dice para qué es y a nombre de quién, pero de SU URL: si ya
	// hay una registrada a la vista, lo que vale es lo de esa cola.
	registrada := h.URL != ""
	var cuentas []string
	for _, r := range ports.URLReservationsOn(ev.reservas, p) {
		h.Held = true
		if registrada {
			break
		}
		if h.URL == "" {
			h.URL = r.URL.String()
		}
		for _, sid := range r.SIDs {
			if strings.HasPrefix(sid, "S-1-5-80-") && ev.servicioDeSID != nil {
				if n := ev.servicioDeSID(sid); n != "" {
					servicios = append(servicios, n)
				}
				continue
			}
			if c := cuentaSDDL(sid); c != "" && !slices.Contains(cuentas, c) {
				cuentas = append(cuentas, c)
			}
		}
	}
	if !h.Held {
		return ports.HTTPHold{}
	}

	h.Service = elegirServicio(servicios, p)
	if h.Service == "" {
		h.Account = unirY(cuentas)
	}
	// Para qué es: por el servicio, si se sabe; si no, por el puerto, pero
	// solo si lo que hay ahí es la reserva de Windows. Un programa cualquiera
	// con su URL en el 2869 no es SSDP, y el kernel escuchando no dice nada
	// más que "http.sys".
	switch {
	case h.Service != "":
		if s, ok := usoDeWindows(h.Service, p); ok {
			h.What = s.que
		}
	case !registrada && h.URL != "":
		if s, ok := usoDeWindows("", p); ok {
			h.What = s.que
		}
	}
	return h
}

// elegirServicio se queda con UN servicio: el único que hay, o, si son
// varios (un svchost compartido), el que la tabla de Windows conoce para ese
// puerto. Si no se puede decir cuál, ninguno.
func elegirServicio(servicios []string, p uint16) string {
	slices.Sort(servicios)
	servicios = slices.Compact(servicios)
	switch len(servicios) {
	case 0:
		return ""
	case 1:
		return servicios[0]
	}
	var conocidos []string
	for _, s := range servicios {
		if _, ok := usoDeWindows(s, p); ok {
			conocidos = append(conocidos, s)
		}
	}
	if len(conocidos) == 1 {
		return conocidos[0]
	}
	return ""
}

// cuentaSDDL nombra las cuentas de todos los Windows que aparecen en las
// reservas de URL, por su alias de SDDL (que no se traduce). Una cuenta de
// usuario (S-1-5-21-…) no se nombra: no dice para qué es el puerto y es un
// dato de la persona.
func cuentaSDDL(sid string) string {
	switch sid {
	case "LS", "S-1-5-19":
		return "servicio local"
	case "NS", "S-1-5-20":
		return "servicio de red"
	case "SY", "S-1-5-18":
		return "SYSTEM"
	case "BA", "S-1-5-32-544":
		return "administradores"
	case "BU", "S-1-5-32-545":
		return "usuarios"
	case "AU", "S-1-5-11":
		return "usuarios autenticados"
	case "IU", "S-1-5-4":
		return "usuarios interactivos"
	case "WD", "S-1-1-0":
		return "todos"
	}
	return ""
}

// conOrigen devuelve los tramos salteados de --free con la fila de cada
// exclusión como la ve atribuida (xs): FindFree trabaja con la tabla cruda
// —para saber qué está tapado no hace falta saber de quién es—, y solo lo que
// se le muestra a una persona o a un script dice de quién.
func conOrigen(skips []ports.Skip, xs ports.Exclusions) []ports.Skip {
	out := slices.Clone(skips)
	for i, s := range out {
		if s.Why != ports.SkipReserved {
			continue
		}
		for _, x := range xs {
			if x.Proto == s.Excluded.Proto && x.Family == s.Excluded.Family && x.Range == s.Excluded.Range && !x.Administered {
				out[i].Excluded = x
				break
			}
		}
	}
	return out
}

// textoReserva dice, en una línea, de quién es una exclusión sin "*": lo que
// --free cuenta de un tramo salteado.
func textoReserva(x ports.ExcludedRange) string {
	switch x.Origin() {
	case ports.OriginWinNAT:
		return "lo reservó WinNAT para Hyper-V, WSL o Docker (bloque " + x.Range.String() + ")"
	case ports.OriginHTTP:
		que := "lo aparta http.sys mientras lo atiende"
		if x.HTTP.What != "" {
			que += ": " + x.HTTP.What
		} else if x.HTTP.URL != "" {
			que += " (" + x.HTTP.URL + ")"
		}
		return que
	}
	return "lo reservó un programa mientras corre, no WinNAT (exclusión " + x.Range.String() + ")"
}

// origenJSON es el nombre del origen para los scripts.
func origenJSON(x ports.ExcludedRange) string {
	switch x.Origin() {
	case ports.OriginAdministered:
		return "administrada"
	case ports.OriginWinNAT:
		return "winnat"
	case ports.OriginHTTP:
		return "httpsys"
	}
	return "programa"
}

// porQueNoReservo es el impedimento de --reserve para un puerto que tapa una
// exclusión que no es de WinNAT: detener WinNAT no la saca, y reservar un
// puerto que Windows ya aparta para otro no se puede.
func porQueNoReservo(r ports.Range, c combo, x ports.ExcludedRange) string {
	que := r.String() + " " + c.String() + " cae en la exclusión " + x.Range.String()
	switch x.Origin() {
	case ports.OriginHTTP:
		que += ", que Windows pone mientras http.sys atiende ese puerto"
		if x.HTTP.What != "" {
			que += " (" + x.HTTP.What + ")"
		}
		return que + ": no es de WinNAT y se va sola cuando http.sys lo suelta, así que no lo toco"
	}
	return que + ", que no es de WinNAT (sus bloques son de " + strconv.Itoa(ports.WinNATBlock) + " puertos) ni administrada: la tiene reservada un programa mientras corre, y detener WinNAT no la saca, así que no lo toco"
}
