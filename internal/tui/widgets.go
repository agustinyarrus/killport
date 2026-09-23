package tui

import (
	"strings"
)

// Todos los widgets devuelven texto (con el margen ya puesto) y no escriben
// nada: son funciones puras del ancho y de si hay color, fáciles de probar.

// Header es la cabecera de cada herramienta: nombre, qué hace y versión.
func (t *Term) Header(name, tagline, version string) []string {
	width := t.Width() - len(Margin)*2
	left := t.Paint(Lavender, name) + t.Paint(Faint, "  ·  ") + t.Paint(Subtle, tagline)
	right := t.Paint(Faint, "navaja "+version)
	gap := width - Width(left) - Width(right)
	if gap < 2 {
		return []string{"", Margin + left, ""}
	}
	return []string{"", Margin + left + strings.Repeat(" ", gap) + right, ""}
}

// Chip es un dato de contexto con su punto de color.
type Chip struct {
	Text string
	Dot  Color
}

// Chips arma la línea de contexto: "● 12 archivos   ● 8 hilos".
func (t *Term) Chips(chips ...Chip) string {
	parts := make([]string, 0, len(chips))
	for i, c := range chips {
		dot := c.Dot
		if !dot.IsSet() {
			dot = Accent(i)
		}
		parts = append(parts, t.Paint(dot, "●")+" "+t.Paint(Subtle, c.Text))
	}
	return Margin + strings.Join(parts, "   ")
}

// Rule es el único separador: una línea fina casi invisible.
func (t *Term) Rule() string {
	return Margin + t.Paint(Ghost, strings.Repeat("─", t.Width()-len(Margin)*2))
}

var eighths = [...]string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}

// Bar dibuja una barra de width columnas, llena en frac ∈ [0,1], con degradé
// teal→lavanda y resolución de 1/8 de columna (8·width pasos distintos).
func (t *Term) Bar(frac float64, width int) string {
	return t.BarColors(frac, width, Teal, Lavender)
}

// BarColors es Bar con los extremos del degradé elegidos.
func (t *Term) BarColors(frac float64, width int, from, to Color) string {
	if width <= 0 {
		return ""
	}
	frac = max(0, min(1, frac))
	if !t.color {
		full := int(frac*float64(width) + 0.5)
		return "[" + strings.Repeat("#", full) + strings.Repeat(".", width-full) + "]"
	}
	cells := frac * float64(width)
	full := int(cells)
	part := int((cells - float64(full)) * 8)
	span := float64(max(width-1, 1))
	b := make([]byte, 0, width*24)
	for i := 0; i < full; i++ {
		b = appendFG(b, from.Lerp(to, float64(i)/span))
		b = append(b, "█"...)
	}
	rest := width - full
	if rest > 0 && part > 0 {
		b = appendFG(b, from.Lerp(to, float64(full)/span))
		b = append(b, eighths[part]...)
		rest--
	}
	if rest > 0 {
		b = appendFG(b, Ghost)
		b = append(b, strings.Repeat("─", rest)...)
	}
	return string(append(b, resetSeq...))
}

// Kind es el tipo de una línea de estado.
type Kind int

const (
	OK Kind = iota
	Fail
	Skip
	Warn
	Info
	Step
)

var kindGlyph = map[Kind]struct {
	glyph string
	color Color
}{
	OK:   {"✓", Sage},
	Fail: {"✗", Rose},
	Skip: {"↷", Cream},
	Warn: {"!", Peach},
	Info: {"·", Subtle},
	Step: {"›", Lavender},
}

// Status arma "  ✓ principal   detalle", con el principal en color de texto y
// el detalle en tono tenue.
func (t *Term) Status(k Kind, main, detail string) string {
	g := kindGlyph[k]
	s := Margin + t.Paint(g.color, g.glyph) + " " + t.Paint(Text, main)
	if detail != "" {
		s += "   " + t.Paint(Subtle, detail)
	}
	return s
}

// Hint es una pista al pie, casi invisible hasta que se la busca.
func (t *Term) Hint(s string) string {
	return Margin + t.Paint(Faint, "› "+s)
}

// KV es un par etiqueta/valor para los bloques informativos.
type KV struct {
	Key, Value string
	Color      Color // color del valor; cero = texto normal
}

// KVBlock alinea las etiquetas en una columna y los valores en otra.
func (t *Term) KVBlock(items []KV) []string {
	keyW := 0
	for _, it := range items {
		keyW = max(keyW, Width(it.Key))
	}
	width := t.Width() - len(Margin)*2 - keyW - 3
	out := make([]string, 0, len(items))
	for _, it := range items {
		c := it.Color
		if !c.IsSet() {
			c = Text
		}
		out = append(out, Margin+t.Paint(Subtle, PadRight(it.Key, keyW))+"   "+t.Paint(c, ClipANSI(it.Value, width)))
	}
	return out
}

// Item es una celda de la tarjeta de resumen.
type Item struct {
	Label, Value string
	Dot          Color
}

// Card dibuja el resumen final: un bloque con el fondo apenas teñido, sin
// bordes (el color vive en el fondo y en el punto pastel de cada etiqueta),
// ancho completo y en tantas columnas como entren.
func (t *Term) Card(title string, items []Item) []string {
	width := t.Width() - len(Margin)*2
	if !t.color {
		return t.plainCard(title, items)
	}
	labelW, valueW := 0, 0
	for _, it := range items {
		labelW = max(labelW, Width(it.Label))
		valueW = max(valueW, Width(it.Value))
	}
	cellW := 2 + labelW + 3 + valueW // "● " + etiqueta + aire + valor
	const pad = 3                    // aire interno a cada lado
	cols := max(1, min(4, (width-2*pad+4)/(cellW+4)))
	cols = min(cols, max(1, len(items)))
	rows := (len(items) + cols - 1) / cols
	stride := (width - 2*pad) / cols

	line := func(content string) string {
		return Margin + t.PaintOn(Text, CardBG, PadRight(content, width))
	}
	out := []string{"", line("")}
	if title != "" {
		out = append(out, line(strings.Repeat(" ", pad)+t.onCard(Subtle, title)), line(""))
	}
	for r := 0; r < rows; r++ {
		var b strings.Builder
		b.WriteString(strings.Repeat(" ", pad))
		for c := 0; c < cols; c++ {
			i := c*rows + r // columna por columna: se lee de arriba hacia abajo
			if i >= len(items) {
				break
			}
			it := items[i]
			dot := it.Dot
			if !dot.IsSet() {
				dot = Accent(i)
			}
			cell := t.onCard(dot, "●") + t.onCard(Text, " ") +
				t.onCard(Subtle, PadRight(it.Label, labelW)) + t.onCard(Text, "   ") +
				t.onCard(Text, it.Value)
			if c < cols-1 {
				cell = t.onCard(Text, "") + PadRight(cell, stride)
			}
			b.WriteString(cell)
		}
		out = append(out, line(b.String()))
	}
	return append(out, line(""))
}

// onCard pinta sobre el fondo de la tarjeta sin cortar el tinte con un reset.
func (t *Term) onCard(fg Color, s string) string {
	if !t.color {
		return s
	}
	b := appendBG(nil, CardBG)
	b = appendFG(b, fg)
	return string(append(b, s...))
}

func (t *Term) plainCard(title string, items []Item) []string {
	out := []string{""}
	if title != "" {
		out = append(out, Margin+"── "+title+" ──")
	}
	labelW := 0
	for _, it := range items {
		labelW = max(labelW, Width(it.Label))
	}
	for _, it := range items {
		out = append(out, Margin+"  "+PadRight(it.Label, labelW)+"  "+it.Value)
	}
	return out
}

// Align es la alineación de una columna de tabla.
type Align int

const (
	Left Align = iota
	Right
)

// Col describe una columna: título, alineación y si es la que se achica cuando
// no entra (Flex; se corta por el medio si Middle).
type Col struct {
	Title  string
	Align  Align
	Flex   bool
	Middle bool
}

// Cell es una celda con su color (cero = texto normal).
type Cell struct {
	Text  string
	Color Color
}

// C arma una celda.
func C(text string, c Color) Cell { return Cell{Text: text, Color: c} }

// Table alinea filas sin un solo borde: títulos en tono tenue, tres espacios
// entre columnas, y la columna Flex cede ancho cuando la ventana no alcanza.
// O(filas × columnas).
func (t *Term) Table(cols []Col, rows [][]Cell) []string {
	const gutter = 3
	width := t.Width() - len(Margin)*2
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = Width(c.Title)
	}
	for _, row := range rows {
		for i := range cols {
			if i < len(row) {
				widths[i] = max(widths[i], Width(row[i].Text))
			}
		}
	}
	total := gutter * (len(cols) - 1)
	for _, w := range widths {
		total += w
	}
	if over := total - width; over > 0 {
		for i, c := range cols {
			if c.Flex {
				widths[i] = max(8, widths[i]-over)
				break
			}
		}
	}
	fit := func(s string, i int) string {
		if Width(s) <= widths[i] {
			return s
		}
		if cols[i].Middle {
			return TruncateMiddle(s, widths[i])
		}
		return Truncate(s, widths[i])
	}
	align := func(s string, i int) string {
		if cols[i].Align == Right {
			return PadLeft(s, widths[i])
		}
		if i == len(cols)-1 {
			return s // la última columna no necesita relleno a la derecha
		}
		return PadRight(s, widths[i])
	}
	sep := strings.Repeat(" ", gutter)
	out := make([]string, 0, len(rows)+1)
	head := make([]string, len(cols))
	for i, c := range cols {
		head[i] = t.Paint(Faint, align(fit(c.Title, i), i))
	}
	out = append(out, Margin+strings.Join(head, sep))
	for _, row := range rows {
		cells := make([]string, len(cols))
		for i := range cols {
			var cell Cell
			if i < len(row) {
				cell = row[i]
			}
			c := cell.Color
			if !c.IsSet() {
				c = Text
			}
			cells[i] = t.Paint(c, align(fit(cell.Text, i), i))
		}
		out = append(out, Margin+strings.Join(cells, sep))
	}
	return out
}
