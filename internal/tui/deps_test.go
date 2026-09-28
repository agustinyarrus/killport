package tui

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Q-03: tui (y con él img, pdf-merge, vidsquash) y clip2qr no cargan con lo
// de killport: las tablas de red (net/netip), el ejecutor acotado (os/exec),
// el cliente de named pipes. Antes tui importaba internal/win entero y cada
// una de esas herramientas pesaba ~105 KB de más.
func TestSinLoDeKillport(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: sin correr go list")
	}
	out, err := exec.Command("go", "list", "-deps", "github.com/agustinyarrus/killport/internal/tui", "github.com/agustinyarrus/killport/cmd/clip2qr").Output()
	if err != nil {
		t.Skipf("go list no anduvo: %v", err)
	}
	deps := strings.Fields(string(out))
	for _, no := range []string{"net/netip", "os/exec", "github.com/agustinyarrus/killport/internal/win", "github.com/agustinyarrus/killport/internal/win/pipeio"} {
		if slices.Contains(deps, no) {
			t.Errorf("tui o clip2qr dependen de %s", no)
		}
	}
}
