package ports

import (
	"strings"
	"testing"
)

func TestParseSpecFormas(t *testing.T) {
	casos := map[string]string{
		"3000":                             "3000",
		"8080,8081":                        "8080–8081",
		"5000-5003":                        "5000–5003",
		"5003-5000":                        "5000–5003", // dado vuelta: se normaliza
		"5000–5003":                        "5000–5003", // la raya que escribe String
		":4200":                            "4200",
		"*:4200":                           "4200",
		"localhost:8000":                   "8000",
		"127.0.0.1:3000":                   "3000",
		"0.0.0.0:3000-3002":                "3000–3002",
		"[::1]:8080":                       "8080",
		"[fe80::1%eth0]:9000":              "9000",
		"localhost:3000/app":               "3000",
		"http://localhost:5173/app":        "5173",
		"HTTP://LOCALHOST:5173":            "5173",
		"http://localhost/":                "80",
		"https://example.com":              "443",
		"https://localhost:8443/x?y#z":     "8443",
		"http://[::1]:8080/":               "8080",
		"postgres://u:clave@localhost/db":  "5432",
		"ws://127.0.0.1:9229/abc":          "9229",
		"http://localhost:5173/a,b":        "5173", // la coma es del camino
		"3000,http://localhost:5173/a,b":   "3000, 5173",
		"3000 5173":                        "3000, 5173",
		"3000,":                            "3000",
		"3000,,3001":                       "3000–3001",
		"3000, 5000–5003":                  "3000, 5000–5003",
		"03000":                            "3000",
		"1-65535":                          "1–65535",
		"8080,8081 localhost:9000,[::1]:1": "1, 8080–8081, 9000",
	}
	for in, want := range casos {
		got, err := ParseSpec(in)
		if err != nil {
			t.Errorf("ParseSpec(%q): %v", in, err)
			continue
		}
		if got.String() != want {
			t.Errorf("ParseSpec(%q) = %q, esperaba %q", in, got, want)
		}
	}
}

func TestParseSpecErrores(t *testing.T) {
	casos := map[string]string{
		"":                       "no dice ningún puerto",
		",":                      "no dice ningún puerto",
		"abc":                    `"abc" no es un puerto`,
		"70000":                  "70000 está fuera de rango",
		"99999999999999999999":   "fuera de rango",
		"0":                      "el puerto 0 no existe",
		"0-100":                  "el puerto 0 no existe",
		"5000-":                  "le falta el final",
		"-5000":                  "le falta el principio",
		"-":                      "no es un rango",
		"50a0-5003":              `"50a0" no es un número de puerto`,
		"+3000":                  "no es un puerto",
		"localhost":              "no dice el puerto (ej.: localhost:3000)",
		"127.0.0.1":              "no dice el puerto",
		"localhost:":             "no dice el puerto",
		"::1:8080":               "entre corchetes",
		"[::1]":                  "no dice el puerto",
		"[::1:8080":              "falta el ]",
		"[zz::1]:80":             "no es una dirección IPv6",
		"999.1.1.1:80":           "no es una dirección IPv4",
		"ho$t:80":                "no es un nombre de host",
		"3000/tcp":               "con --tcp",
		"http://localhost:abc/":  "no es una URL válida",
		"http://localhost:0/":    "el puerto 0 no existe",
		"http://localhost:70000": "fuera de rango",
		"ftpx://localhost/":      "no sé qué puerto usa ftpx://",
		"http:///camino":         "le falta el host",
	}
	for in, want := range casos {
		_, err := ParseSpec(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseSpec(%q): error = %v, esperaba que diga %q", in, err, want)
		}
	}
}

func TestParseSpecs(t *testing.T) {
	got, err := ParseSpecs([]string{"3000", "5173", "8080,8081", "5000-5003", ":4200", "localhost:8000", "http://localhost:5173/app"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "3000, 4200, 5000–5003, 5173, 8000, 8080–8081"; got.String() != want {
		t.Errorf("ParseSpecs = %q, esperaba %q", got, want)
	}
	if _, err := ParseSpecs([]string{"3000", "nada"}); err == nil || !strings.Contains(err.Error(), `"nada"`) {
		t.Errorf("el error tendría que citar el argumento malo: %v", err)
	}
}
