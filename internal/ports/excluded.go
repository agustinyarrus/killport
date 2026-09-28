package ports

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Rangos excluidos: puertos que Windows le niega a cualquier programa (bind
// falla con WSAEACCES, 10013) sin que haya un proceso dueño. Los reserva WinNAT
// para Hyper-V, WSL y Docker (dinámicos, cambian en cada arranque) o un
// administrador con "netsh int ipvX add excludedportrange" (persistentes,
// marcados con "*"). Es la causa típica de "el puerto está ocupado pero netstat
// no muestra a nadie".
//
// La fuente es la salida de
//
//	netsh interface ipv4|ipv6 show excludedportrange protocol=tcp|udp
//
// que viene TRADUCIDA al idioma de Windows, y en una codificación que depende
// de la versión (en esta máquina sale en UTF-8 aunque la consola esté en la
// página 850). El parser no lee ni una palabra: se ancla en la línea de guiones
// que subraya los títulos (igual en todos los idiomas) y de ahí en adelante
// toma las filas que son exactamente dos enteros y, opcionalmente, un "*".
// Todo lo demás (títulos, la nota al pie "* - Exclusiones de puertos
// administrados", líneas vacías) se ignora.

// ExcludedRange es una fila de la tabla de exclusiones.
type ExcludedRange struct {
	Proto  Proto
	Family Family
	Range
	// Administered: fila con "*", agregada a mano y persistente. Reiniciar
	// WinNAT no la libera; hay que borrarla con netsh.
	Administered bool
}

// Exclusions es la tabla completa (los cuatro protocolo×familia juntos).
type Exclusions []ExcludedRange

// ParseExcluded interpreta la salida de netsh para un protocolo y familia.
// Sin la línea de guiones devuelve error: eso no es una tabla (netsh falló,
// o la salida es otra cosa) y tomarla como "no hay rangos" sería mentir.
func ParseExcluded(out string, proto Proto, fam Family) (Exclusions, error) {
	var xs Exclusions
	table := false
	for n, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !table {
			table = isRule(fields)
			continue
		}
		if !isDigits(fields[0]) {
			continue // la nota al pie, u otra prosa traducida
		}
		row, err := parseExcludedRow(fields)
		if err != nil {
			return nil, fmt.Errorf("salida de netsh, línea %d (%q): %w", n+1, strings.TrimSpace(line), err)
		}
		row.Proto, row.Family = proto, fam
		xs = append(xs, row)
	}
	if !table {
		return nil, fmt.Errorf("la salida de netsh no tiene la tabla de rangos excluidos")
	}
	slices.SortFunc(xs, func(a, b ExcludedRange) int { return int(a.Lo) - int(b.Lo) })
	return xs, nil
}

// isRule reconoce la línea que subraya los títulos: solo guiones.
func isRule(fields []string) bool {
	for _, f := range fields {
		if strings.Trim(f, "-") != "" {
			return false
		}
	}
	return true
}

func parseExcludedRow(fields []string) (ExcludedRange, error) {
	var row ExcludedRange
	switch {
	case len(fields) == 2:
	case len(fields) == 3 && fields[2] == "*":
		row.Administered = true
	default:
		return row, fmt.Errorf("esperaba «inicio fin [*]»")
	}
	var ends [2]uint16
	for i := range ends {
		if !isDigits(fields[i]) {
			return row, fmt.Errorf("%q no es un puerto", fields[i])
		}
		v, err := strconv.ParseUint(fields[i], 10, 32)
		if err != nil || v > 65535 {
			return row, fmt.Errorf("%s está fuera de rango", fields[i])
		}
		ends[i] = uint16(v)
	}
	if ends[0] > ends[1] {
		return row, fmt.Errorf("el rango %d–%d está dado vuelta", ends[0], ends[1])
	}
	row.Range = Range{ends[0], ends[1]}
	return row, nil
}

// Find devuelve el primer rango (el de inicio más bajo) de ese protocolo y
// familia que contiene p. Barrido lineal a propósito: la tabla tiene decenas
// de filas y puede traer rangos solapados (WinNAT y un administrador pueden
// reservar lo mismo), con lo que una bisección tendría que mirar hacia atrás
// igual. O(k).
func (x Exclusions) Find(proto Proto, fam Family, p uint16) (ExcludedRange, bool) {
	for _, r := range x {
		if r.Proto == proto && r.Family == fam && r.Contains(p) {
			return r, true
		}
	}
	return ExcludedRange{}, false
}

// Set junta los puertos excluidos de un protocolo y familia en un conjunto.
func (x Exclusions) Set(proto Proto, fam Family) Set {
	var rs []Range
	for _, r := range x {
		if r.Proto == proto && r.Family == fam {
			rs = append(rs, r.Range)
		}
	}
	return NewSet(rs...)
}
