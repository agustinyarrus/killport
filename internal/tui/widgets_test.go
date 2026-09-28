package tui

import (
	"os"
	"strings"
	"testing"
)

// Un error que no entra en la ventana se parte en palabras, alineado después
// de la marca: antes iba en un renglón y la consola lo cortaba donde caía
// (`killport 3000 --wsl`, a 100 columnas, dejaba "ree" arriba y "nvío lo tiene"
// solo abajo).
func TestMarkedSeParteEnPalabras(t *testing.T) {
	term := &Term{}
	term.SetWidth(100)
	msg := "--wsl es del listado (killport --wsl): con puertos, killport ya mira dentro de WSL cuando un reenvío lo tiene"
	l := term.Marked("✗ ", Rose, msg)
	if len(l) != 2 || !strings.HasPrefix(l[0], Margin+"✗ --wsl") || !strings.HasPrefix(l[1], Margin+"  ") {
		t.Fatalf("renglones = %q", l)
	}
	var palabras []string
	for _, x := range l {
		if w := Width(x); w > 100-len(Margin) {
			t.Errorf("renglón de %d columnas: %q", w, x)
		}
		palabras = append(palabras, strings.Fields(strings.TrimPrefix(strings.TrimSpace(x), "✗ "))...)
	}
	if strings.Join(palabras, " ") != msg {
		t.Errorf("el mensaje cambió al partirse: %q", palabras)
	}
	// Uno corto queda como siempre: margen, marca y texto.
	if l := term.Marked("✗ ", Rose, "nada que liberar"); len(l) != 1 || l[0] != Margin+"✗ nada que liberar" {
		t.Errorf("corto: %q", l)
	}
	// A un archivo (o a un pipe), sin ancho, va entero en un renglón: el que lo
	// lee con findstr busca en una línea.
	f, err := os.CreateTemp(t.TempDir(), "salida-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	t.Setenv("COLUMNS", "")
	if l := (&Term{out: f}).Marked("✗ ", Rose, msg); len(l) != 1 || l[0] != Margin+"✗ "+msg {
		t.Errorf("sin ancho: %q", l)
	}
}
