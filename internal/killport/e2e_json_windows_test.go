//go:build windows

package killport

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// X-05 y X-06 de punta a punta: el JSON de `killport P` dice del objetivo lo
// mismo que el listado (línea de comandos y ruta de verdad, desde cuándo
// escucha), y el del listado no trae textos de pantalla.
func TestE2EJSONComandoDeVerdad(t *testing.T) {
	dir := binariosE2E(t)
	p, pid := escuchar(t, dir, t.TempDir(), "127.0.0.1", 0, "J")
	P := strconv.Itoa(p)

	out, code := killportE2E(t, dir, nil, P, "--json", "--tcp")
	var rep struct {
		Objetivos []struct {
			PID     int    `json:"pid"`
			Comando string `json:"comando"`
			Ruta    string `json:"ruta"`
			Desde   string `json:"desde"`
		} `json:"objetivos"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 || len(rep.Objetivos) != 1 {
		t.Fatalf("%s --json: %v (código %d)\n%s", P, err, code, out)
	}
	o := rep.Objetivos[0]
	desde, err := time.Parse(time.RFC3339Nano, o.Desde)
	if o.PID != pid || !strings.Contains(o.Comando, "-puerto 0 -etiqueta J") || !strings.EqualFold(filepath.Base(o.Ruta), "escuchador.exe") ||
		err != nil || time.Since(desde) > time.Minute {
		t.Fatalf("objetivo = %+v", o)
	}

	out, _ = killportE2E(t, dir, nil, "--json")
	var lst struct {
		Escuchando []map[string]any `json:"escuchando"`
	}
	if err := json.Unmarshal([]byte(out), &lst); err != nil {
		t.Fatal(err)
	}
	for _, f := range lst.Escuchando {
		if _, hay := f["direccion"]; hay {
			t.Fatalf("el listado trae 'direccion': %v", f)
		}
		if f["pid"].(float64) == float64(pid) && (f["comando"] != o.Comando || f["ruta"] != o.Ruta) {
			t.Fatalf("la fila del escuchador no dice lo mismo que el objetivo: %v", f)
		}
	}
}
