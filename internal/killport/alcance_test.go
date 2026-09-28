package killport

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

var t3 = time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)

func esc(pid uint32, dir string, port uint16, nacido time.Duration) ports.Socket {
	return ports.Socket{Proto: ports.TCP, Local: netip.AddrPortFrom(netip.MustParseAddr(dir), port), State: ports.Listen, PID: pid, Since: t3.Add(nacido)}
}

func dirs(ss ...string) []netip.Addr {
	var out []netip.Addr
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

func TestTextoDirecciones(t *testing.T) {
	casos := []struct {
		in   []string
		want string
	}{
		{[]string{"0.0.0.0", "::"}, "∗ v4+v6"},
		{[]string{"0.0.0.0", "::", "::1"}, "∗ v4+v6"}, // Docker: los comodines cubren el [::1] de wslrelay
		{[]string{"127.0.0.1"}, "127.0.0.1"},
		{[]string{"::1"}, "[::1]"},
		{[]string{"0.0.0.0", "::1"}, "0.0.0.0 [::1]"},
		{[]string{"::"}, "[::]"},
	}
	for _, c := range casos {
		socks := []ports.Socket{}
		for _, d := range c.in {
			socks = append(socks, esc(1, d, 80, 0))
		}
		if got := textoDirecciones(direccionesDe(socks)); got != c.want {
			t.Errorf("%v: %q, esperaba %q", c.in, got, c.want)
		}
	}
}

func TestAlcanceDe(t *testing.T) {
	nombre := func(a netip.Addr) string {
		return map[string]string{"192.168.1.5": "Wi-Fi", "172.24.1.1": "vEthernet (WSL)"}[a.String()]
	}
	casos := []struct {
		in           []string
		texto, clave string
		ifaces       []string
	}{
		{[]string{"0.0.0.0", "::"}, "toda la red", "red", nil},
		{[]string{"0.0.0.0"}, "toda la red", "red", nil},
		{[]string{"::"}, "toda la red (v6)", "red", nil},
		{[]string{"127.0.0.1"}, "esta PC", "esta_pc", nil},
		{[]string{"::1"}, "esta PC (v6)", "esta_pc", nil},
		{[]string{"127.0.0.1", "::1"}, "esta PC", "esta_pc", nil},
		{[]string{"192.168.1.5"}, "Wi-Fi", "interfaces", []string{"Wi-Fi"}},
		{[]string{"127.0.0.1", "172.24.1.1", "10.9.9.9"}, "esta PC + WSL + 10.9.9.9", "interfaces", []string{"vEthernet (WSL)", "10.9.9.9"}},
		{[]string{"0.0.0.0", "192.168.1.5"}, "toda la red", "red", nil},
	}
	for _, c := range casos {
		al := alcanceDe(dirs(c.in...), nombre)
		if al.texto != c.texto || al.clave != c.clave || !reflect.DeepEqual(al.interfaces, c.ifaces) {
			t.Errorf("%v: %+v, esperaba %q %q %v", c.in, al, c.texto, c.clave, c.ifaces)
		}
	}
}

// El puerto compartido del contrato: node en [::1] y python en 0.0.0.0.
func TestCompartidos(t *testing.T) {
	ocs := []ocupante{
		{nombre: "node.exe (PID 812)", proceso: "node.exe", pid: 812, socks: []ports.Socket{esc(812, "::1", 5173, 0)}},
		{nombre: "python.exe (PID 990)", proceso: "python.exe", pid: 990, socks: []ports.Socket{esc(990, "0.0.0.0", 5173, time.Second)}},
		{nombre: "otro.exe (PID 7)", pid: 7, socks: []ports.Socket{esc(7, "0.0.0.0", 3000, 0)}},
		// UDP compartido (mDNS): no se marca.
		{nombre: "chrome.exe (PID 3)", pid: 3, socks: []ports.Socket{{Proto: ports.UDP, Local: netip.MustParseAddrPort("0.0.0.0:5353"), PID: 3}}},
		{nombre: "adb.exe (PID 4)", pid: 4, socks: []ports.Socket{{Proto: ports.UDP, Local: netip.MustParseAddrPort("0.0.0.0:5353"), PID: 4}}},
	}
	cs := compartidos(ocs)
	if len(cs) != 1 || cs[0].port != 5173 || len(cs[0].ocupantes) != 2 {
		t.Fatalf("compartidos = %+v", cs)
	}
	titulo, detalle := cs[0].lineas()
	want := []string{
		"localhost y [::1] → node.exe (PID 812) en [::1]",
		"127.0.0.1 y la red por IPv4 → python.exe (PID 990) en 0.0.0.0",
	}
	if !strings.HasPrefix(titulo, "5173: lo escuchan 2 procesos distintos") || !reflect.DeepEqual(detalle, want) {
		t.Fatalf("%q\n%q", titulo, detalle)
	}
	// En el JSON, datos (X-13): el proceso sin el PID pegado, las direcciones
	// sin corchetes, y la red aparte (no "la red por IPv4" entre direcciones).
	j := compartidosJSON(cs)
	if len(j) != 1 || j[0].Procesos[0].Proceso != "node.exe" || j[0].Procesos[1].PID != 990 ||
		!reflect.DeepEqual(j[0].Contestan[0].PIDs, []uint32{812}) || !reflect.DeepEqual(j[0].Contestan[0].Destinos, []string{"localhost", "::1"}) ||
		!reflect.DeepEqual(j[0].Contestan[1].Destinos, []string{"127.0.0.1"}) || !reflect.DeepEqual(j[0].Contestan[1].Red, []string{"ipv4"}) {
		t.Fatalf("JSON = %+v", j)
	}
	// Dos en la misma dirección: reparto indefinido.
	ocs = []ocupante{
		{nombre: "a (PID 1)", pid: 1, socks: []ports.Socket{esc(1, "0.0.0.0", 80, 0)}},
		{nombre: "b (PID 2)", pid: 2, socks: []ports.Socket{esc(2, "0.0.0.0", 80, time.Second)}},
	}
	_, detalle = compartidos(ocs)[0].lineas()
	// (localhost: nadie en ::1, así que cae a 127.0.0.1)
	if !reflect.DeepEqual(detalle, []string{"localhost, 127.0.0.1 y la red por IPv4 → reparto indefinido entre a (PID 1) en 0.0.0.0 y b (PID 2) en 0.0.0.0 (Windows no promete a cuál)"}) {
		t.Fatalf("empate: %q", detalle)
	}
	// Un solo proceso en dos direcciones no es compartido.
	if cs := compartidos([]ocupante{{nombre: "x", socks: []ports.Socket{esc(1, "0.0.0.0", 80, 0), esc(1, "::1", 80, 0)}}}); len(cs) != 0 {
		t.Fatalf("un solo dueño: %+v", cs)
	}
}

func TestCoincideNombre(t *testing.T) {
	casos := []struct {
		patron, nombre, ruta string
		want                 bool
	}{
		{"node", "node.exe", `C:\Program Files\nodejs\node.exe`, true},
		{"NODE.EXE", "node.exe", "", true},
		{"nod", "node.exe", "", false},
		{"python*", "python3.12.exe", "", true},
		{"py?hon", "python.exe", "", true},
		{`C:\dev\*`, "server.exe", `C:\dev\shop\bin\server.exe`, true},
		{`C:\dev\*`, "server.exe", `C:\devx\server.exe`, false},
		{`c:/dev/*/bin/*.exe`, "server.exe", `C:\dev\shop\bin\server.exe`, true},
		{`C:\dev\*.exe`, "server.exe", `C:\dev\shop\bin\server.exe`, false},
		{`C:\**\node.exe`, "node.exe", `C:\Program Files\nodejs\node.exe`, true},
		{`C:\**\node.exe`, "node.exe", "", false},
		{`C:\Program Files\nodejs\node.exe`, "node.exe", `C:\Program Files\nodejs\node.exe`, true},
	}
	for _, c := range casos {
		if got := coincideNombre(c.patron, c.nombre, c.ruta); got != c.want {
			t.Errorf("coincideNombre(%q, %q, %q) = %v", c.patron, c.nombre, c.ruta, got)
		}
	}
	var s seleccion
	if err := s.sumarNombres(" node , python* ,, "); err != nil || !reflect.DeepEqual(s.nombres, []string{"node", "python*"}) {
		t.Errorf("sumarNombres: %v %q", err, s.nombres)
	}
	if err := s.sumarNombres(","); err == nil {
		t.Error("una lista vacía es un error")
	}
	if err := s.sumarNombres("no[de"); err == nil {
		t.Error("un patrón mal formado es un error")
	}
}

func TestSeleccionSeInterseca(t *testing.T) {
	shop := &proyecto{Raiz: `C:\dev\shop`, PorScript: true}
	casos := []struct {
		sel  seleccion
		c    candidato
		want bool
	}{
		{seleccion{huerfanos: true}, candidato{nombre: "node.exe", huerfano: true}, true},
		{seleccion{huerfanos: true}, candidato{nombre: "node.exe"}, false},
		{seleccion{huerfanos: true, nombres: []string{"python"}}, candidato{nombre: "node.exe", huerfano: true}, false},
		{seleccion{huerfanos: true, nombres: []string{"node"}}, candidato{nombre: "node.exe"}, false},
		{seleccion{nombres: []string{"python", "node"}}, candidato{nombre: "node.exe"}, true},
		{seleccion{carpeta: `C:\dev\shop`}, candidato{nombre: "node.exe", cwd: `C:\DEV\Shop\web`}, true},
		{seleccion{carpeta: `C:\dev\shop`}, candidato{nombre: "node.exe", cwd: `C:\dev\shopping`}, false},
		{seleccion{carpeta: `C:\dev\shop`}, candidato{nombre: "node.exe", cwd: `C:\Windows\System32`, proyecto: shop}, true},
		{seleccion{carpeta: `C:\dev\shop`, nombres: []string{"node"}}, candidato{nombre: "python.exe", cwd: `C:\dev\shop`}, false},
	}
	for i, c := range casos {
		if got := c.sel.cumple(c.c); got != c.want {
			t.Errorf("caso %d: cumple = %v", i, got)
		}
	}
	if (seleccion{}).activa() || !(seleccion{huerfanos: true}).activa() {
		t.Error("activa")
	}
	if d := (seleccion{huerfanos: true, nombres: []string{"node", "bun"}, carpeta: `C:\x`}).describir(); d != `huérfanos, node y bun, de C:\x` {
		t.Errorf("describir = %q", d)
	}
}
