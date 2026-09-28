package ports

import (
	"fmt"
	"slices"
	"strings"
)

// Reservas de URL de http.sys: qué URL tiene apartadas cada cuenta para poder
// registrarlas sin ser administrador. Windows trae las de sus servicios —el
// 2869 de SSDP a nombre de Servicio local, el 5357 de WSD, el 5985 de WinRM—
// y quedan aunque el servicio no corra. Junto con la exclusión de un solo
// puerto que http.sys pone mientras atiende, dicen para qué es ese puerto
// aunque la cola del servicio no esté a la vista.
//
// La fuente es
//
//	netsh http show urlacl
//
// que anda sin administrador y sale TRADUCIDA, también en los nombres de las
// cuentas ("NT AUTHORITY\SERVICIO LOCAL", "NT-AUTORITÄT\LOKALER DIENST"). El
// parser no lee ni una palabra; lee la FORMA:
//
//   - la línea de guiones que subraya el título: de ahí en adelante hay datos;
//   - una reserva empieza en la línea cuyo último campo es una URL
//     (http:// o https://), después de la etiqueta traducida;
//   - las cuentas salen del SDDL de la reserva, que no se traduce: la línea
//     cuyo último campo empieza con "D:" y trae entradas "(A;;GX;;;LS)". De
//     cada entrada que PERMITE (tipo "A") se toma la cuenta, en la forma del
//     SDDL: un alias de dos letras ("LS", "NS", "BU") o un SID ("S-1-5-80-…",
//     el SID de un servicio).
//
// Las líneas de "Usuario:", "Escuchar:" y "Delegar:" se ignoran: dicen lo mismo
// que el SDDL, pero en el idioma de Windows.

// URLReservation es una reserva de URL de http.sys.
type URLReservation struct {
	URL  QueueURL
	SIDs []string // las cuentas a las que está reservada, como las escribe el SDDL
}

// ParseURLReservations interpreta la salida de netsh http show urlacl. Sin
// reservas es una lista vacía, sin error. Sin la línea de guiones, error: eso
// no es la lista (netsh falló, o la salida es otra cosa).
func ParseURLReservations(out string) ([]URLReservation, error) {
	lines := strings.Split(out, "\n")
	start := -1
	for i, l := range lines {
		if f := strings.Fields(l); len(f) > 0 && isRule(f) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("la salida de netsh no tiene la lista de reservas de URL de http.sys")
	}
	var rs []URLReservation
	for _, l := range lines[start:] {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		last := f[len(f)-1]
		if u, ok := parseQueueURL(last); ok {
			rs = append(rs, URLReservation{URL: u})
			continue
		}
		if len(rs) > 0 && strings.HasPrefix(last, "D:") {
			r := &rs[len(rs)-1]
			for _, sid := range sddlAllowed(last) {
				if !slices.Contains(r.SIDs, sid) {
					r.SIDs = append(r.SIDs, sid)
				}
			}
		}
	}
	return rs, nil
}

// sddlAllowed saca las cuentas de las entradas que permiten de una DACL en
// SDDL: "D:(A;;GX;;;BU)(A;;GX;;;LS)" → BU, LS. Una entrada es
// (tipo;banderas;derechos;objeto;objeto heredado;cuenta); las que niegan ("D")
// y las mal formadas se saltean.
func sddlAllowed(dacl string) []string {
	var out []string
	rest := dacl
	for {
		open := strings.IndexByte(rest, '(')
		if open < 0 {
			return out
		}
		end := strings.IndexByte(rest[open:], ')')
		if end < 0 {
			return out
		}
		ace := strings.Split(rest[open+1:open+end], ";")
		rest = rest[open+end+1:]
		if len(ace) == 6 && ace[0] == "A" && ace[5] != "" {
			out = append(out, ace[5])
		}
	}
}

// URLReservationsOn devuelve las reservas en ese puerto.
func URLReservationsOn(rs []URLReservation, port uint16) []URLReservation {
	var out []URLReservation
	for _, r := range rs {
		if r.URL.Port == port {
			out = append(out, r)
		}
	}
	return out
}
