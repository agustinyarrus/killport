//go:build windows

package killport

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
)

// X-30: una clave del JSON dice lo mismo en todos los modos. "libres" es un
// número (el reporte de cerrar) y la lista de --free se llama "encontrados";
// "desde" es siempre una hora, y un rango de puertos va en "inicio"/"fin".
func TestJSONClavesConUnSoloSentido(t *testing.T) {
	q := ports.FreeQuery{Within: ports.Range{Lo: 8123, Hi: 65535}, Protos: []ports.Proto{ports.TCP}, Count: 1}
	r := ports.FreeResult{Found: []uint16{8124}, Skips: []ports.Skip{{Range: ports.Range{Lo: 8123, Hi: 8123}, Why: ports.SkipWinNAT,
		Excluded: ports.ExcludedRange{Range: ports.Range{Lo: 8100, Hi: 8199}}}}}
	b, _ := json.Marshal(libreJSON(q, r, nil))
	libre := string(b)
	for _, no := range []string{`"libres"`, `"desde"`, `"hasta"`} {
		if strings.Contains(libre, no) {
			t.Fatalf("--free --json trae %s: %s", no, libre)
		}
	}
	for _, si := range []string{`"encontrados":[8124]`, `"inicio":8123`, `"fin":65535`, `"rango":{"inicio":8100,"fin":8199`} {
		if !strings.Contains(libre, si) {
			t.Fatalf("--free --json no trae %s: %s", si, libre)
		}
	}
	rep := reaper.Report{Ports: []reaper.PortResult{{Port: 8150, Status: reaper.PortReserved, Reserved: ports.ExcludedRange{Range: ports.Range{Lo: 8100, Hi: 8199}}}}}
	b, _ = json.Marshal(construirJSONReporte(rep, reaper.Plan{}, true))
	if s := string(b); !strings.Contains(s, `"reservado":{"inicio":8100,"fin":8199`) || !strings.Contains(s, `"libres":0`) {
		t.Fatalf("reporte: %s", s)
	}
}
