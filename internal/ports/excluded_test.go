package ports

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Salidas REALES de esta máquina (Windows 11 en español), capturadas con
// `netsh interface ipvX show excludedportrange protocol=Y > archivo`. Hoy no
// hay rangos TCP; en UDP hay uno administrado (50000–50059 *).
func TestExcludedSalidaReal(t *testing.T) {
	casos := []struct {
		file  string
		proto Proto
		fam   Family
		want  string // rangos, "" = ninguno
		admin bool
	}{
		{"netsh_es_ipv4_tcp.txt", TCP, IPv4, "", false},
		{"netsh_es_ipv6_tcp.txt", TCP, IPv6, "", false},
		{"netsh_es_ipv4_udp.txt", UDP, IPv4, "50000–50059", true},
		{"netsh_es_ipv6_udp.txt", UDP, IPv6, "50000–50059", true},
	}
	for _, c := range casos {
		raw, err := os.ReadFile(filepath.Join("testdata", c.file))
		if err != nil {
			t.Fatal(err)
		}
		xs, err := ParseExcluded(string(raw), c.proto, c.fam)
		if err != nil {
			t.Errorf("%s: %v", c.file, err)
			continue
		}
		if got := xs.Set(c.proto, c.fam).String(); got != c.want {
			t.Errorf("%s: rangos %q, esperaba %q", c.file, got, c.want)
		}
		for _, x := range xs {
			if x.Administered != c.admin || x.Proto != c.proto || x.Family != c.fam {
				t.Errorf("%s: fila %+v", c.file, x)
			}
		}
	}
}

// Sintéticas: inglés, varias filas, con y sin "*", la palabra acentuada en
// CP850 (0xA2 = ó) como la escribiría una versión vieja, y fin de línea LF.
func TestExcludedSintetica(t *testing.T) {
	en := "\r\nProtocol tcp Port Exclusion Ranges\r\n\r\nStart Port    End Port\r\n----------    --------\r\n" +
		"      1080        1179\r\n      2869        2869     *\r\n     50000       50059     *\r\n\r\n* - Administered port exclusions.\r\n"
	xs, err := ParseExcluded(en, TCP, IPv4)
	if err != nil {
		t.Fatal(err)
	}
	if got := xs.Set(TCP, IPv4).String(); got != "1080–1179, 2869, 50000–50059" {
		t.Errorf("inglés: %q", got)
	}
	if r, ok := xs.Find(TCP, IPv4, 1100); !ok || r.Administered || r.Lo != 1080 {
		t.Errorf("Find(1100) = %+v, %v", r, ok)
	}
	if r, ok := xs.Find(TCP, IPv4, 2869); !ok || !r.Administered {
		t.Errorf("Find(2869) = %+v, %v", r, ok)
	}
	if _, ok := xs.Find(TCP, IPv6, 1100); ok {
		t.Error("una fila IPv4 no cubre IPv6")
	}
	if _, ok := xs.Find(UDP, IPv4, 1100); ok {
		t.Error("una fila TCP no cubre UDP")
	}

	cp850 := "\r\nProtocolo tcp Intervalos de exclusi\xa2n de puertos\r\n\r\nPuerto de inicio    Puerto final\r\n" +
		"----------    --------\r\n      5357        5357\r\n\r\n* - Exclusiones de puertos administrados.\r\n"
	xs, err = ParseExcluded(cp850, TCP, IPv6)
	if err != nil || xs.Set(TCP, IPv6).String() != "5357" {
		t.Errorf("CP850: %v, %v", xs, err)
	}

	lf := strings.ReplaceAll(en, "\r\n", "\n")
	if xs, err := ParseExcluded(lf, TCP, IPv4); err != nil || len(xs) != 3 {
		t.Errorf("LF: %v, %v", xs, err)
	}

	// Rangos solapados (WinNAT y un administrador reservando lo mismo): Find
	// devuelve el de inicio más bajo que lo contiene.
	over := "----\n 1000 2000\n 1500 1600 *\n"
	xs, err = ParseExcluded(over, UDP, IPv4)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := xs.Find(UDP, IPv4, 1550); r.Lo != 1000 {
		t.Errorf("solapados: %+v", r)
	}
}

func TestExcludedErrores(t *testing.T) {
	casos := map[string]string{
		"El servicio no se está ejecutando.\r\n": "no tiene la tabla",
		"":                                         "no tiene la tabla",
		"------\r\n  70000   70001\r\n":            "fuera de rango",
		"------\r\n  5000   4000\r\n":              "dado vuelta",
		"------\r\n  5000   5001   x\r\n":          "esperaba",
		"------\r\n  5000\r\n":                     "esperaba",
		"------\r\n  5000   50a1\r\n":              `"50a1" no es un puerto`,
		"------\r\n  1   2\r\n  3   4   *   *\r\n": "línea 3",
	}
	for in, want := range casos {
		_, err := ParseExcluded(in, TCP, IPv4)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseExcluded(%q): error = %v, esperaba que diga %q", in, err, want)
		}
	}
}
