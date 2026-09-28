//go:build windows

package killport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

// Del listado interactivo se eligió la fila de UN proceso en un puerto que
// escuchan dos: se apunta a ese y el otro queda al margen. Sin selectores ni
// fila elegida, no hay predicado (el puerto entero).
func TestElegirFilaDePuertoCompartido(t *testing.T) {
	f := reaper.NewForest(conWindows(
		reaper.Proc{ID: reaper.ID{PID: 812, Created: t0.Add(-time.Hour)}, Name: "node.exe", Parent: 1, SessionID: 1},
		reaper.Proc{ID: reaper.ID{PID: 990, Created: t0.Add(-time.Hour)}, Name: "python.exe", Parent: 1, SessionID: 1},
	))
	socks := []ports.Socket{sock(ports.TCP, "::1", 5173, ports.Listen, 812, ""), sock(ports.TCP, "0.0.0.0", 5173, ports.Listen, 990, "")}
	if picked, _ := elegir(&sistema{}, opciones{}, f, socks, nil, cfgC2); picked != nil {
		t.Fatal("sin selectores ni fila elegida, nil")
	}
	py, _ := f.LivePID(990)
	picked, _ := elegir(&sistema{}, opciones{elegido: py.ID}, f, socks, nil, cfgC2)
	if picked == nil || picked(socks[0]) || !picked(socks[1]) {
		t.Fatal("la fila elegida es python y nadie más")
	}
	plan := reaper.Focus(reaper.PlanKill(ports.Of(5173), socks, f, nil, cfgC2), ports.Whole(ports.Of(5173)), picked)
	if len(plan.Targets) != 1 || plan.Targets[0].Root.PID != 990 || len(plan.Bystanders) != 1 || plan.Bystanders[0].PID != 812 {
		t.Fatalf("plan = %+v / al margen %+v", plan.Targets, plan.Bystanders)
	}
	// -n node: el predicado elige por nombre.
	picked, _ = elegir(&sistema{}, opciones{sel: seleccion{nombres: []string{"node"}}}, f, socks, nil, cfgC2)
	if !picked(socks[0]) || picked(socks[1]) {
		t.Fatal("-n node elige a node")
	}
}

// Los bloques del plan, sin sistema real: dónde escucha cada objetivo,
// su proyecto, si quedó suelto (y si no parece de desarrollo), lo que queda
// al margen, y la nota del puerto compartido con cómo apuntar a uno.
func TestBloquesDondeProyectoYCompartido(t *testing.T) {
	f := reaper.NewForest(conWindows(
		reaper.Proc{ID: reaper.ID{PID: 500, Created: t0.Add(-3 * time.Hour)}, Name: "pwsh.exe", SessionID: 1},
		reaper.Proc{ID: reaper.ID{PID: 812, Created: t0.Add(-2 * time.Hour)}, Name: "node.exe", Parent: 500, SessionID: 1, Path: `C:\Program Files\nodejs\node.exe`},
		reaper.Proc{ID: reaper.ID{PID: 990, Created: t0.Add(-time.Hour)}, Name: "Discord.exe", Parent: 77, SessionID: 1, Path: `C:\Users\u\AppData\Local\Discord\Discord.exe`},
	))
	socks := []ports.Socket{
		sock(ports.TCP, "::1", 5173, ports.Listen, 812, ""),
		sock(ports.TCP, "0.0.0.0", 5173, ports.Listen, 990, ""),
	}
	cfg := cfgC2
	q, _ := ports.ParseRequest([]string{"5173"})
	plan := reaper.Focus(reaper.PlanKill(q.Ports, socks, f, nil, cfg), q, nil)
	node, _ := f.LivePID(812)
	discord, _ := f.LivePID(990)
	det := detalle{
		proyectos: map[reaper.ID]proyecto{node.ID: {Raiz: `C:\dev\shop\web`, Nombre: "shop-web", Marcador: "package.json", Repo: `C:\dev\shop`, Rama: "fix-ui"}},
		huerfanos: map[reaper.ID]reaper.Orphan{discord.ID: {Proc: discord, Why: "su padre (PID 77) ya no existe"}},
	}
	term := tui.Open()
	term.DisableColor()
	texto := strings.Join(bloques(term, &sistema{}, f, plan, cfg, det), "\n")
	for _, want := range []string{
		"5173 TCP escuchando en [::1] · esta PC (v6)",
		"5173 TCP escuchando en 0.0.0.0 · toda la red",
		"proyecto: shop › web (package.json «shop-web», rama fix-ui) · C:\\dev\\shop\\web",
		"quedó suelto: su padre (PID 77) ya no existe (no parece de desarrollo)",
		"! 5173: lo escuchan 2 procesos distintos a la vez, y ninguno dio error",
		"localhost y [::1] → node.exe (PID 812) en [::1]",
		"127.0.0.1 y la red por IPv4 → Discord.exe (PID 990) en 0.0.0.0",
		"→ killport localhost:5173 o 127.0.0.1:5173 apuntan solo al que contesta esa dirección",
	} {
		if !strings.Contains(texto, want) {
			t.Errorf("falta %q en:\n%s", want, texto)
		}
	}
	// Un huérfano de desarrollo no lleva la salvedad.
	det.huerfanos[discord.ID] = reaper.Orphan{Proc: discord, Why: "x", Dev: true}
	if texto := strings.Join(bloques(term, &sistema{}, f, plan, cfg, det), "\n"); strings.Contains(texto, "no parece de desarrollo") {
		t.Errorf("uno de desarrollo no lleva la salvedad:\n%s", texto)
	}

	// Con dirección: localhost apunta a node, Discord queda al margen, y la
	// pista de cómo apuntar ya no hace falta.
	q, _ = ports.ParseRequest([]string{"localhost:5173"})
	plan = reaper.Focus(reaper.PlanKill(q.Ports, socks, f, nil, cfg), q, nil)
	texto = strings.Join(bloques(term, &sistema{}, f, plan, cfg, detalle{}), "\n")
	if !strings.Contains(texto, "Discord.exe (PID 990) queda al margen   escucha el 5173 en 0.0.0.0 · toda la red; no contesta localhost: no se toca") ||
		strings.Contains(texto, "apuntan solo") {
		t.Errorf("con dirección:\n%s", texto)
	}
	b, _ := json.Marshal(construirJSONReporte(reaper.Report{}, plan, true))
	var doc struct {
		Objetivos []struct {
			PID         uint32   `json:"pid"`
			Direcciones []string `json:"direcciones"`
			Alcance     string   `json:"alcance"`
		} `json:"objetivos"`
		AlMargen []struct {
			PID         uint32   `json:"pid"`
			Direcciones []string `json:"direcciones"`
		} `json:"al_margen"`
		Compartidos []struct {
			Puerto uint16 `json:"puerto"`
		} `json:"compartidos"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Objetivos) != 1 || doc.Objetivos[0].PID != 812 || strings.Join(doc.Objetivos[0].Direcciones, ",") != "::1" || doc.Objetivos[0].Alcance != "esta_pc" ||
		len(doc.AlMargen) != 1 || doc.AlMargen[0].PID != 990 || len(doc.Compartidos) != 1 || doc.Compartidos[0].Puerto != 5173 {
		t.Errorf("JSON = %s", b)
	}
}
