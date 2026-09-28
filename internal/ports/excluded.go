package ports

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Rangos excluidos: puertos que Windows le niega a cualquier programa (bind
// falla con WSAEACCES, 10013). Es la causa típica de "el puerto está ocupado
// pero netstat no muestra a nadie". La tabla no dice quién puso cada fila,
// solo si la agregó un administrador ("*", persistente); el resto se reconoce
// por su FORMA, y hay tres clases (ver Origin):
//
//   - los bloques de WinNAT, para Hyper-V, WSL y Docker: siempre de 100
//     puertos, sin alinear, elegidos de nuevo en cada arranque;
//   - el puerto que atiende http.sys: una fila de UN puerto, en TCP v4 y v6,
//     que aparece cuando http.sys empieza a atenderlo y se va cuando lo suelta
//     (el 2869 de SSDP y el 5357 de WSD, que van y vienen con esos servicios);
//   - la reserva que pidió un programa en ejecución (SIO_ACQUIRE_PORT_RESERVATION,
//     no pide administrador): del tamaño que pidió, hasta que la suelta o
//     termina. Windows no dice cuál.
//
// Para distinguir la segunda de la tercera hace falta mirar a http.sys; eso lo
// hace quien lee el sistema y lo deja en HTTP (ver HTTPHold).
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
	// HTTP: lo que se sabe de http.sys en el puerto de la fila, si es suya.
	// ParseExcluded lo deja vacío; lo completa quien leyó http.sys.
	HTTP HTTPHold
}

// WinNATBlock es el tamaño de los bloques que aparta WinNAT: 100 puertos
// seguidos, siempre, y en cualquier lugar (sin alinear: 55150, 55308 y 55408
// en una misma máquina). El tamaño es su firma.
const WinNATBlock = 100

// HTTPHold es la prueba de que una exclusión es de http.sys: http.sys aparta
// cada puerto TCP que atiende (una fila de un solo puerto, en IPv4 y en IPv6,
// aunque la URL esté atada a 127.0.0.1) mientras lo atiende. Todos los campos
// son texto, así que ExcludedRange se sigue pudiendo comparar con ==.
type HTTPHold struct {
	Held bool // hay pruebas: una URL en ese puerto, o el kernel escuchándolo
	// URL es una URL de http.sys en ese puerto: la registrada, si hay una a
	// la vista, o si no la reservada (netsh http show urlacl). "" si lo único
	// que se vio fue al kernel escuchando.
	URL     string
	Service string // el servicio que la atiende, o al que está reservada, si se supo
	Account string // la cuenta a la que está reservada, si no es un servicio ("servicio local")
	What    string // qué es, si es algo conocido de Windows ("SSDP (UPnP), …")
}

// ExclusionOrigin es de dónde sale una exclusión.
type ExclusionOrigin uint8

const (
	// OriginProgram: la pidió un programa en ejecución y Windows no dice
	// cuál. Es lo que queda cuando no es ninguna de las otras.
	OriginProgram      ExclusionOrigin = iota
	OriginAdministered                 // "*": la agregó un administrador (o killport --reserve)
	OriginWinNAT                       // un bloque de WinNAT: Hyper-V, WSL o Docker
	OriginHTTP                         // el puerto que atiende http.sys
)

// Origin dice de dónde sale la fila: administrada si tiene "*"; de WinNAT si
// es un bloque de exactamente WinNATBlock puertos; de http.sys si hay pruebas
// (HTTP.Held); y si no, de un programa.
func (x ExcludedRange) Origin() ExclusionOrigin {
	switch {
	case x.Administered:
		return OriginAdministered
	case x.Len() == WinNATBlock:
		return OriginWinNAT
	case x.HTTP.Held:
		return OriginHTTP
	}
	return OriginProgram
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
