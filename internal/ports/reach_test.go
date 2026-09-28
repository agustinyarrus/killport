package ports

import (
	"fmt"
	"math/rand"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 28, 5, 49, 29, 0, time.UTC)

// escucha arma un socket TCP en LISTEN; nacido es el desfase de su hora de
// creación (la misma para las dos filas de un doble pila).
func escucha(pid uint32, dir string, port uint16, nacido time.Duration) Socket {
	return Socket{Proto: TCP, Local: netip.AddrPortFrom(netip.MustParseAddr(dir), port), State: Listen, PID: pid, Since: t0.Add(nacido)}
}

// doble es un socket de doble pila: sus dos filas.
func doble(pid uint32, port uint16, nacido time.Duration) []Socket {
	return []Socket{escucha(pid, "0.0.0.0", port, nacido), escucha(pid, "::", port, nacido)}
}

func pids(ss []Socket) []string {
	var out []string
	for _, s := range ss {
		out = append(out, fmt.Sprintf("%d@%s", s.PID, s.Local.Addr()))
	}
	return out
}

var (
	lo4 = netip.MustParseAddr("127.0.0.1")
	lo6 = netip.MustParseAddr("::1")
)

// Los casos medidos en la máquina real (t3\probe, 18 de 18): quién contestó
// cada conexión de verdad.
func TestAnswerComoLoMidioWindows(t *testing.T) {
	const p = 5173
	casos := []struct {
		nombre     string
		socks      []Socket
		v4, v6, lh string // quién contesta 127.0.0.1, ::1 y localhost ("" = nadie)
	}{
		{"doble pila sola", doble(1, p, 0), "1@0.0.0.0", "1@::", "1@::"},
		{"doble pila + 127.0.0.1", append(doble(1, p, 0), escucha(2, "127.0.0.1", p, 1)), "2@127.0.0.1", "1@::", "1@::"},
		{"doble pila + 127.0.0.1 + [::1]", append(doble(1, p, 0), escucha(2, "127.0.0.1", p, 1), escucha(3, "::1", p, 2)), "2@127.0.0.1", "3@::1", "3@::1"},
		{"solo [::] (v6only)", []Socket{escucha(1, "::", p, 0)}, "", "1@::", "1@::"},
		{"solo 0.0.0.0", []Socket{escucha(1, "0.0.0.0", p, 0)}, "1@0.0.0.0", "", "1@0.0.0.0"},
		{"0.0.0.0 + [::1] (el del contrato)", []Socket{escucha(1, "0.0.0.0", p, 0), escucha(2, "::1", p, 1)}, "1@0.0.0.0", "2@::1", "2@::1"},
		{"doble pila + 0.0.0.0 nativo (A: nativo primero)", append([]Socket{escucha(2, "0.0.0.0", p, 0)}, doble(1, p, 1)...), "2@0.0.0.0", "1@::", "1@::"},
		{"doble pila + 0.0.0.0 nativo (B: doble primero)", append(doble(1, p, 0), escucha(2, "0.0.0.0", p, 1)), "2@0.0.0.0", "1@::", "1@::"},
		{"dos 0.0.0.0: el primero", []Socket{escucha(2, "0.0.0.0", p, 5), escucha(1, "0.0.0.0", p, 0)}, "1@0.0.0.0 2@0.0.0.0", "", "1@0.0.0.0 2@0.0.0.0"},
		// Un v4 nativo y un v6only del MISMO proceso tienen horas distintas: no
		// son un doble pila, y el nativo le gana al doble pila de otro.
		{"v4 nativo + v6only del mismo proceso, y un doble pila", append([]Socket{escucha(1, "0.0.0.0", p, 0), escucha(1, "::", p, 1)}, doble(2, p, 2)...),
			"1@0.0.0.0", "1@:: 2@::", "1@:: 2@::"},
		{"nadie", nil, "", "", ""},
	}
	for _, c := range casos {
		got4 := strings.Join(pids(Answer(lo4, TCP, p, c.socks)), " ")
		got6 := strings.Join(pids(Answer(lo6, TCP, p, c.socks)), " ")
		lh, _ := AnswerDest(Localhost, TCP, p, c.socks)
		if got4 != c.v4 || got6 != c.v6 || strings.Join(pids(lh), " ") != c.lh {
			t.Errorf("%s: 127.0.0.1→%q ::1→%q localhost→%q; esperaba %q %q %q", c.nombre, got4, got6, pids(lh), c.v4, c.v6, c.lh)
		}
	}
}

func TestAnswerOtrosPuertosYProtocolos(t *testing.T) {
	socks := []Socket{
		escucha(1, "0.0.0.0", 3000, 0),
		{Proto: TCP, Local: netip.MustParseAddrPort("127.0.0.1:3000"), Remote: netip.MustParseAddrPort("127.0.0.1:50000"), State: Established, PID: 9},
		{Proto: UDP, Local: netip.MustParseAddrPort("127.0.0.1:3000"), PID: 7},
		escucha(2, "127.0.0.1", 3001, 0),
	}
	if got := pids(Answer(lo4, TCP, 3000, socks)); !reflect.DeepEqual(got, []string{"1@0.0.0.0"}) {
		t.Errorf("TCP 3000: una conexión establecida no contesta nada, y UDP es otro protocolo: %v", got)
	}
	if got := pids(Answer(lo4, UDP, 3000, socks)); !reflect.DeepEqual(got, []string{"7@127.0.0.1"}) {
		t.Errorf("UDP 3000: %v", got)
	}
}

func TestAnswerDestAtadoYZonas(t *testing.T) {
	socks := append(doble(1, 8080, 0), escucha(2, "0.0.0.0", 8080, 1), escucha(3, "fe80::1%12", 8080, 2))
	d, _ := DestOf("0.0.0.0")
	who, _ := AnswerDest(d, TCP, 8080, socks)
	if got := pids(who); !reflect.DeepEqual(got, []string{"1@0.0.0.0", "2@0.0.0.0"}) {
		t.Errorf("0.0.0.0:8080 elige lo atado ahí (las dos filas v4): %v", got)
	}
	d, _ = DestOf("::")
	who, _ = AnswerDest(d, TCP, 8080, socks)
	if got := pids(who); !reflect.DeepEqual(got, []string{"1@::"}) {
		t.Errorf("[::]:8080: %v", got)
	}
	// Una fe80:: sin zona encuentra a la atada con zona; con otra zona, no.
	if got := pids(Answer(netip.MustParseAddr("fe80::1"), TCP, 8080, socks)); !reflect.DeepEqual(got, []string{"3@fe80::1%12"}) {
		t.Errorf("fe80::1 sin zona: %v", got)
	}
	if got := pids(Answer(netip.MustParseAddr("fe80::1%7"), TCP, 8080, socks)); !reflect.DeepEqual(got, []string{"1@::"}) {
		t.Errorf("fe80::1%%7 (otra interfaz) cae en el comodín: %v", got)
	}
	// Desde la red: el comodín, con la regla del v4 nativo.
	if got := pids(AnswerNet(IPv4, TCP, 8080, socks)); !reflect.DeepEqual(got, []string{"2@0.0.0.0"}) {
		t.Errorf("desde la red por IPv4: %v", got)
	}
	if got := pids(AnswerNet(IPv6, TCP, 8080, socks)); !reflect.DeepEqual(got, []string{"1@::"}) {
		t.Errorf("desde la red por IPv6: %v", got)
	}
}

// Propiedades sobre fotos al azar: lo que contesta escucha en ese puerto y
// protocolo; si alguien está atado a la dirección exacta, contestan solo esos;
// un comodín IPv6 nunca atiende IPv4 salvo como mitad de un doble pila, y en
// ese caso solo si no hay un 0.0.0.0 nativo; el más viejo va primero.
func TestAnswerPropiedades(t *testing.T) {
	rng := rand.New(rand.NewSource(28092026))
	dirs := []string{"0.0.0.0", "::", "127.0.0.1", "::1", "192.168.1.5", "fe80::1%12", "10.0.0.2"}
	for caso := 0; caso < 20000; caso++ {
		var socks []Socket
		for i := 0; i < rng.Intn(7); i++ {
			pid := uint32(1 + rng.Intn(4))
			port := uint16(3000 + rng.Intn(2))
			nacido := time.Duration(rng.Intn(5)) * time.Millisecond
			if rng.Intn(4) == 0 {
				socks = append(socks, doble(pid, port, nacido)...)
				continue
			}
			s := escucha(pid, dirs[rng.Intn(len(dirs))], port, nacido)
			if rng.Intn(5) == 0 {
				s.State = Established
			}
			socks = append(socks, s)
		}
		addr := netip.MustParseAddr(strings.Split(dirs[2+rng.Intn(len(dirs)-2)], "%")[0])
		who := Answer(addr, TCP, 3000, socks)
		exact := false
		for _, s := range socks {
			if listens(s, TCP, 3000) && sameAddr(s.Local.Addr(), addr) {
				exact = true
			}
		}
		for i, s := range who {
			if !listens(s, TCP, 3000) {
				t.Fatalf("caso %d: contesta un socket que no escucha el 3000: %v", caso, s)
			}
			if exact != sameAddr(s.Local.Addr(), addr) {
				t.Fatalf("caso %d: exacta=%v pero contestó %v para %v", caso, exact, s.Local, addr)
			}
			if addr.Is4() && s.Local.Addr().Is6() && !sameAddr(s.Local.Addr().Unmap(), addr) {
				t.Fatalf("caso %d: un socket IPv6 contestó %v: %v", caso, addr, s.Local)
			}
			if addr.Is6() && s.Local.Addr().Is4() {
				t.Fatalf("caso %d: un socket IPv4 contestó %v", caso, addr)
			}
			if i > 0 && !who[i-1].Since.IsZero() && who[i-1].Since.After(s.Since) {
				t.Fatalf("caso %d: no está el más viejo primero: %v", caso, pids(who))
			}
		}
		if addr.Is4() && !exact {
			nativo := slices.ContainsFunc(socks, func(s Socket) bool {
				return listens(s, TCP, 3000) && s.Local.Addr() == netip.IPv4Unspecified() && !DualStack(s, socks)
			})
			for _, s := range who {
				if nativo && DualStack(s, socks) {
					t.Fatalf("caso %d: con un 0.0.0.0 nativo contestó la mitad de un doble pila", caso)
				}
			}
		}
	}
}

func TestReachOf(t *testing.T) {
	casos := map[string]Reach{
		"127.0.0.1": ReachLoopback, "127.5.0.9": ReachLoopback, "::1": ReachLoopback, "::ffff:127.0.0.1": ReachLoopback,
		"0.0.0.0": ReachAll, "::": ReachAll,
		"192.168.1.5": ReachIface, "fe80::1%12": ReachIface,
	}
	for in, want := range casos {
		if got := ReachOf(netip.MustParseAddr(in)); got != want {
			t.Errorf("ReachOf(%s) = %d, esperaba %d", in, got, want)
		}
	}
	if ReachOf(netip.Addr{}) != ReachNone {
		t.Error("una dirección inválida no llega a ningún lado")
	}
}

func TestParseRequest(t *testing.T) {
	q, err := ParseRequest([]string{"localhost:5173", "127.0.0.1:8000-8001", "3000", "[::1]:9000", "http://localhost:4200/app", "api.local:7000", "0.0.0.0:6000", "*:6001"})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := ParseSpecs([]string{"5173", "8000-8001", "3000", "9000", "4200", "7000", "6000", "6001"})
	if !q.Ports.Equal(want) {
		t.Fatalf("Ports = %v, esperaba %v", q.Ports, want)
	}
	casos := map[uint16]string{
		5173: "localhost", 8000: "127.0.0.1", 8001: "127.0.0.1", 3000: "", 9000: "[::1]", 4200: "localhost",
		7000: "", 6000: "0.0.0.0", 6001: "", 1234: "",
	}
	for p, w := range casos {
		var got []string
		for _, d := range q.Dests(p) {
			got = append(got, d.String())
		}
		if strings.Join(got, ",") != w {
			t.Errorf("Dests(%d) = %v, esperaba %q", p, got, w)
		}
	}
	if !reflect.DeepEqual(q.Names, []string{"api.local"}) {
		t.Errorf("Names = %v", q.Names)
	}
	if !q.Aimed() {
		t.Error("el pedido apunta")
	}
	if q.Label(5173) != "localhost:5173" || q.Label(3000) != "puerto 3000" {
		t.Errorf("Label: %q %q", q.Label(5173), q.Label(3000))
	}
	// Entero le gana a con dirección; dos direcciones se juntan.
	q, _ = ParseRequest([]string{"localhost:3000", "3000", "127.0.0.1:4000", "[::1]:4000", "localhost:4000", "localhost:4000"})
	if q.Dests(3000) != nil || q.Aimed() && len(q.Dests(4000)) != 3 {
		t.Errorf("3000 entero y 4000 con tres direcciones: %v %v", q.Dests(3000), q.Dests(4000))
	}
	if got := q.Label(4000); got != "127.0.0.1:4000, [::1]:4000 y localhost:4000" {
		t.Errorf("Label(4000) = %q", got)
	}
	q, _ = ParseRequest([]string{"3000", "localhost:3000"})
	if q.Aimed() {
		t.Error("todo lo que apunta también se pidió entero: no apunta")
	}
	if Whole(Of(80)).Dests(80) != nil || Whole(Of(80)).Aimed() {
		t.Error("Whole pide entero")
	}
	// Los errores siguen siendo los de siempre.
	if _, err := ParseRequest([]string{"localhost"}); err == nil {
		t.Error("localhost sin puerto es un error")
	}
	if _, err := ParseRequest([]string{"[::1]x"}); err == nil {
		t.Error("[::1]x es un error")
	}
	if _, err := ParseRequest([]string{"[127.0.0.1]:80"}); err == nil {
		t.Error("una IPv4 entre corchetes no es una IPv6")
	}
}

func TestDestOf(t *testing.T) {
	casos := map[string]string{
		"": "", "*": "", "api.local": "", "LocalHost": "localhost", "127.0.0.1": "127.0.0.1",
		"::ffff:127.0.0.1": "127.0.0.1", "::1": "[::1]", "0.0.0.0": "0.0.0.0", "::": "[::]",
	}
	for in, want := range casos {
		d, ok := DestOf(in)
		if ok != (want != "") || d.String() != want {
			t.Errorf("DestOf(%q) = %v %v, esperaba %q", in, d, ok, want)
		}
	}
	if d, _ := DestOf("0.0.0.0"); d.Kind != DestBound {
		t.Error("0.0.0.0 elige lo atado ahí")
	}
}

// DualStack (solo las pruebas) dice si s es una de las dos filas de un socket de doble pila
// entre los sockets de socks (la misma foto).
func DualStack(s Socket, socks []Socket) bool {
	if s.Since.IsZero() || !s.Local.Addr().IsUnspecified() {
		return false
	}
	var ls []Socket
	for _, x := range socks {
		if listens(x, s.Proto, s.Port()) {
			ls = append(ls, x)
		}
	}
	return dualStack(ls)[twinKey{s.PID, s.Since.UnixNano()}]
}
