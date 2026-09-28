package ports

import (
	"fmt"
	"strconv"
	"strings"
)

// Rango dinámico: de dónde saca Windows los puertos que reparte solo —el
// puerto local de cada conexión saliente, y los bloques de a 100 que WinNAT
// aparta para Hyper-V, WSL y Docker en cada arranque—. De fábrica es
// 49152–65535 (el rango que la IANA reserva para eso, RFC 6335), bien lejos de
// los puertos de desarrollo.
//
// El problema clásico es una máquina donde quedó en 1025–5000 (el valor de
// Windows XP, que algunos instaladores y "optimizadores" todavía restauran):
// ahí WinNAT reserva sus bloques ENTRE el 3000 y el 5000, distintos en cada
// arranque, y el servidor de desarrollo amanece con "acceso denegado" en un
// puerto que ayer andaba y que netstat muestra vacío.
//
// La fuente es
//
//	netsh interface ipv4|ipv6 show dynamicport tcp|udp
//
// que sale traducida. Igual que con los rangos excluidos, el parser no lee
// palabras: se ancla en la línea de guiones y toma, en orden, los dos enteros
// que siguen a un ":" (el inicio y la cantidad).

// Valores de fábrica del rango dinámico (Windows Vista en adelante).
const (
	DynamicDefaultStart = 49152
	DynamicDefaultCount = 16384
)

// DynamicRange es el rango dinámico de un protocolo y una familia.
type DynamicRange struct {
	Proto  Proto
	Family Family
	Start  uint16
	Count  int // cuántos puertos, contando Start
}

// Range es el rango como intervalo cerrado [Start, Start+Count-1].
func (d DynamicRange) Range() Range {
	return Range{d.Start, uint16(int(d.Start) + d.Count - 1)}
}

// Default dice si es el de fábrica.
func (d DynamicRange) Default() bool {
	return d.Start == DynamicDefaultStart && d.Count == DynamicDefaultCount
}

// Low dice si el rango invade los puertos registrados (arranca antes del
// 49152): ahí es donde pisa a los servidores de desarrollo.
func (d DynamicRange) Low() bool { return d.Start < DynamicDefaultStart }

// DynamicRanges son los cuatro (protocolo × familia).
type DynamicRanges []DynamicRange

// Find devuelve el rango de ese protocolo y familia.
func (x DynamicRanges) Find(proto Proto, fam Family) (DynamicRange, bool) {
	for _, d := range x {
		if d.Proto == proto && d.Family == fam {
			return d, true
		}
	}
	return DynamicRange{}, false
}

// ParseDynamic interpreta la salida de netsh para un protocolo y familia. Sin
// la línea de guiones, o sin exactamente dos valores después, devuelve error:
// inventar un rango sería peor que no saberlo.
func ParseDynamic(out string, proto Proto, fam Family) (DynamicRange, error) {
	d := DynamicRange{Proto: proto, Family: fam}
	var vals []int
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
		i := strings.LastIndexByte(line, ':')
		if i < 0 {
			continue // prosa traducida sin valor
		}
		v := strings.TrimSpace(line[i+1:])
		if !isDigits(v) {
			continue
		}
		num, err := strconv.ParseUint(v, 10, 32)
		if err != nil || num > 65535 {
			return d, fmt.Errorf("salida de netsh, línea %d (%q): %s está fuera de rango", n+1, strings.TrimSpace(line), v)
		}
		vals = append(vals, int(num))
	}
	switch {
	case !table:
		return d, fmt.Errorf("la salida de netsh no tiene el rango de puertos dinámicos")
	case len(vals) != 2:
		return d, fmt.Errorf("la salida de netsh trae %d valores y esperaba dos (inicio y cantidad)", len(vals))
	case vals[0] < 1 || vals[1] < 1:
		return d, fmt.Errorf("el rango dinámico %d + %d no es válido", vals[0], vals[1])
	case vals[0]+vals[1]-1 > 65535:
		return d, fmt.Errorf("el rango dinámico %d + %d se pasa del 65535", vals[0], vals[1])
	}
	d.Start, d.Count = uint16(vals[0]), vals[1]
	return d, nil
}
