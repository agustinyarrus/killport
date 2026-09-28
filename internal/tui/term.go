package tui

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
)

const (
	hideCursorSeq = "\x1b[?25l"
	showCursorSeq = "\x1b[?25h"
	fallbackWidth = 100 // ancho asumido cuando la salida no es una consola
	maxWidth      = 180 // más ancho que esto, una línea deja de leerse de un vistazo
	Margin        = "  "
)

// Term es la salida de una herramienta. Sabe si puede pintar (color), si
// puede mover el cursor (vivo) y cuánto mide la ventana.
type Term struct {
	out      *os.File
	in       *os.File
	mu       sync.Mutex
	color    bool
	live     bool
	winTerm  bool
	stdinTTY bool
	restore  func()
	hidden   bool
	width    int // ancho fijado con SetWidth; 0 = el de la consola
}

// Open prepara stdout: activa el procesamiento VT en la consola de Windows y
// respeta NO_COLOR (https://no-color.org).
func Open() *Term {
	t := &Term{out: os.Stdout, in: os.Stdin}
	vt, restore, isConsole := setupConsole(os.Stdout)
	t.restore = restore
	t.live = isConsole && vt
	t.color = t.live && os.Getenv("NO_COLOR") == ""
	_, wt := os.LookupEnv("WT_SESSION")
	t.winTerm = t.live && wt
	t.stdinTTY = stdinIsConsole(os.Stdin)
	return t
}

// CanPrompt dice si se puede hacer una pregunta interactiva: la salida es una
// consola viva Y la entrada es un teclado (no un pipe ni un archivo). Sin
// esto, una herramienta no debe preguntar: tiene que exigir --yes.
func (t *Term) CanPrompt() bool { return t.live && t.stdinTTY }

// Prompt escribe la pregunta (sin salto) y lee una línea de la entrada. ok es
// false si no se pudo leer (EOF, entrada cerrada). No usar si !CanPrompt.
func (t *Term) Prompt(question string) (respuesta string, ok bool) {
	t.Print(Margin + question)
	sc := bufio.NewScanner(t.in)
	if !sc.Scan() {
		return "", false
	}
	return strings.TrimSpace(sc.Text()), true
}

// DisableColor apaga los colores (flag --no-color) sin perder el modo vivo.
func (t *Term) DisableColor() { t.color = false }

// Color dice si la salida lleva color.
func (t *Term) Color() bool { return t.color }

// Interactive dice si hay consola con cursor controlable (barras vivas).
func (t *Term) Interactive() bool { return t.live }

// Width devuelve las columnas útiles (acotadas a maxWidth, ancho pero legible).
// Sin consola vale COLUMNS, si está (también en Windows).
func (t *Term) Width() int {
	w := t.width
	if w <= 0 {
		w = consoleWidth(t.out)
	}
	if w <= 0 {
		w = columnsEnv()
	}
	if w <= 0 {
		w = fallbackWidth
	}
	return min(w, maxWidth)
}

// unbounded: la salida no es una consola y nadie fijó un ancho (ni SetWidth
// ni COLUMNS). Lo que no se lee en una ventana no tiene por qué recortarse.
func (t *Term) unbounded() bool {
	return t.out != nil && !t.live && t.width <= 0 && consoleWidth(t.out) <= 0 && columnsEnv() <= 0
}

// Bounded dice si las líneas tienen un ancho que respetar: una consola, o un
// ancho fijado (SetWidth, COLUMNS). A un pipe o a un archivo sin ancho, cada
// renglón va entero: no se recorta ni se parte.
func (t *Term) Bounded() bool { return t.live || t.width > 0 || !t.unbounded() }

// columnsEnv es el ancho que dice la variable COLUMNS (0 si no hay).
func columnsEnv() int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("COLUMNS")))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// SetWidth fija el ancho en vez de preguntárselo a la consola (0 vuelve a
// preguntar). Es para dibujar a un ancho conocido: las pruebas de cómo se lee
// una tabla en 100 columnas y en 180. Se llama antes de dibujar, no mientras.
func (t *Term) SetWidth(w int) { t.width = w }

// Height devuelve las filas visibles (0 si no se sabe).
func (t *Term) Height() int { return consoleHeight(t.out) }

// Close restaura cursor, progreso de la barra de tareas y modo de consola.
func (t *Term) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.hidden {
		t.out.WriteString(showCursorSeq)
		t.hidden = false
	}
	if t.winTerm {
		t.out.WriteString("\x1b]9;4;0;0\x07")
	}
	if t.restore != nil {
		t.restore()
		t.restore = nil
	}
}

// Write escribe crudo, bajo el candado (seguro entre goroutines).
func (t *Term) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.out.Write(p)
}

// Print escribe s tal cual.
func (t *Term) Print(s string) { t.Write([]byte(s)) }

// Line escribe una línea con el margen izquierdo.
func (t *Term) Line(s string) { t.Write([]byte(Margin + s + "\n")) }

// Lines escribe varias líneas (ya con margen) de una sola vez: una sola
// llamada a WriteConsoleW en vez de una por línea.
func (t *Term) Lines(lines []string) {
	if len(lines) == 0 {
		return
	}
	t.Write([]byte(strings.Join(lines, "\n") + "\n"))
}

// Blank escribe una línea vacía.
func (t *Term) Blank() { t.Write([]byte("\n")) }

// Paint pinta s con c si hay color; si no, lo devuelve intacto.
func (t *Term) Paint(c Color, s string) string {
	if !t.color || !c.set || s == "" {
		return s
	}
	b := make([]byte, 0, len(s)+24)
	b = appendFG(b, c)
	b = append(b, s...)
	return string(append(b, resetSeq...))
}

// PaintOn pinta s con frente fg sobre fondo bg.
func (t *Term) PaintOn(fg, bg Color, s string) string {
	if !t.color {
		return s
	}
	b := make([]byte, 0, len(s)+44)
	if bg.set {
		b = appendBG(b, bg)
	}
	if fg.set {
		b = appendFG(b, fg)
	}
	b = append(b, s...)
	return string(append(b, resetSeq...))
}

// Italic pinta en itálica (Cascadia la tiene cursiva real, no sintetizada).
func (t *Term) Italic(c Color, s string) string {
	if !t.color {
		return s
	}
	return "\x1b[3m" + t.Paint(c, s)
}

// TaskProgress publica el avance en la pestaña y en el ícono de la barra de
// tareas de Windows Terminal (OSC 9;4). state: 1 normal, 2 error, 3 indeterminado.
func (t *Term) TaskProgress(state int, frac float64) {
	if !t.winTerm {
		return
	}
	pct := int(frac*100 + 0.5)
	pct = max(0, min(100, pct))
	t.Write([]byte("\x1b]9;4;" + itoa(state) + ";" + itoa(pct) + "\x07"))
}

// TaskProgressClear quita el indicador de la barra de tareas.
func (t *Term) TaskProgressClear() {
	if t.winTerm {
		t.Write([]byte("\x1b]9;4;0;0\x07"))
	}
}

func (t *Term) setCursorHidden(hidden bool) {
	if !t.live || t.hidden == hidden {
		return
	}
	t.hidden = hidden
	if hidden {
		t.out.WriteString(hideCursorSeq)
	} else {
		t.out.WriteString(showCursorSeq)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
