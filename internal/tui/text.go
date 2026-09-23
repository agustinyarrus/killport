package tui

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// wideRanges: bloques que la consola dibuja en dos columnas (CJK, Hangul,
// emoji). zeroRanges: marcas combinantes y selectores que no ocupan columna.
// Tablas ordenadas para buscar por bisección: O(log k) por runa.
var wideRanges = [][2]rune{
	{0x1100, 0x115f}, {0x231a, 0x231b}, {0x2329, 0x232a}, {0x23e9, 0x23ec},
	{0x23f0, 0x23f0}, {0x23f3, 0x23f3}, {0x25fd, 0x25fe}, {0x2614, 0x2615},
	{0x2648, 0x2653}, {0x267f, 0x267f}, {0x2693, 0x2693}, {0x26a1, 0x26a1},
	{0x26aa, 0x26ab}, {0x26bd, 0x26be}, {0x26c4, 0x26c5}, {0x26ce, 0x26ce},
	{0x26d4, 0x26d4}, {0x26ea, 0x26ea}, {0x26f2, 0x26f3}, {0x26f5, 0x26f5},
	{0x26fa, 0x26fa}, {0x26fd, 0x26fd}, {0x2705, 0x2705}, {0x270a, 0x270b},
	{0x2728, 0x2728}, {0x274c, 0x274c}, {0x274e, 0x274e}, {0x2753, 0x2755},
	{0x2757, 0x2757}, {0x2795, 0x2797}, {0x27b0, 0x27b0}, {0x27bf, 0x27bf},
	{0x2b1b, 0x2b1c}, {0x2b50, 0x2b50}, {0x2b55, 0x2b55}, {0x2e80, 0x303e},
	{0x3041, 0x33ff}, {0x3400, 0x4dbf}, {0x4e00, 0x9fff}, {0xa000, 0xa4cf},
	{0xa960, 0xa97f}, {0xac00, 0xd7a3}, {0xf900, 0xfaff}, {0xfe10, 0xfe19},
	{0xfe30, 0xfe6f}, {0xff00, 0xff60}, {0xffe0, 0xffe6}, {0x16fe0, 0x16fe4},
	{0x17000, 0x18cff}, {0x1b000, 0x1b2ff}, {0x1f004, 0x1f004}, {0x1f0cf, 0x1f0cf},
	{0x1f18e, 0x1f18e}, {0x1f191, 0x1f19a}, {0x1f200, 0x1f251}, {0x1f300, 0x1f64f},
	{0x1f680, 0x1f6ff}, {0x1f7e0, 0x1f7eb}, {0x1f90c, 0x1f9ff}, {0x1fa70, 0x1faff},
	{0x20000, 0x2fffd}, {0x30000, 0x3fffd},
}

var zeroRanges = [][2]rune{
	{0x0300, 0x036f}, {0x0483, 0x0489}, {0x0591, 0x05bd}, {0x0610, 0x061a},
	{0x064b, 0x065f}, {0x0e31, 0x0e31}, {0x0e34, 0x0e3a}, {0x1ab0, 0x1aff},
	{0x1dc0, 0x1dff}, {0x200b, 0x200f}, {0x2028, 0x202e}, {0x2060, 0x2064},
	{0x20d0, 0x20ff}, {0xfe00, 0xfe0f}, {0xfe20, 0xfe2f}, {0xfeff, 0xfeff},
	{0xe0100, 0xe01ef},
}

func inRanges(r rune, table [][2]rune) bool {
	i := sort.Search(len(table), func(i int) bool { return table[i][1] >= r })
	return i < len(table) && table[i][0] <= r
}

// RuneWidth devuelve cuántas columnas ocupa r en la consola.
func RuneWidth(r rune) int {
	switch {
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		return 0
	case r < 0x300:
		return 1 // camino rápido: ASCII y Latin-1, el 99 % de lo que se imprime
	case inRanges(r, zeroRanges):
		return 0
	case inRanges(r, wideRanges):
		return 2
	}
	return 1
}

// skipEscape devuelve el índice del primer byte después de la secuencia de
// escape que empieza en s[i] (CSI "ESC[ … final" u OSC "ESC] … BEL|ST").
func skipEscape(s string, i int) int {
	if i+1 >= len(s) {
		return len(s)
	}
	switch s[i+1] {
	case '[':
		j := i + 2
		for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
			j++
		}
		return min(j+1, len(s))
	case ']':
		for j := i + 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
		}
		return len(s)
	}
	return i + 2
}

// Width mide el ancho visible de s en columnas, ignorando los escapes ANSI.
// O(n) sobre los bytes.
func Width(s string) int {
	w := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c == 0x1b {
			i = skipEscape(s, i)
			continue
		}
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != 0x7f {
				w++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w += RuneWidth(r)
		i += size
	}
	return w
}

const ellipsis = "…"

// Truncate corta s (texto plano) para que entre en max columnas, con "…" al
// final si hizo falta cortar.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if Width(s) <= max {
		return s
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := RuneWidth(r)
		if w+rw > max-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	b.WriteString(ellipsis)
	return b.String()
}

// TruncateMiddle recorta por el medio, que es lo que sirve para rutas: se ve
// la unidad y el nombre del archivo, se pierde la mitad aburrida.
func TruncateMiddle(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if Width(s) <= max {
		return s
	}
	if max <= 2 {
		return Truncate(s, max)
	}
	runes := []rune(s)
	keep := max - 1
	headCols := keep / 2
	tailCols := keep - headCols
	var head, tail []rune
	w := 0
	for _, r := range runes {
		rw := RuneWidth(r)
		if w+rw > headCols {
			break
		}
		head = append(head, r)
		w += rw
	}
	w = 0
	for i := len(runes) - 1; i >= 0; i-- {
		rw := RuneWidth(runes[i])
		if w+rw > tailCols {
			break
		}
		tail = append(tail, runes[i])
		w += rw
	}
	for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
		tail[i], tail[j] = tail[j], tail[i]
	}
	return string(head) + ellipsis + string(tail)
}

// PadRight completa con espacios hasta w columnas (acepta texto con escapes).
func PadRight(s string, w int) string {
	if gap := w - Width(s); gap > 0 {
		return s + strings.Repeat(" ", gap)
	}
	return s
}

// PadLeft alinea a la derecha dentro de w columnas.
func PadLeft(s string, w int) string {
	if gap := w - Width(s); gap > 0 {
		return strings.Repeat(" ", gap) + s
	}
	return s
}

// Wrap parte texto plano en líneas de a lo sumo width columnas, cortando en
// espacios (greedy: óptimo para un solo párrafo de consola, O(n)).
func Wrap(s string, width int) []string {
	if width < 8 {
		width = 8
	}
	var lines []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			lines = append(lines, "")
			continue
		}
		cur, curW := words[0], Width(words[0])
		for _, word := range words[1:] {
			ww := Width(word)
			if curW+1+ww > width {
				lines = append(lines, cur)
				cur, curW = word, ww
				continue
			}
			cur += " " + word
			curW += 1 + ww
		}
		lines = append(lines, cur)
	}
	return lines
}
