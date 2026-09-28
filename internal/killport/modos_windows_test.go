//go:build windows

package killport

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// Q-05, como propiedad: en cada modo, todo flag que no hace nada ahí es un
// error de línea de comandos (código 2), no algo que se ignora en silencio.
// Antes `killport 3000 --reserve --list --timeout 5s`, `killport 3000 --list
// --yes` y `killport 3000 --system --list` pasaban callados.
func TestModosCadaFlagFueraDeSuModoEsError(t *testing.T) {
	flags := map[string]func(*opciones){
		"--list":       func(o *opciones) { o.list = true },
		"--yes":        func(o *opciones) { o.yes = true },
		"--force":      func(o *opciones) { o.force = true },
		"--tree":       func(o *opciones) { o.tree = true },
		"--manual":     func(o *opciones) { o.manual = true },
		"--timeout":    func(o *opciones) { o.timeout, o.timeoutDado = time.Second, true },
		"--system":     func(o *opciones) { o.system = true },
		"--wsl":        func(o *opciones) { o.wsl = true },
		"--no-docker":  func(o *opciones) { o.noDocker = true },
		"--all-states": func(o *opciones) { o.allStates = true },
		"--orphans":    func(o *opciones) { o.sel.huerfanos = true },
		"--name":       func(o *opciones) { o.sel.nombres = []string{"node"} },
		"--count":      func(o *opciones) { o.count, o.countDado = 3, true },
		"--any":        func(o *opciones) { o.any = true },
		"--stable":     func(o *opciones) { o.stable, o.stableDado = time.Second, true },
		"--new":        func(o *opciones) { o.nuevos = true },
	}
	modos := []struct {
		nombre     string
		pos        []string
		base       opciones
		permitidos []string
	}{
		{"listado", nil, opciones{}, []string{"--list", "--system", "--wsl", "--no-docker", "--all-states", "--orphans", "--name"}},
		{"cerrar", []string{"3000"}, opciones{}, []string{"--list", "--yes", "--force", "--tree", "--manual", "--timeout", "--no-docker", "--all-states", "--orphans", "--name"}},
		{"--free", []string{"3000"}, opciones{free: true}, []string{"--count"}},
		{"--wait", []string{"3000"}, opciones{wait: 5 * time.Second, waitDado: true}, []string{"--any", "--stable", "--new"}},
		{"--reserve", []string{"3000"}, opciones{reserve: true}, []string{"--list", "--yes"}},
	}
	for _, m := range modos {
		if err := validarModos(m.pos, m.base); err != nil {
			t.Fatalf("%s solo: %v", m.nombre, err)
		}
		for f, pon := range flags {
			o := m.base
			pon(&o)
			err := validarModos(m.pos, o)
			switch vale := slices.Contains(m.permitidos, f); {
			case vale && err != nil:
				t.Errorf("%s %s: %v", m.nombre, f, err)
			case !vale && err == nil:
				t.Errorf("%s %s: se aceptaba y no hace nada", m.nombre, f)
			case !vale && !strings.Contains(err.Error(), f) && !strings.Contains(err.Error(), "--system") && f != "--orphans" && f != "--name":
				t.Errorf("%s %s: el error no nombra el flag: %v", m.nombre, f, err)
			}
		}
	}
	// Las combinaciones dentro de cerrar: --list solo mira.
	for _, f := range []string{"--yes", "--force", "--timeout"} {
		o := opciones{list: true}
		flags[f](&o)
		if err := validarModos([]string{"3000"}, o); err == nil || !strings.Contains(err.Error(), "--list solo mira") {
			t.Errorf("--list %s: %v", f, err)
		}
	}
	// --system vale con un selector (elige también en otras sesiones).
	if err := validarModos(nil, opciones{system: true, sel: seleccion{nombres: []string{"node"}}}); err != nil {
		t.Errorf("-n node --system: %v", err)
	}
}
