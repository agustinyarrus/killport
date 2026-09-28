//go:build windows

package killport

// El JSON de http.sys: los otros procesos del mismo puerto son del mismo tipo
// que el principal (antes era un tipo copiado, jsonHTTPOtro).

import (
	"encoding/json"
	"testing"
)

func TestJSONHTTPOtrosMismaForma(t *testing.T) {
	h := jsonHTTP{PID: 4812, Proceso: "api.exe", URLs: []string{"http://+:8080/api/"},
		Otros: []jsonHTTP{{PID: 7304, Proceso: "panel.exe", URLs: []string{"http://+:8080/panel/"}}}}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"pid":4812,"proceso":"api.exe","urls":["http://+:8080/api/"],"otros":[{"pid":7304,"proceso":"panel.exe","urls":["http://+:8080/panel/"]}]}`
	if string(b) != want {
		t.Fatalf("JSON de http.sys:\n%s\nesperaba\n%s", b, want)
	}
}
