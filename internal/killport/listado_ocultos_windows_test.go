//go:build windows

package killport

// Qué oculta el listado general y qué no: lo de un usuario detrás de http.sys que corre en un
// host de scripts de Windows (S-02, caso C) y las filas de System o del IP
// Helper cuando no se pudo mirar detrás (S-05).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
)

// S-02 (C): un HttpListener de Windows PowerShell 5.1 (su imagen está en
// System32) registró http://localhost:47953/kp-vs-s/: la fila de System se
// desenmascara y NO se oculta por ser "de Windows": es un script del usuario.
func TestListadoHTTPDePowerShellNoSeOculta(t *testing.T) {
	ps := reaper.Proc{ID: reaper.ID{PID: 5120, Created: t0}, Name: "powershell.exe", Parent: 3000, SessionID: 1,
		Path: raizWindows + `\System32\WindowsPowerShell\v1.0\powershell.exe`}
	forest := reaper.NewForest(conWindows(
		reaper.Proc{ID: reaper.ID{PID: 3000, Created: t0.Add(-60e9)}, Name: "explorer.exe", Path: raizWindows + `\explorer.exe`, SessionID: 1}, ps))
	h := &httpsys{netsh: func(context.Context, ...string) (string, error) { return "", nil }, dependientes: func(string) []string { return nil },
		elevado: func() bool { return false }}
	h.consultar(context.Background())
	h.colas, h.err = []ports.RequestQueue{{Processes: []ports.QueueProcess{{PID: 5120}},
		URLs: []ports.QueueURL{{Raw: "HTTP://LOCALHOST:47953/KP-VS-S/", Host: "localhost", Port: 47953, Path: "/kp-vs-s/"}}}}, nil
	socks := []ports.Socket{sock(ports.TCP, "::", 47953, ports.Listen, 4, "System")}
	filas := anotarWindows(context.Background(), &sistema{http: h}, filasDeListado(socks, forest, nil, cfgC2), forest, nil, cfgC2)
	muestra, ocultas := visibles(filas, mostrar{})
	if len(muestra) != 1 || ocultas.filas != 0 || nombreFila(muestra[0]) != "http.sys · powershell.exe" {
		t.Fatalf("visibles = %+v (ocultas %d)", muestra, ocultas.filas)
	}
}

// S-05: si netsh o el registro fallan, la fila de System o del IP Helper no
// vuelve a ser "de Windows" en silencio: se muestra con "no pude mirar", y el
// JSON lo dice.
func TestListadoSinPoderMirarDetras(t *testing.T) {
	h, _ := httpFalso(t, "", errors.New("netsh terminó con código 1"))
	forestHTTP := reaper.NewForest(conWindows())
	filas := anotarWindows(context.Background(), &sistema{http: h},
		filasDeListado([]ports.Socket{sock(ports.TCP, "::", 47181, ports.Listen, 4, "System")}, forestHTTP, nil, cfgC2), forestHTTP, nil, cfgC2)
	muestra, ocultas := visibles(filas, mostrar{})
	if len(muestra) != 1 || ocultas.filas != 0 || muestra[0].Comando != "http.sys · no pude mirar quién está detrás" || muestra[0].Relay != reaper.RelayHTTP {
		t.Fatalf("http.sys sin netsh: visibles = %+v (ocultas %d)", muestra, ocultas.filas)
	}
	b, _ := json.Marshal(construirJSONListado(muestra, ocultas))
	if !strings.Contains(string(b), `"no_pude_mirar":"no pude preguntarle a http.sys quién registró la URL: netsh terminó con código 1"`) {
		t.Fatalf("JSON:\n%s", b)
	}

	forest, svc, _ := bosqueProxy()
	reg := registroDePrueba()
	reg.err = errors.New("Acceso denegado.")
	filas = anotarWindows(context.Background(), &sistema{proxy: reg.cliente(false)},
		filasDeListado([]ports.Socket{sock(ports.TCP, "0.0.0.0", 3000, ports.Listen, 2200, "iphlpsvc")}, forest, svc, cfgC2), forest, svc, cfgC2)
	muestra, ocultas = visibles(filas, mostrar{})
	if len(muestra) != 1 || ocultas.filas != 0 || !strings.Contains(muestra[0].SinMirar, "Acceso denegado") || muestra[0].Relay != reaper.RelayPortProxy {
		t.Fatalf("portproxy sin registro: visibles = %+v (ocultas %d)", muestra, ocultas.filas)
	}
}
