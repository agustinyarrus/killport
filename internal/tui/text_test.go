package tui

import (
	"reflect"
	"strings"
	"testing"
)

func TestWidth(t *testing.T) {
	casos := map[string]int{
		"":                            0,
		"hola":                        4,
		"ñandú":                       5, // Latin-1: una columna cada una
		"日本":                          4, // CJK: dos columnas cada una
		"e\u0301":                     1, // e + acento combinante
		"\x1b[38;2;1;2;3mhola\x1b[0m": 4, // los escapes ANSI no ocupan lugar
		"\x1b]9;4;1;50\x07texto":      5, // OSC terminado en BEL
		"a\tb":                        2, // el tab es control: no suma
	}
	for in, want := range casos {
		if got := Width(in); got != want {
			t.Errorf("Width(%q) = %d, esperaba %d", in, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("hola mundo", 6); got != "hola …" {
		t.Errorf("Truncate = %q", got)
	}
	if got := Truncate("corto", 10); got != "corto" {
		t.Errorf("Truncate no debería tocar lo que entra: %q", got)
	}
	if got := Truncate("x", 0); got != "" {
		t.Errorf("Truncate a 0 = %q", got)
	}
}

func TestTruncateMiddle(t *testing.T) {
	path := `C:\carpeta\muy\larga\de\verdad\foto.webp`
	got := TruncateMiddle(path, 16)
	if Width(got) > 16 {
		t.Errorf("TruncateMiddle se pasó: %q (%d columnas)", got, Width(got))
	}
	if !strings.Contains(got, "…") || !strings.HasPrefix(got, `C:\`) || !strings.HasSuffix(got, ".webp") {
		t.Errorf("TruncateMiddle tendría que conservar el principio y el final: %q", got)
	}
}

func TestPad(t *testing.T) {
	if got := PadRight("ñu", 4); got != "ñu  " {
		t.Errorf("PadRight = %q", got)
	}
	if got := PadLeft("7", 3); got != "  7" {
		t.Errorf("PadLeft = %q", got)
	}
	if got := PadRight("largo", 2); got != "largo" {
		t.Errorf("PadRight no debería cortar: %q", got)
	}
}

func TestWrap(t *testing.T) {
	got := Wrap("uno dos tres cuatro", 8)
	want := []string{"uno dos", "tres", "cuatro"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Wrap = %q, esperaba %q", got, want)
	}
	if got := Wrap("a\n\nb", 20); !reflect.DeepEqual(got, []string{"a", "", "b"}) {
		t.Errorf("Wrap con párrafos = %q", got)
	}
}

func TestClipANSI(t *testing.T) {
	in := "\x1b[31mhola mundo\x1b[0m"
	got := ClipANSI(in, 4)
	if Width(got) != 4 {
		t.Errorf("ClipANSI dejó %d columnas: %q", Width(got), got)
	}
	if !strings.HasPrefix(got, "\x1b[31m") || !strings.HasSuffix(got, resetSeq) {
		t.Errorf("ClipANSI tendría que conservar el color y cerrar con reset: %q", got)
	}
	if got := ClipANSI("corto", 10); got != "corto" {
		t.Errorf("ClipANSI no debería tocar lo que entra: %q", got)
	}
}
