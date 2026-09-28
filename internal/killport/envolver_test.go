package killport

import (
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/tui"
)

// X-02: se parte en espacios, un comando entre backticks queda entero, y una
// ruta más larga que el renglón se corta en un separador de carpeta.
func TestEnvolver(t *testing.T) {
	got := envolver("lo reserva para vos: WinNAT no lo vuelve a tomar (permanente; pide administrador)", 40)
	for _, l := range got {
		if tui.Width(l) > 40 {
			t.Fatalf("renglón de %d: %q", tui.Width(l), l)
		}
	}
	if strings.Join(got, " ") != "lo reserva para vos: WinNAT no lo vuelve a tomar (permanente; pide administrador)" {
		t.Fatalf("se perdió texto: %q", got)
	}
	for _, l := range got {
		if strings.HasSuffix(l, "pide a") || strings.HasPrefix(l, "dministrador") {
			t.Fatalf("partió una palabra: %q", got)
		}
	}
	got = envolver("detenelo con `netsh interface ipv4 add excludedportrange protocol=tcp startport=55200 numberofports=1` como administrador", 50)
	if !slicesContieneEntero(got, "`netsh interface ipv4 add excludedportrange protocol=tcp startport=55200 numberofports=1`") {
		t.Fatalf("el comando se partió: %q", got)
	}
	got = envolver(`proyecto: web · C:\Users\mauro\dev\_pruebas\killport\revision\x\proy\web\muy\adentro`, 30)
	for _, l := range got[1:] {
		if !strings.HasSuffix(got[0], " ·") && !strings.HasSuffix(l, `\`) && l != got[len(got)-1] {
			t.Fatalf("la ruta se cortó fuera de un separador: %q", got)
		}
	}
}

func slicesContieneEntero(ls []string, s string) bool {
	for _, l := range ls {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// renglones y estado: a 80 columnas nada se pasa del ancho, la sangría cuelga
// debajo del texto, y sin ancho (un pipe) va todo en una línea.
func TestRenglonesYEstado(t *testing.T) {
	term := tui.Open()
	term.SetWidth(80)
	largo := `proyecto: web (package.json «kp-ux-web», rama rama-ux) · C:\Users\mauro\dev\_pruebas\killport\revision\x\proy\web`
	ls := renglones(term, tui.Margin+"    ", tui.Faint, largo)
	if len(ls) < 2 {
		t.Fatalf("a 80 no entra en una: %q", ls)
	}
	for i, l := range ls {
		if tui.Width(l) > 80 {
			t.Fatalf("renglón %d de %d: %q", i, tui.Width(l), l)
		}
		if i > 0 && !strings.HasPrefix(l, tui.Margin+"    "+strings.Repeat(" ", len("proyecto: "))) {
			t.Fatalf("la sangría no cuelga debajo del texto: %q", l)
		}
	}
	det := "lo relanza node.exe (PID 35944): cerrá ese supervisor (por ej. nodemon/pm2) y después el puerto queda libre"
	es := estado(term, tui.Warn, "puerto 47912 lo retomó otro proceso", det)
	if len(es) < 2 || strings.Contains(strings.Join(es, "\n"), "(por\n") {
		t.Fatalf("estado = %q", es)
	}
	for _, l := range es {
		if tui.Width(l) > 80 {
			t.Fatalf("estado se pasa: %q", l)
		}
	}
	sin := tui.Open() // en las pruebas, la salida no es una consola
	if ls := renglones(sin, "    ", tui.Faint, largo); len(ls) != 1 {
		t.Fatalf("sin ancho, una sola línea: %q", ls)
	}
}
