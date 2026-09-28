//go:build windows

package killport

import (
	"context"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win/pipetest"
)

// X-22: con --wsl, si la única distro que corre es la de Docker Desktop, no
// hay ninguna distro del usuario: se dice (antes, silencio).
func TestNotasWSLSoloLaDelMotor(t *testing.T) {
	got := strings.Join(plano(notasWSL(tui.Open(), &miradaWSL{Distros: []string{"docker-desktop"}})), "\n")
	if !strings.Contains(got, "ninguna distro tuya") || !strings.Contains(got, "docker-desktop") {
		t.Fatalf("notas = %q", got)
	}
	if got := notasWSL(tui.Open(), &miradaWSL{Distros: []string{"Ubuntu", "docker-desktop"}}); len(got) != 0 {
		t.Fatalf("con Ubuntu corriendo no hay nada que avisar: %q", got)
	}
}

// X-23: la tabla de lo que escucha solo dentro de WSL no repite "dentro de
// WSL" en cada fila (sin columna ALCANCE); el PID de Windows es "—" y el de
// Linux va con el proceso.
func TestTablaSoloWSL(t *testing.T) {
	p := procesoLinux{Distro: "Ubuntu", PID: 323, Comm: "python3"}
	f := fila{Port: 8766, Proto: ports.TCP, Name: "python3", Titulo: tituloLinux(p), Linux: &p, SoloWSL: true, Direccion: "127.0.0.1", Comando: "python3 -m http.server"}
	f.Alcance = alcanceFila(nil, f)
	term := tui.Open()
	term.SetWidth(120)
	lineas := plano(tablaListado(term, nil, []fila{f}, false, true))
	if strings.Contains(lineas[0], "ALCANCE") || strings.Contains(lineas[1], "dentro de WSL") ||
		!strings.Contains(lineas[1], "python3 (323) en Ubuntu") || !strings.Contains(lineas[1], "—") {
		t.Fatalf("tabla solo-WSL:\n%s", strings.Join(lineas, "\n"))
	}
}

// Q-11 (WSL): el relay reenvía el 3000 a DOS procesos Linux (uno en Ubuntu,
// otro en Debian). `killport 3000` los tiene a los dos como objetivo; el
// listado también muestra los dos, no solo el primero.
func TestListadoWSLDosDuenos(t *testing.T) {
	debian := "@navaja 1\n@ss\ntcp   LISTEN 0 5 0.0.0.0:3000 0.0.0.0:* users:((\"python3\",pid=70,fd=3))\n@clock 1790400000 100\n@pid 70\n" +
		"S 70 (python3) S 1 70 70 0 -1 4194560 800 0 0 0 5 2 0 0 20 0 1 0 610000 30000000 4000 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 1 0 0 0 0 0 0 0 0 0 0 0 0 0\n" +
		"U 1000 agus\nC python3 -m http.server 3000\n@fin\n"
	f := &wslFalso{running: []string{"Ubuntu", "Debian"}, sondeo: map[string]string{"Ubuntu": string(grabado(t, "wsl_sondeo_ss.txt")), "Debian": debian}}
	forest := reaper.NewForest([]reaper.Proc{{ID: reaper.ID{PID: 9120, Created: t0}, Name: "wslrelay.exe", Path: `C:\Program Files\WSL\wslrelay.exe`}})
	socks := []ports.Socket{sock(ports.TCP, "127.0.0.1", 3000, ports.Listen, 9120, "wslrelay.exe")}
	sys := sistemaFalso(t, nuevoMotorFalso(t, pipetest.Options{}), f)
	filas, _, _ := anotarListado(context.Background(), sys, filasDeListado(socks, forest, nil, reaper.Config{SystemRoot: `C:\Windows`}), forest, opciones{wsl: true})
	var nombres []string
	for _, fl := range filas {
		if fl.Port == 3000 {
			nombres = append(nombres, nombreFila(fl))
		}
	}
	if strings.Join(nombres, " | ") != "python3 (70) en Debian | node (812) en Ubuntu" {
		t.Fatalf("filas del 3000 = %q", nombres)
	}
}
