package tui

import (
	"sync"
	"time"
	"unicode/utf8"
)

// 20 cuadros por segundo: la barra se ve fluida y la consola no se entera.
const frameInterval = 50 * time.Millisecond

// Live es una región al pie de la salida que se redibuja en el lugar (barras,
// spinners). Las líneas permanentes se imprimen ARRIBA de la región.
//
// Orden de candados, para que nunca haya deadlock: render() corre SIN tener el
// candado del terminal (puede tomar el del estado de la herramienta), y
// Println nunca llama a render (reusa el último cuadro). Así el que tiene el
// estado puede imprimir, y el que dibuja nunca espera al estado con el
// terminal tomado.
type Live struct {
	t      *Term
	render func(width int) []string
	last   []string
	drawn  int
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
	on     bool
}

// StartLive arranca el redibujo periódico. En una salida no interactiva no
// dibuja nada y Println escribe directo.
func (t *Term) StartLive(render func(width int) []string) *Live {
	l := &Live{t: t, render: render, stop: make(chan struct{}), done: make(chan struct{})}
	if !t.live {
		close(l.done)
		return l
	}
	l.on = true
	t.mu.Lock()
	t.setCursorHidden(true)
	t.mu.Unlock()
	go l.loop()
	return l
}

func (l *Live) loop() {
	defer close(l.done)
	tick := time.NewTicker(frameInterval)
	defer tick.Stop()
	l.Refresh()
	for {
		select {
		case <-l.stop:
			return
		case <-tick.C:
			l.Refresh()
		}
	}
}

// Refresh dibuja un cuadro ya mismo.
func (l *Live) Refresh() {
	if !l.on {
		return
	}
	lines := l.render(l.t.Width())
	l.t.mu.Lock()
	defer l.t.mu.Unlock()
	l.last = lines
	l.paint(lines, "")
}

// Println imprime líneas permanentes por encima de la región viva.
func (l *Live) Println(lines ...string) {
	var text string
	for _, s := range lines {
		text += s + "\n"
	}
	if !l.on {
		l.t.Print(text)
		return
	}
	l.t.mu.Lock()
	defer l.t.mu.Unlock()
	l.paint(l.last, text)
}

// Stop frena el redibujo. keep=false borra la región (lo habitual: después
// viene la tarjeta de resumen); keep=true deja el último cuadro en pantalla.
func (l *Live) Stop(keep bool) {
	l.once.Do(func() {
		if !l.on {
			return
		}
		close(l.stop)
		<-l.done
		lines := l.render(l.t.Width())
		l.t.mu.Lock()
		defer l.t.mu.Unlock()
		if keep {
			l.paint(lines, "")
			l.t.out.WriteString("\n")
		} else {
			l.paint(nil, "")
		}
		l.drawn = 0
		l.t.setCursorHidden(false)
	})
}

// paint borra la región, escribe el texto permanente y dibuja la región nueva,
// todo en UNA escritura (una sola llamada a la consola por cuadro, sin
// parpadeo). Requiere el candado del terminal.
func (l *Live) paint(region []string, above string) {
	width := l.t.Width()
	b := make([]byte, 0, 256+len(above)+len(region)*width*2)
	switch {
	case l.drawn == 1:
		b = append(b, "\r\x1b[J"...)
	case l.drawn > 1:
		b = append(b, "\r\x1b["...)
		b = append(b, itoa(l.drawn-1)...)
		b = append(b, "A\x1b[J"...)
	}
	b = append(b, above...)
	for i, line := range region {
		if i > 0 {
			b = append(b, '\n')
		}
		// width-1: una línea que llena la última columna deja el cursor en
		// "wrap pendiente" y la cuenta de líneas del próximo borrado se rompe.
		b = append(b, ClipANSI(line, width-1)...)
	}
	l.drawn = len(region)
	l.t.out.Write(b)
}

// ClipANSI recorta a max columnas visibles una línea que puede traer escapes:
// copia los escapes intactos, corta el texto y cierra con un reset si cortó.
func ClipANSI(s string, max int) string {
	if Width(s) <= max {
		return s
	}
	b := make([]byte, 0, len(s))
	w := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := skipEscape(s, i)
			b = append(b, s[i:j]...)
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := RuneWidth(r)
		if w+rw > max {
			break
		}
		b = append(b, s[i:i+size]...)
		w += rw
		i += size
	}
	return string(append(b, resetSeq...))
}

var spinnerFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner devuelve el cuadro que corresponde al instante now (80 ms por cuadro,
// independiente de la frecuencia de redibujo).
func Spinner(now time.Time) string {
	return spinnerFrames[(now.UnixMilli()/80)%int64(len(spinnerFrames))]
}
