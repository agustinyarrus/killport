// Package textdist mide cuán parecidas son dos palabras, para sugerir "¿quisiste
// decir…?" ante un flag o un archivo mal escrito.
package textdist

import "strings"

// Distance es la distancia de Damerau–Levenshtein restringida (optimal string
// alignment): inserciones, borrados, sustituciones y transposiciones de dos
// letras vecinas, que es el error de tipeo más común ("--froce").
// Programación dinámica con tres filas rodantes: O(n·m) tiempo, O(m) memoria.
func Distance(a, b string) int {
	ra, rb := []rune(strings.ToLower(a)), []rune(strings.ToLower(b))
	n, m := len(ra), len(rb)
	if n == 0 {
		return m
	}
	if m == 0 {
		return n
	}
	prev2 := make([]int, m+1) // fila i-2
	prev := make([]int, m+1)  // fila i-1
	cur := make([]int, m+1)   // fila i
	for j := 0; j <= m; j++ {
		prev[j] = j
	}
	for i := 1; i <= n; i++ {
		cur[0] = i
		for j := 1; j <= m; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			best := min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				best = min(best, prev2[j-2]+1)
			}
			cur[j] = best
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[m]
}

// Closest devuelve el candidato más parecido a word si está lo bastante cerca
// (a lo sumo un tercio de sus letras mal, mínimo una), o "" si ninguno lo está.
func Closest(word string, candidates []string) string {
	best, bestD := "", 1<<30
	limit := max(1, len([]rune(word))/3)
	for _, c := range candidates {
		if d := Distance(word, c); d < bestD {
			best, bestD = c, d
		}
	}
	if bestD <= limit {
		return best
	}
	return ""
}
