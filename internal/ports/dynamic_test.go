package ports

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Las muestras "es" son la salida REAL de netsh en esta máquina.
func TestDinamicoMuestras(t *testing.T) {
	casos := []struct {
		file  string
		proto Proto
		fam   Family
	}{
		{"netsh_es_dynamic_ipv4_tcp.txt", TCP, IPv4},
		{"netsh_es_dynamic_ipv6_udp.txt", UDP, IPv6},
	}
	for _, c := range casos {
		raw, err := os.ReadFile(filepath.Join("testdata", c.file))
		if err != nil {
			t.Fatal(err)
		}
		lf := strings.ReplaceAll(string(raw), "\r\n", "\n")
		for forma, texto := range map[string]string{"tal cual": string(raw), "LF": lf, "CRLF": strings.ReplaceAll(lf, "\n", "\r\n")} {
			d, err := ParseDynamic(texto, c.proto, c.fam)
			if err != nil {
				t.Errorf("%s (%s): %v", c.file, forma, err)
				continue
			}
			if d.Proto != c.proto || d.Family != c.fam || d.Start != 49152 || d.Count != 16384 {
				t.Errorf("%s (%s): %+v", c.file, forma, d)
			}
			if !d.Default() || d.Low() || d.Range() != (Range{49152, 65535}) {
				t.Errorf("%s: de fábrica=%v bajo=%v rango=%v", c.file, d.Default(), d.Low(), d.Range())
			}
		}
	}
}

func TestDinamicoFormas(t *testing.T) {
	const cabecera = "\nProtocol tcp Dynamic Port Range\n---------------------------------\n"
	buenas := []struct {
		out          string
		start        uint16
		count        int
		lo, hi       uint16
		fabrica, low bool
	}{
		// El rango de Windows XP, que todavía restauran algunos instaladores:
		// pisa los puertos de desarrollo.
		{cabecera + "Start Port      : 1025\nNumber of Ports : 3976\n", 1025, 3976, 1025, 5000, false, true},
		{cabecera + "Start Port      : 49152\nNumber of Ports : 16384\n", 49152, 16384, 49152, 65535, true, false},
		// El que recomienda Microsoft para un servidor con muchas conexiones.
		{cabecera + "Start Port      : 10000\nNumber of Ports : 55536\n", 10000, 55536, 10000, 65535, false, true},
		// Distinto del de fábrica pero arriba: no molesta a nadie.
		{cabecera + "Start Port      : 50000\nNumber of Ports : 255\n", 50000, 255, 50000, 50254, false, false},
		// Otro idioma, otra sangría, líneas de más: se ancla en los guiones y
		// toma los dos valores que siguen a un ":".
		{"Xyz\n\n   ----   -----  \n\n  Qwerty uiop:49152\n  prosa sin valor\n  Asdf  :   16384   \n\n", 49152, 16384, 49152, 65535, true, false},
		{cabecera + "Start Port      : 65535\nNumber of Ports : 1\n", 65535, 1, 65535, 65535, false, false},
	}
	for _, c := range buenas {
		d, err := ParseDynamic(c.out, TCP, IPv4)
		if err != nil {
			t.Errorf("%q: %v", c.out, err)
			continue
		}
		if d.Start != c.start || d.Count != c.count || d.Range() != (Range{c.lo, c.hi}) || d.Default() != c.fabrica || d.Low() != c.low {
			t.Errorf("%q: %+v rango %v de fábrica %v bajo %v", c.out, d, d.Range(), d.Default(), d.Low())
		}
	}

	malas := map[string]string{
		"sin guiones":       "Start Port : 49152\nNumber of Ports : 16384\n",
		"vacía":             "",
		"un solo valor":     cabecera + "Start Port      : 49152\n",
		"tres valores":      cabecera + "A : 1\nB : 2\nC : 3\n",
		"inicio cero":       cabecera + "Start Port      : 0\nNumber of Ports : 100\n",
		"cantidad cero":     cabecera + "Start Port      : 49152\nNumber of Ports : 0\n",
		"se pasa del 65535": cabecera + "Start Port      : 60000\nNumber of Ports : 10000\n",
		"fuera de rango":    cabecera + "Start Port      : 70000\nNumber of Ports : 10\n",
		"no es la tabla":    "El servicio no responde.\n",
	}
	for nombre, out := range malas {
		if d, err := ParseDynamic(out, TCP, IPv4); err == nil {
			t.Errorf("%s: %+v sin error", nombre, d)
		}
	}
}

func TestDinamicoBuscar(t *testing.T) {
	x := DynamicRanges{
		{Proto: TCP, Family: IPv4, Start: 1025, Count: 3976},
		{Proto: UDP, Family: IPv6, Start: 49152, Count: 16384},
	}
	if d, ok := x.Find(TCP, IPv4); !ok || d.Start != 1025 {
		t.Errorf("TCP v4: %+v %v", d, ok)
	}
	if d, ok := x.Find(UDP, IPv6); !ok || !d.Default() {
		t.Errorf("UDP v6: %+v %v", d, ok)
	}
	if _, ok := x.Find(TCP, IPv6); ok {
		t.Error("TCP v6 no está en la lista")
	}
	if _, ok := DynamicRanges(nil).Find(TCP, IPv4); ok {
		t.Error("una lista vacía no tiene nada")
	}
}
