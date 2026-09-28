package reaper

// Una sola forma de nombrar a un proceso en un texto. Antes había siete
// versiones, y las que armaban "nombre (PID n)" a mano decían "PID 5 (PID 5)"
// de un proceso cuyo nombre no se supo.

import (
	"strings"
	"testing"
)

func TestNameWithPID(t *testing.T) {
	for _, c := range []struct {
		name string
		pid  uint32
		or   string
		with string
	}{
		{"node.exe", 812, "node.exe", "node.exe (PID 812)"},
		{"", 812, "PID 812", "PID 812"},
	} {
		if got := NameOrPID(c.name, c.pid); got != c.or {
			t.Errorf("NameOrPID(%q, %d) = %q", c.name, c.pid, got)
		}
		if got := NameWithPID(c.name, c.pid); got != c.with {
			t.Errorf("NameWithPID(%q, %d) = %q", c.name, c.pid, got)
		}
	}
}

func TestSinPIDRepetido(t *testing.T) {
	sinNombre := Proc{ID: ID{PID: 5}}
	// El dueño de un puerto que solo se miró (no fue víctima).
	if got, _ := busyWhy(nil, sinNombre); got != "lo tiene PID 5" {
		t.Errorf("busyWhy: %q", got)
	}
	// El padre que lanzó a un impostor, sin nombre conocido.
	f := NewForest([]Proc{sinNombre, {ID: ID{PID: 9}, Parent: 5, Name: "svchost.exe"}})
	hijo, _ := f.Lookup(ID{PID: 9})
	if got := parentText(f, hijo); got != "PID 5" {
		t.Errorf("parentText: %q", got)
	}
	// El que sostiene un puerto después de un relanzamiento.
	r := relaunchResult(3000, relaunch{sup: sinNombre, child: Proc{ID: ID{PID: 7}}})
	for _, s := range []string{r.Reason, r.Advice} {
		if strings.Contains(s, "PID 5 (PID 5)") || strings.Contains(s, "PID 7 (PID 7)") || strings.Contains(s, "PID 5, PID 5") {
			t.Errorf("relanzado sin nombres: %q", s)
		}
	}
}
