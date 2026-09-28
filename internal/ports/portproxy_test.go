package ports

import (
	"math/rand/v2"
	"net/netip"
	"regexp"
	"strings"
	"testing"
)

func TestProxyClases(t *testing.T) {
	for _, k := range ProxyKinds {
		got, ok := ParseProxyKind(strings.ToUpper(k.String()))
		if !ok || got != k {
			t.Errorf("ParseProxyKind(%q) = %v, %v", k.String(), got, ok)
		}
		if want := `SYSTEM\CurrentControlSet\Services\PortProxy\` + k.String() + `\tcp`; k.RegistryPath() != want {
			t.Errorf("%v: clave %q", k, k.RegistryPath())
		}
	}
	if _, ok := ParseProxyKind("v4tov5"); ok {
		t.Error("v4tov5 no es una clase")
	}
	for k, fam := range map[ProxyKind]Family{V4ToV4: IPv4, V4ToV6: IPv4, V6ToV4: IPv6, V6ToV6: IPv6} {
		if k.ListenFamily() != fam {
			t.Errorf("%v escucha en %v", k, k.ListenFamily())
		}
	}
}

func TestProxyRegla(t *testing.T) {
	buenas := []struct {
		kind         ProxyKind
		name, data   string
		addr         string
		port         uint16
		texto, clave string
		borrar       string
	}{
		{V4ToV4, "0.0.0.0/3000", "172.24.1.5/3000", "0.0.0.0", 3000,
			"0.0.0.0:3000 → 172.24.1.5:3000", "portproxy/v4tov4/0.0.0.0/3000",
			"interface portproxy delete v4tov4 listenport=3000 listenaddress=0.0.0.0 protocol=tcp"},
		// Sin listenaddress netsh guarda "*": se borra sin nombrar la dirección.
		{V4ToV4, "*/4000", "localhost/4000", "*", 4000,
			"*:4000 → localhost:4000", "portproxy/v4tov4/*/4000",
			"interface portproxy delete v4tov4 listenport=4000 protocol=tcp"},
		{V6ToV4, "::/8080", "127.0.0.1/80", "::", 8080,
			"[::]:8080 → 127.0.0.1:80", "portproxy/v6tov4/::/8080",
			"interface portproxy delete v6tov4 listenport=8080 listenaddress=:: protocol=tcp"},
		{V4ToV6, "192.168.1.10/65535", "fd00::7/1", "192.168.1.10", 65535,
			"192.168.1.10:65535 → [fd00::7]:1", "portproxy/v4tov6/192.168.1.10/65535",
			"interface portproxy delete v4tov6 listenport=65535 listenaddress=192.168.1.10 protocol=tcp"},
		// El destino puede nombrar el puerto por servicio: solo se muestra.
		{V4ToV4, "mi-pc/80", "otra_pc/http", "mi-pc", 80,
			"mi-pc:80 → otra_pc:http", "portproxy/v4tov4/mi-pc/80",
			"interface portproxy delete v4tov4 listenport=80 listenaddress=mi-pc protocol=tcp"},
	}
	for _, c := range buenas {
		r, err := ParseProxyRule(c.kind, c.name, c.data)
		if err != nil {
			t.Errorf("%s = %s: %v", c.name, c.data, err)
			continue
		}
		if r.Kind != c.kind || r.ListenAddr != c.addr || r.ListenPort != c.port || r.Target != c.data {
			t.Errorf("%s: %+v", c.name, r)
		}
		if r.String() != c.texto || r.Key() != c.clave {
			t.Errorf("%s: texto %q, clave %q", c.name, r.String(), r.Key())
		}
		if got := strings.Join(r.DeleteArgs(), " "); got != c.borrar {
			t.Errorf("%s: borrar = %q, esperaba %q", c.name, got, c.borrar)
		}
	}

	malas := []struct{ name, data string }{
		{"", "1.2.3.4/80"},
		{"3000", "1.2.3.4/80"},                    // sin dirección/puerto
		{"/3000", "1.2.3.4/80"},                   // dirección vacía
		{"0.0.0.0/", "1.2.3.4/80"},                // sin puerto
		{"0.0.0.0/0", "1.2.3.4/80"},               // el 0 no es un puerto
		{"0.0.0.0/65536", "1.2.3.4/80"},           // fuera de rango
		{"0.0.0.0/http", "1.2.3.4/80"},            // no se puede comparar con un socket
		{"0.0.0.0/+80", "1.2.3.4/80"},             // signo
		{"0.0.0.0/ 80", "1.2.3.4/80"},             // espacio
		{"0.0.0.0 x=1/80", "1.2.3.4/80"},          // otro argumento colado
		{`0.0.0.0"/80`, "1.2.3.4/80"},             // comillas
		{"a=b/80", "1.2.3.4/80"},                  // un "=" confundiría a netsh
		{"**/80", "1.2.3.4/80"},                   // el comodín es exactamente "*"
		{"0.0.0.0%2/80", "1.2.3.4/80"},            // zona
		{strings.Repeat("a", 254) + "/80", "x/1"}, // nombre imposible
		{"0.0.0.0/80", ""},                        // sin destino
		{"0.0.0.0/80", "1.2.3.4/80 & calc"},       // destino con basura
		{"0.0.0.0/80", "1.2.3.4\x00/80"},          // control
	}
	for _, c := range malas {
		if r, err := ParseProxyRule(V4ToV4, c.name, c.data); err == nil {
			t.Errorf("ParseProxyRule(%q, %q) = %+v sin error", c.name, c.data, r)
		}
	}
	if _, err := ParseProxyRule(ProxyKind(9), "0.0.0.0/80", "1.2.3.4/80"); err == nil {
		t.Error("una clase desconocida tiene que ser un error")
	}
}

func TestProxyMisma(t *testing.T) {
	a, _ := ParseProxyRule(V4ToV4, "0.0.0.0/3000", "172.24.1.5/3000")
	for _, c := range []struct {
		kind       ProxyKind
		name, data string
		misma      bool
	}{
		{V4ToV4, "0.0.0.0/3000", "172.24.1.5/3000", true},
		{V4ToV4, "0.0.0.0/3000", "172.24.1.6/3000", false}, // cambió el destino: es otra
		{V4ToV4, "0.0.0.0/3001", "172.24.1.5/3000", false},
		{V4ToV4, "127.0.0.1/3000", "172.24.1.5/3000", false},
		{V4ToV6, "0.0.0.0/3000", "172.24.1.5/3000", false},
	} {
		b, err := ParseProxyRule(c.kind, c.name, c.data)
		if err != nil {
			t.Fatal(err)
		}
		if a.Same(b) != c.misma || b.Same(a) != c.misma {
			t.Errorf("%v vs %v: misma = %v", a, b, a.Same(b))
		}
	}
	h1, _ := ParseProxyRule(V4ToV4, "MI-PC/80", "Destino/80")
	h2, _ := ParseProxyRule(V4ToV4, "mi-pc/80", "destino/80")
	if !h1.Same(h2) {
		t.Error("los nombres de máquina no distinguen mayúsculas")
	}
}

func sockTCP(local string, state State) Socket {
	return Socket{Proto: TCP, Local: netip.MustParseAddrPort(local), State: state, PID: 1500}
}

func TestProxyEsSuSocket(t *testing.T) {
	regla := func(kind ProxyKind, name string) ProxyRule {
		r, err := ParseProxyRule(kind, name, "10.0.0.1/80")
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	casos := []struct {
		r    ProxyRule
		s    Socket
		want bool
	}{
		{regla(V4ToV4, "0.0.0.0/3000"), sockTCP("0.0.0.0:3000", Listen), true},
		{regla(V4ToV4, "*/3000"), sockTCP("0.0.0.0:3000", Listen), true},
		{regla(V4ToV4, "*/3000"), sockTCP("127.0.0.1:3000", Listen), false}, // "*" ata la comodín
		{regla(V4ToV4, "0.0.0.0/3000"), sockTCP("0.0.0.0:3001", Listen), false},
		{regla(V4ToV4, "0.0.0.0/3000"), sockTCP("[::]:3000", Listen), false}, // otra familia
		{regla(V4ToV6, "127.0.0.1/3000"), sockTCP("127.0.0.1:3000", Listen), true},
		{regla(V4ToV4, "127.0.0.1/3000"), sockTCP("192.168.1.2:3000", Listen), false},
		{regla(V6ToV4, "::/3000"), sockTCP("[::]:3000", Listen), true},
		{regla(V6ToV6, "*/3000"), sockTCP("[::]:3000", Listen), true},
		{regla(V6ToV4, "::1/3000"), sockTCP("[::1]:3000", Listen), true},
		{regla(V6ToV4, "fe80::1/3000"), sockTCP("[fe80::1%7]:3000", Listen), true}, // la zona es del socket, no de la regla
		{regla(V6ToV4, "::/3000"), sockTCP("0.0.0.0:3000", Listen), false},
		{regla(V4ToV4, "mi-pc/3000"), sockTCP("192.168.1.2:3000", Listen), true}, // un nombre: alcanza con puerto y familia
		{regla(V4ToV4, "mi-pc/3000"), sockTCP("[::]:3000", Listen), false},
	}
	for _, c := range casos {
		if got := c.r.Holds(c.s); got != c.want {
			t.Errorf("%v tiene %v = %v, esperaba %v", c.r, c.s.Local, got, c.want)
		}
	}
	udp := Socket{Proto: UDP, Local: netip.MustParseAddrPort("0.0.0.0:3000")}
	if regla(V4ToV4, "0.0.0.0/3000").Holds(udp) {
		t.Error("portproxy es solo TCP")
	}
}

// Propiedad de seguridad: de una regla que el parser aceptó, el comando que la
// borra sale SIEMPRE con la misma forma, y ningún argumento puede leerse como
// dos (sin espacios, sin comillas, un solo "=").
func TestProxyPropiedadComando(t *testing.T) {
	forma := []*regexp.Regexp{
		regexp.MustCompile(`^interface$`), regexp.MustCompile(`^portproxy$`), regexp.MustCompile(`^delete$`),
		regexp.MustCompile(`^v[46]tov[46]$`), regexp.MustCompile(`^listenport=[1-9][0-9]{0,4}$`),
	}
	direccion := regexp.MustCompile(`^listenaddress=[A-Za-z0-9.:_-]{1,253}$`)
	rng := rand.New(rand.NewPCG(7, 20260927))
	alfabeto := []byte("abzAZ019.:-_*/ =\"'&|;%\\\x00\t,")
	aceptadas := 0
	for i := 0; i < 20000; i++ {
		var name []byte
		for n := rng.IntN(12); n > 0; n-- {
			name = append(name, alfabeto[rng.IntN(len(alfabeto))])
		}
		name = append(name, '/')
		for n := 1 + rng.IntN(5); n > 0; n-- {
			name = append(name, "0123456789x "[rng.IntN(12)])
		}
		kind := ProxyKinds[rng.IntN(len(ProxyKinds))]
		r, err := ParseProxyRule(kind, string(name), "10.0.0.1/80")
		if err != nil {
			continue
		}
		aceptadas++
		args := r.DeleteArgs()
		if len(args) != 6 && len(args) != 7 {
			t.Fatalf("%q: %d argumentos: %q", name, len(args), args)
		}
		for j, re := range forma {
			if !re.MatchString(args[j]) {
				t.Fatalf("%q: argumento %d = %q", name, j, args[j])
			}
		}
		if len(args) == 7 && !direccion.MatchString(args[5]) {
			t.Fatalf("%q: dirección = %q", name, args[5])
		}
		if len(args) == 6 && r.ListenAddr != "*" {
			t.Fatalf("%q: sin dirección y la regla escucha en %q", name, r.ListenAddr)
		}
		if args[len(args)-1] != "protocol=tcp" {
			t.Fatalf("%q: último argumento %q", name, args[len(args)-1])
		}
	}
	if aceptadas < 100 {
		t.Fatalf("la propiedad casi no se ejerció: %d reglas aceptadas", aceptadas)
	}
}
