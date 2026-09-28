package cli

import (
	"strings"

	"github.com/agustinyarrus/killport/internal/tui"
)

// Help arma la ayuda completa: cabecera, uso, flags por sección, ejemplos y
// notas, con el ancho de la ventana y el lenguaje visual del resto de la salida.
func (a *App) Help(t *tui.Term) []string {
	out := t.Header(a.Name, a.Tagline, a.Version)
	title := func(s string) { out = append(out, tui.Margin+t.Paint(tui.Subtle, s)) }
	width := t.Width() - len(tui.Margin)*2

	title("uso")
	for _, u := range a.Usage {
		out = append(out, tui.Margin+"  "+t.Paint(tui.Text, u))
	}

	all := append([]*section(nil), a.sections...)
	all = append(all, &section{title: "general", flags: []*flag{
		{long: "help", short: 'h', help: "muestra esta ayuda", isBool: true},
		{long: "version", short: 'V', help: "muestra la versión", isBool: true},
		{long: "no-color", help: "salida sin colores (también respeta NO_COLOR)", isBool: true},
	}})

	// Una sola columna de flags para toda la ayuda: todas las descripciones
	// arrancan a la misma altura, sección tras sección.
	flagCol := 0
	for _, s := range all {
		for _, f := range s.flags {
			flagCol = max(flagCol, tui.Width(flagLabel(f)))
		}
	}
	flagCol = min(flagCol, 30)
	helpW := max(24, width-2-flagCol-3)

	for _, s := range all {
		if len(s.flags) == 0 {
			continue
		}
		out = append(out, "")
		title(s.title)
		for _, f := range s.flags {
			label := flagLabel(f)
			desc := f.help
			if f.def != "" && !f.isBool {
				desc += "  (" + f.def + ")"
			}
			lines := tui.Wrap(desc, helpW)
			painted := paintLabel(t, f)
			if tui.Width(label) > flagCol {
				out = append(out, tui.Margin+"  "+painted)
				painted, label = "", ""
			}
			for i, l := range lines {
				lead := strings.Repeat(" ", flagCol)
				if i == 0 {
					lead = painted + strings.Repeat(" ", flagCol-tui.Width(label))
				}
				out = append(out, tui.Margin+"  "+lead+"   "+paintDefault(t, l, f.def))
			}
		}
	}

	if len(a.Examples) > 0 {
		out = append(out, "")
		title("ejemplos")
		cmdW := 0
		for _, e := range a.Examples {
			cmdW = max(cmdW, tui.Width(e.Cmd))
		}
		cmdW = min(cmdW, width/2)
		// La descripción se parte al ancho que queda, con las líneas de más
		// alineadas debajo de la primera: en una consola angosta, una
		// descripción larga no se sale del borde.
		descW := max(24, width-2-cmdW-3)
		for _, e := range a.Examples {
			cmd := e.Cmd
			if tui.Width(cmd) > cmdW {
				out = append(out, tui.Margin+"  "+t.Paint(tui.Sky, cmd))
				cmd = ""
			}
			for i, l := range tui.Wrap(e.Desc, descW) {
				lead := strings.Repeat(" ", cmdW)
				if i == 0 {
					lead = tui.PadRight(cmd, cmdW)
				}
				out = append(out, tui.Margin+"  "+t.Paint(tui.Sky, lead)+"   "+t.Paint(tui.Subtle, l))
			}
		}
	}

	if len(a.Notes) > 0 {
		out = append(out, "")
		title("notas")
		for _, n := range a.Notes {
			for i, l := range tui.Wrap(n, width-4) {
				lead := "· "
				if i > 0 {
					lead = "  "
				}
				out = append(out, tui.Margin+"  "+t.Paint(tui.Faint, lead)+t.Paint(tui.Subtle, l))
			}
		}
	}
	return append(out, "")
}

func flagLabel(f *flag) string {
	s := "    "
	if f.short != 0 {
		s = "-" + string(f.short) + ", "
	}
	s += "--" + f.long
	if !f.isBool && f.arg != "" {
		s += " " + f.arg
	}
	return s
}

func paintLabel(t *tui.Term, f *flag) string {
	s := "    "
	if f.short != 0 {
		s = t.Paint(tui.Teal, "-"+string(f.short)) + t.Paint(tui.Faint, ", ")
	}
	s += t.Paint(tui.Teal, "--"+f.long)
	if !f.isBool && f.arg != "" {
		s += " " + t.Paint(tui.Peach, f.arg)
	}
	return s
}

// paintDefault tiñe el "(valor por defecto)" del final en tono tenue.
func paintDefault(t *tui.Term, line, def string) string {
	suffix := "  (" + def + ")"
	if def != "" && strings.HasSuffix(line, suffix) {
		return t.Paint(tui.Text, strings.TrimSuffix(line, suffix)) + t.Paint(tui.Faint, suffix)
	}
	return t.Paint(tui.Text, line)
}
