package tui

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// Tablas sin un solo borde: títulos en tono tenue, tres espacios entre
// columnas, y el ancho que no alcanza se reparte con un criterio que se puede
// explicar en una línea:
//
//	las columnas Flex CEDEN, las más anchas primero ("llenado de agua"), hasta
//	su Min; si ni así entra, se van las OPCIONALES (Drop 1, 2, …) y las Flex
//	vuelven a crecer; si no queda ninguna para sacar, las Flex ceden de más.
//
// Así una tabla de ocho columnas se lee en una consola de 100 y aprovecha una
// de 180 sin que nadie decida anchos a mano. En una consola angosta, antes de
// sacar una columna o de apretar una Flex por debajo de su Min, el aire entre
// columnas baja a dos espacios (en 80 columnas, siete huecos de tres son 21).
//
// Sin consola (la salida va a un pipe o a un archivo) la última columna va
// entera: el que hace `killport | findstr llama` busca justo lo que el recorte
// se come. COLUMNS, si está, fija el ancho igual que en una consola.

// Align es la alineación de una columna de tabla.
type Align int

const (
	Left Align = iota
	Right
)

// Col describe una columna.
type Col struct {
	Title string
	Align Align
	// Flex: la columna cede ancho cuando la tabla no entra (se recorta al
	// final, o por el medio si Middle).
	Flex   bool
	Middle bool
	// Min (solo Flex): hasta dónde cede de buena gana. Por debajo, antes que
	// seguir apretándola se saca una columna opcional; solo si ya no queda
	// ninguna para sacar, cede más. 0 = 8 columnas.
	Min int
	// Drop: 0 = la columna va siempre. 1, 2, … = es OPCIONAL, y ese es el
	// orden en que se saca cuando la tabla no entra ni con las Flex en su Min
	// (primero la 1; a igual número, la de más a la derecha). Una columna que
	// se saca no deja hueco.
	Drop int
}

// Cell es una celda con su color (cero = texto normal).
type Cell struct {
	Text  string
	Color Color
	// Keep (en una columna Middle): cuántas columnas del principio respeta el
	// recorte por el medio. Sirve para una línea de comandos: el programa se
	// ve entero y se recorta lo que le sigue ("node …\app\server.js 3000", no
	// "no…js 3000").
	Keep int
	// Alt es otra forma, más corta, de decir lo mismo: si Text no entra en la
	// columna, se usa Alt (con su AltKeep) antes de recortar. Una línea de
	// comandos con rutas enteras tiene una forma con solo los nombres de
	// archivo: "node srv.js 3000" dice más que "node ~\dev…\x\m.txt".
	Alt     string
	AltKeep int
}

// C arma una celda.
func C(text string, c Color) Cell { return Cell{Text: text, Color: c} }

// tableGutter es el aire entre columnas.
const tableGutter = 3

// Table alinea las filas en el ancho de la consola. O(filas × columnas) más
// la disposición (ver layout).
func (t *Term) Table(cols []Col, rows [][]Cell) []string {
	width := t.Width() - len(Margin)*2
	// Sin consola: la tabla se dispone en el ancho más generoso (maxWidth), y
	// la ÚLTIMA columna va entera (no se recorta ni se rellena: es la que se
	// busca con findstr; estirar las demás hasta la celda más larga dejaría
	// renglones de cientos de columnas de espacios).
	entera := t.unbounded() && len(cols) > 0
	if entera {
		width = maxWidth - len(Margin)*2
	}
	natural := make([]int, len(cols))
	for i, c := range cols {
		natural[i] = Width(c.Title)
	}
	for _, row := range rows {
		for i := range cols {
			if i < len(row) {
				natural[i] = max(natural[i], Width(row[i].Text))
			}
		}
	}
	if entera {
		// Para disponer las demás, la última pide lo mínimo.
		k := len(cols) - 1
		natural[k] = min(natural[k], max(cols[k].Min, Width(cols[k].Title)))
	}
	gutter := tableGutter
	widths, shown, ok := layoutOK(cols, natural, width, gutter)
	if !ok || countShown(shown) < len(cols) {
		// Con dos espacios entre columnas, ¿entra mejor? Mejor es mostrar más
		// columnas, o las mismas sin apretar ninguna por debajo de su Min.
		w2, s2, ok2 := layoutOK(cols, natural, width, tableGutter-1)
		if n, n2 := countShown(shown), countShown(s2); n2 > n || (n2 == n && ok2 && !ok) {
			widths, shown, gutter = w2, s2, tableGutter-1
		}
	}
	last := -1
	for i := range cols {
		if shown[i] {
			last = i
		}
	}

	fit := func(cell Cell, i int) string {
		s, keep, w := cell.Text, cell.Keep, widths[i]
		if entera && i == len(cols)-1 {
			return s
		}
		if Width(s) > w && cell.Alt != "" {
			s, keep = cell.Alt, cell.AltKeep
		}
		switch {
		case Width(s) <= w:
			return s
		case cols[i].Middle && keep > 0:
			return TruncateMiddleKeep(s, keep, w)
		case cols[i].Middle:
			return TruncateMiddle(s, w)
		}
		return Truncate(s, w)
	}
	align := func(s string, i int) string {
		if cols[i].Align == Right {
			return PadLeft(s, widths[i])
		}
		if i == last {
			return s // la última columna no necesita relleno a la derecha
		}
		return PadRight(s, widths[i])
	}
	sep := strings.Repeat(" ", gutter)
	line := func(cells []Cell, paint func(Cell) Color) string {
		parts := make([]string, 0, len(cols))
		for i := range cols {
			if !shown[i] {
				continue
			}
			var cell Cell
			if i < len(cells) {
				cell = cells[i]
			}
			parts = append(parts, t.Paint(paint(cell), align(fit(cell, i), i)))
		}
		return Margin + strings.Join(parts, sep)
	}

	out := make([]string, 0, len(rows)+1)
	head := make([]Cell, len(cols))
	for i, c := range cols {
		head[i] = Cell{Text: c.Title}
	}
	out = append(out, line(head, func(Cell) Color { return Faint }))
	for _, row := range rows {
		out = append(out, line(row, func(c Cell) Color {
			if !c.Color.IsSet() {
				return Text
			}
			return c.Color
		}))
	}
	return out
}

// flexMin es el Min de una columna Flex que no dice el suyo.
const flexMin = 8

// hardMin es lo último que cede una columna Flex cuando ya no queda ninguna
// opcional para sacar (o su Min, si es menor): una letra, el "…" y algo más.
const hardMin = 3

// layout decide qué columnas se muestran y con qué ancho, para que la tabla
// entre en width columnas. natural[i] es lo que pide la columna i (su título
// o su celda más ancha). Pura.
//
// Invariantes (probadas en table_test):
//   - si todo entra con su ancho natural, no se toca nada;
//   - una columna que no es Flex nunca se angosta, y una Flex nunca pasa de
//     su ancho natural;
//   - las opcionales se sacan en orden de Drop, y solo mientras la tabla no
//     entra con las Flex en su Min;
//   - si la tabla entra (con lo que haya que sacar), el ancho total no pasa
//     de width y, si hubo que recortar, lo usa entero.
//
// O(k² · log W) con k columnas y W el ancho natural más grande: k vueltas
// como mucho (una por columna que se saca), cada una con una bisección.
func layout(cols []Col, natural []int, width, gutter int) (widths []int, shown []bool) {
	widths, shown, _ = layoutOK(cols, natural, width, gutter)
	return widths, shown
}

// layoutOK es layout y además dice si las Flex quedaron en su Min o más
// (ok=false: hubo que apretar alguna por debajo, sin opcionales para sacar).
func layoutOK(cols []Col, natural []int, width, gutter int) (widths []int, shown []bool, ok bool) {
	shown = make([]bool, len(cols))
	for i := range shown {
		shown[i] = true
	}
	for {
		if w, ok := fill(cols, natural, shown, width, gutter, true); ok {
			return w, shown, true
		}
		i := nextDrop(cols, shown)
		if i < 0 {
			w, _ := fill(cols, natural, shown, width, gutter, false)
			return w, shown, false
		}
		shown[i] = false
	}
}

func countShown(shown []bool) int {
	n := 0
	for _, s := range shown {
		if s {
			n++
		}
	}
	return n
}

// nextDrop elige la próxima opcional a sacar: la de Drop más chico entre las
// que se muestran, y a igual Drop la de más a la derecha. -1 si no queda.
func nextDrop(cols []Col, shown []bool) int {
	best := -1
	for i, c := range cols {
		if !shown[i] || c.Drop <= 0 {
			continue
		}
		if best < 0 || c.Drop <= cols[best].Drop {
			best = i
		}
	}
	return best
}

// fill reparte el ancho entre las columnas que se muestran. comfortable
// decide el piso de las Flex (su Min, o hardMin). ok=false si ni con las Flex
// en el piso entra (entonces widths son los pisos).
//
// El reparte es un llenado de agua: se busca el techo C más alto tal que, con
// cada Flex en min(natural, max(C, piso)), todo entre. Las más anchas son las
// primeras en tocar el techo, así que ceden primero; las angostas no se
// tocan hasta que el techo baja hasta ellas. Lo que sobra por redondeo (menos
// de una columna por cada Flex recortada) va a las recortadas más anchas.
func fill(cols []Col, natural []int, shown []bool, width, gutter int, comfortable bool) ([]int, bool) {
	widths := make([]int, len(cols))
	n, fixed := 0, 0
	var flex []int
	for i, c := range cols {
		if !shown[i] {
			continue
		}
		n++
		widths[i] = natural[i]
		if c.Flex {
			flex = append(flex, i)
		} else {
			fixed += natural[i]
		}
	}
	avail := width - fixed - gutter*max(n-1, 0)
	floor := func(i int) int {
		f := cols[i].Min
		if f <= 0 {
			f = flexMin
		}
		if !comfortable {
			f = min(f, hardMin)
		}
		return min(natural[i], f)
	}
	total := func(ceil int) int {
		s := 0
		for _, i := range flex {
			s += min(natural[i], max(ceil, floor(i)))
		}
		return s
	}
	top := 0
	for _, i := range flex {
		top = max(top, natural[i])
	}
	if total(top) <= avail {
		return widths, true // todo entra con su ancho natural
	}
	if total(0) > avail {
		for _, i := range flex {
			widths[i] = floor(i)
		}
		return widths, false
	}
	// El techo más alto que entra: total es no decreciente en el techo.
	ceil := sort.Search(top+1, func(c int) bool { return total(c) > avail }) - 1
	used := 0
	var cut []int // las que quedaron recortadas: reciben lo que sobra
	for _, i := range flex {
		widths[i] = min(natural[i], max(ceil, floor(i)))
		used += widths[i]
		if widths[i] < natural[i] {
			cut = append(cut, i)
		}
	}
	sort.SliceStable(cut, func(a, b int) bool { return natural[cut[a]] > natural[cut[b]] })
	for k := 0; used < avail && k < len(cut); k++ {
		widths[cut[k]]++
		used++
	}
	return widths, true
}

// TruncateMiddleKeep recorta s por el medio para que entre en max columnas,
// sin tocar sus primeras keep columnas: lo que se pierde sale de lo que les
// sigue. Si ni el principio respetado entra (con el "…" y algo del final), se
// corta al final, que es lo que más se parece a respetarlo.
func TruncateMiddleKeep(s string, keep, max int) string {
	if Width(s) <= max {
		return s
	}
	if keep <= 0 {
		return TruncateMiddle(s, max)
	}
	w, cut := 0, 0
	for cut < len(s) {
		r, size := utf8.DecodeRuneInString(s[cut:])
		if w+RuneWidth(r) > keep {
			break
		}
		w += RuneWidth(r)
		cut += size
	}
	head, rest := s[:cut], s[cut:]
	if Width(head)+2 > max || rest == "" {
		return Truncate(s, max)
	}
	return head + TruncateMiddle(rest, max-Width(head))
}
