package killport

// envolver.go: los renglones que no entran en la consola se parten con
// criterio, en vez de dejar que la terminal los corte donde caiga ("pide a" /
// "dministrador", "startport=5" / "5200"). Se corta en espacios; un comando
// entre `backticks` no se parte nunca (se copia entero); una palabra más larga
// que el renglón —una ruta— se corta en un separador de carpeta. Las líneas
// que siguen van con sangría colgante, debajo del texto y no del rótulo.
//
// Solo en una consola: a un pipe o a un archivo va cada renglón entero (el que
// lo lee con findstr busca en una línea).

import (
	"strings"

	"github.com/agustinyarrus/killport/internal/tui"
)

// envolver parte s en renglones de a lo sumo ancho columnas. Puro. O(n).
func envolver(s string, ancho int) []string {
	if ancho < 12 {
		ancho = 12
	}
	if tui.Width(s) <= ancho {
		return []string{s}
	}
	var out []string
	cur := ""
	for _, w := range palabras(s) {
		for tui.Width(w) > ancho && !strings.HasPrefix(w, "`") {
			// Más larga que el renglón entero: cortar en un separador (o, si no
			// hay, donde toque), y lo que sobra sigue en el que viene. Un
			// comando no: va entero en su renglón, aunque se pase (la terminal
			// lo parte a la vista, pero se copia entero).
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			cabe, resto := cortarPalabra(w, ancho)
			out = append(out, cabe)
			w = resto
		}
		switch {
		case cur == "":
			cur = w
		case tui.Width(cur)+1+tui.Width(w) <= ancho:
			cur += " " + w
		default:
			out = append(out, cur)
			cur = w
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// palabras parte en espacios, pero un tramo entre `backticks` es UNA palabra
// (un comando se copia entero, no se parte al medio).
func palabras(s string) []string {
	var out []string
	var b strings.Builder
	dentro := false
	for _, r := range s {
		switch {
		case r == '`':
			dentro = !dentro
			b.WriteRune(r)
		case r == ' ' && !dentro:
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// cortarPalabra corta una palabra más larga que ancho: en el último separador
// de carpeta que entre (se queda con él), o donde entre.
func cortarPalabra(w string, ancho int) (cabe, resto string) {
	sep, lleno, n := 0, len(w), 0
	for i, r := range w {
		if n+tui.RuneWidth(r) > ancho {
			lleno = i
			break
		}
		n += tui.RuneWidth(r)
		if r == '\\' || r == '/' {
			sep = i + 1 // el separador queda arriba
		}
	}
	if sep > 0 && lleno < len(w) {
		return w[:sep], w[sep:]
	}
	return w[:lleno], w[lleno:]
}

// renglones son las líneas de un renglón de bloque: prefijo (margen y
// sangría) + el texto pintado de c, partido si no entra (en consola), con las
// que siguen colgadas debajo del texto después del rótulo ("proyecto: ") si lo
// tiene.
func renglones(t *tui.Term, prefijo string, c tui.Color, s string) []string {
	ancho := t.Width() - tui.Width(prefijo)
	if !t.Bounded() || tui.Width(s) <= ancho {
		return []string{prefijo + t.Paint(c, s)}
	}
	cuelga := 2
	if i := strings.Index(s, ": "); i > 0 && i < 14 && !strings.ContainsAny(s[:i], " `") {
		cuelga = tui.Width(s[:i+2])
	}
	partes := envolver(s, ancho)
	if len(partes) > 1 {
		// Las que siguen tienen menos lugar (la sangría colgante): se rearman.
		resto := envolver(strings.Join(partes[1:], " "), ancho-cuelga)
		partes = append(partes[:1], resto...)
	}
	out := make([]string, len(partes))
	for i, p := range partes {
		pre := prefijo
		if i > 0 {
			pre += strings.Repeat(" ", cuelga)
		}
		out[i] = pre + t.Paint(c, p)
	}
	return out
}

// pista es t.Hint partida si no entra (con la sangría debajo del texto).
func pista(t *tui.Term, s string) []string {
	return renglones(t, tui.Margin+tui.Margin, tui.Faint, "› "+s)
}

// estado es t.Status que, si no entra en la consola, deja el principal en su
// renglón y el detalle debajo, partido con criterio.
func estado(t *tui.Term, k tui.Kind, principal, detalle string) []string {
	return estadoCon(t, "", k, principal, detalle)
}

// estadoCon es estado con una sangría de más delante de cada renglón (las
// notas que van debajo de una tabla o de un bloque).
func estadoCon(t *tui.Term, sangria string, k tui.Kind, principal, detalle string) []string {
	una := sangria + t.Status(k, principal, detalle)
	if !t.Bounded() || detalle == "" || tui.Width(una) <= t.Width() {
		return []string{una}
	}
	out := []string{sangria + t.Status(k, principal, "")}
	for _, l := range envolver(detalle, t.Width()-tui.Width(sangria+tui.Margin)-4) {
		out = append(out, sangria+tui.Margin+"    "+t.Paint(tui.Subtle, l))
	}
	return out
}
