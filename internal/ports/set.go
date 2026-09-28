package ports

import (
	"iter"
	"slices"
	"strconv"
	"strings"
)

// Range es un intervalo CERRADO de puertos [Lo, Hi]. Con Lo > Hi no describe
// nada: los constructores lo dan vuelta, así que un Range que sale de este
// paquete siempre cumple Lo <= Hi.
type Range struct{ Lo, Hi uint16 }

// R arma un rango aceptando los extremos en cualquier orden: R(5003, 5000) es
// [5000, 5003], igual que el parser acepta "5003-5000".
func R(a, b uint16) Range {
	if a > b {
		a, b = b, a
	}
	return Range{a, b}
}

// Len cuenta los puertos del rango (hasta 65.536, por eso int y no uint16).
func (r Range) Len() int { return int(r.Hi) - int(r.Lo) + 1 }

// Contains dice si p cae en el rango.
func (r Range) Contains(p uint16) bool { return r.Lo <= p && p <= r.Hi }

// String escribe "3000" o "5000–5003" (raya corta, la tipográfica de un rango).
func (r Range) String() string {
	if r.Lo == r.Hi {
		return strconv.Itoa(int(r.Lo))
	}
	return strconv.Itoa(int(r.Lo)) + "–" + strconv.Itoa(int(r.Hi))
}

// Set es un conjunto de puertos en FORMA CANÓNICA: intervalos cerrados
// ordenados, disjuntos y no adyacentes. Invariante, para todo i:
//
//	rs[i].Lo <= rs[i].Hi
//	rs[i].Hi + 1 < rs[i+1].Lo      (entre dos intervalos falta al menos un puerto)
//
// La forma canónica es única: dos Set con los mismos puertos tienen los mismos
// intervalos, así que Equal es comparar slices y String es determinista. Con
// k intervalos: Contains O(log k) por bisección, Union/Intersect/Minus O(k+m)
// por mezcla, y la memoria es O(k) aunque el conjunto tenga 65.536 puertos.
//
// El valor cero es el conjunto vacío. Un Set es inmutable: ningún método
// modifica al receptor, así que se puede copiar y compartir sin cuidado.
type Set struct{ rs []Range }

// NewSet normaliza rangos en cualquier orden, solapados, adyacentes o dados
// vuelta. O(k log k) por el ordenamiento; el barrido que fusiona es O(k).
func NewSet(rs ...Range) Set {
	if len(rs) == 0 {
		return Set{}
	}
	tmp := make([]Range, len(rs))
	for i, r := range rs {
		tmp[i] = R(r.Lo, r.Hi)
	}
	slices.SortFunc(tmp, func(a, b Range) int { return int(a.Lo) - int(b.Lo) })
	out := tmp[:1]
	for _, r := range tmp[1:] {
		last := &out[len(out)-1]
		// Aritmética en int: con uint16, Hi+1 desborda en 65535 y fusionaría
		// [65535,65535] con cualquier cosa que empiece en 0.
		if int(r.Lo) <= int(last.Hi)+1 {
			last.Hi = max(last.Hi, r.Hi)
			continue
		}
		out = append(out, r)
	}
	return Set{rs: slices.Clip(out)}
}

// Of arma el conjunto de unos puertos sueltos.
func Of(ps ...uint16) Set {
	rs := make([]Range, len(ps))
	for i, p := range ps {
		rs[i] = Range{p, p}
	}
	return NewSet(rs...)
}

// Contains dice si p está en el conjunto. Bisección sobre los extremos
// superiores: el primer intervalo con Hi >= p es el único que puede tenerlo.
// O(log k).
func (s Set) Contains(p uint16) bool {
	i, _ := slices.BinarySearchFunc(s.rs, p, func(r Range, p uint16) int { return int(r.Hi) - int(p) })
	return i < len(s.rs) && s.rs[i].Lo <= p
}

// Len cuenta los puertos. O(k).
func (s Set) Len() int {
	n := 0
	for _, r := range s.rs {
		n += r.Len()
	}
	return n
}

// IsEmpty dice si no hay ningún puerto.
func (s Set) IsEmpty() bool { return len(s.rs) == 0 }

// Ranges devuelve una copia de los intervalos canónicos.
func (s Set) Ranges() []Range { return slices.Clone(s.rs) }

// Min devuelve el puerto más chico (false si el conjunto está vacío).
func (s Set) Min() (uint16, bool) {
	if len(s.rs) == 0 {
		return 0, false
	}
	return s.rs[0].Lo, true
}

// All recorre los puertos en orden creciente.
func (s Set) All() iter.Seq[uint16] {
	return func(yield func(uint16) bool) {
		for _, r := range s.rs {
			for p := int(r.Lo); p <= int(r.Hi); p++ {
				if !yield(uint16(p)) {
					return
				}
			}
		}
	}
}

// Equal compara dos conjuntos. Por la forma canónica alcanza con comparar los
// intervalos uno a uno: O(k).
func (s Set) Equal(t Set) bool { return slices.Equal(s.rs, t.rs) }

// Union es s ∪ t. Mezcla de dos listas ordenadas: O(k+m).
func (s Set) Union(t Set) Set {
	if s.IsEmpty() {
		return t
	}
	if t.IsEmpty() {
		return s
	}
	out := make([]Range, 0, len(s.rs)+len(t.rs))
	i, j := 0, 0
	push := func(r Range) {
		if n := len(out); n > 0 && int(r.Lo) <= int(out[n-1].Hi)+1 {
			out[n-1].Hi = max(out[n-1].Hi, r.Hi)
			return
		}
		out = append(out, r)
	}
	for i < len(s.rs) || j < len(t.rs) {
		if j == len(t.rs) || (i < len(s.rs) && s.rs[i].Lo <= t.rs[j].Lo) {
			push(s.rs[i])
			i++
		} else {
			push(t.rs[j])
			j++
		}
	}
	return Set{rs: out}
}

// Intersect es s ∩ t. Dos punteros: en cada paso se descarta el intervalo que
// termina antes, porque ya no puede cortarse con nada más. O(k+m). Los cortes
// de dos conjuntos canónicos salen ordenados y separados: no hace falta fusionar.
func (s Set) Intersect(t Set) Set {
	var out []Range
	i, j := 0, 0
	for i < len(s.rs) && j < len(t.rs) {
		a, b := s.rs[i], t.rs[j]
		if lo, hi := max(a.Lo, b.Lo), min(a.Hi, b.Hi); lo <= hi {
			out = append(out, Range{lo, hi})
		}
		if a.Hi < b.Hi {
			i++
		} else {
			j++
		}
	}
	return Set{rs: out}
}

// Minus es s \ t: a cada intervalo de s se le recortan los de t que lo pisan.
// O(k+m): el puntero de t nunca retrocede.
func (s Set) Minus(t Set) Set {
	var out []Range
	j := 0
	for _, a := range s.rs {
		lo := int(a.Lo)
		for j < len(t.rs) && t.rs[j].Hi < a.Lo {
			j++
		}
		for k := j; k < len(t.rs) && int(t.rs[k].Lo) <= int(a.Hi); k++ {
			b := t.rs[k]
			if int(b.Lo) > lo {
				out = append(out, Range{uint16(lo), b.Lo - 1})
			}
			lo = int(b.Hi) + 1
		}
		if lo <= int(a.Hi) {
			out = append(out, Range{uint16(lo), a.Hi})
		}
	}
	return Set{rs: out}
}

// String escribe el conjunto en es-AR: "3000, 5000–5003". Sin separador de
// miles a propósito: un puerto es un nombre, no una cantidad (como un año). La
// salida vuelve a entrar por ParseSpec y da el mismo conjunto.
func (s Set) String() string {
	parts := make([]string, len(s.rs))
	for i, r := range s.rs {
		parts[i] = r.String()
	}
	return strings.Join(parts, ", ")
}
