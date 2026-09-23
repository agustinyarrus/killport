package textdist

import "testing"

func TestDistance(t *testing.T) {
	casos := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"kitten", "sitting", 3}, // el ejemplo clásico
		{"force", "froce", 1},    // transposición de vecinas: UN error, no dos
		{"ABC", "abc", 0},        // no distingue mayúsculas
		{"ñandú", "nandu", 2},    // runas, no bytes
		{"--list", "--lst", 1},
	}
	for _, c := range casos {
		if got := Distance(c.a, c.b); got != c.want {
			t.Errorf("Distance(%q, %q) = %d, esperaba %d", c.a, c.b, got, c.want)
		}
		if got := Distance(c.b, c.a); got != c.want {
			t.Errorf("Distance no es simétrica para (%q, %q): %d", c.b, c.a, got)
		}
	}
}

func TestClosest(t *testing.T) {
	flags := []string{"--force", "--help", "--recursive", "--out"}
	if got := Closest("--froce", flags); got != "--force" {
		t.Errorf("Closest(--froce) = %q, esperaba --force", got)
	}
	if got := Closest("--xyzzy", flags); got != "" {
		t.Errorf("Closest(--xyzzy) = %q, esperaba nada (demasiado lejos)", got)
	}
	if got := Closest("x", nil); got != "" {
		t.Errorf("Closest sin candidatos = %q, esperaba nada", got)
	}
}
