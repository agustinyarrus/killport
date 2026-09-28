package killport

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
)

// La consulta de --free a partir de la línea de comandos.
func TestConsultaLibre(t *testing.T) {
	tcp := []ports.Proto{ports.TCP}
	casos := []struct {
		pos      []string
		tcp, udp bool
		count    int
		within   ports.Range
		explicit bool
		protos   []ports.Proto
		err      string
	}{
		{pos: nil, within: ports.Range{Lo: 1024, Hi: 49151}, protos: tcp},
		{pos: []string{"3000"}, within: ports.Range{Lo: 3000, Hi: 65535}, protos: tcp},
		{pos: []string{"3000-3999"}, within: ports.Range{Lo: 3000, Hi: 3999}, explicit: true, protos: tcp},
		{pos: []string{"3999-3000"}, within: ports.Range{Lo: 3000, Hi: 3999}, explicit: true, protos: tcp},
		{pos: []string{"5000–5003"}, within: ports.Range{Lo: 5000, Hi: 5003}, explicit: true, protos: tcp},
		{pos: []string{"3000-3000"}, within: ports.Range{Lo: 3000, Hi: 3000}, explicit: true, protos: tcp},
		{pos: []string{" 8080 "}, within: ports.Range{Lo: 8080, Hi: 65535}, protos: tcp},
		{pos: []string{"3000"}, udp: true, within: ports.Range{Lo: 3000, Hi: 65535}, protos: []ports.Proto{ports.UDP}},
		{pos: []string{"3000"}, tcp: true, udp: true, count: 5, within: ports.Range{Lo: 3000, Hi: 65535}, protos: []ports.Proto{ports.TCP, ports.UDP}},
		{pos: []string{"3000", "4000"}, err: "a lo sumo un inicio"},
		{pos: []string{"3000,4000"}, err: "no una lista"},
		{pos: []string{"localhost:3000"}, err: "TODAS las direcciones"},
		{pos: []string{":3000"}, err: "TODAS las direcciones"},
		{pos: []string{"api.local:3000"}, err: "TODAS las direcciones"},
		{pos: []string{"http://localhost:5173/"}, err: "TODAS las direcciones"},
		{pos: []string{"99999"}, err: "fuera de rango"},
		{pos: []string{"0"}, err: "no existe"},
		{pos: []string{"abc"}, err: "no es un puerto"},
	}
	for _, c := range casos {
		q, err := consultaLibre(c.pos, c.tcp, c.udp, c.count)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: error %v, esperaba %q", c.pos, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.pos, err)
			continue
		}
		if q.Within != c.within || q.Explicit != c.explicit || !slices.Equal(q.Protos, c.protos) || q.Count != max(c.count, 1) {
			t.Errorf("%q: %+v", c.pos, q)
		}
	}
}

// Cómo se cuenta cada tramo salteado.
func TestTextoSalteo(t *testing.T) {
	nombre := func(pid uint32) string {
		if pid == 812 {
			return "node.exe (PID 812 · shop-web)"
		}
		return ""
	}
	ap := netip.MustParseAddrPort
	casos := []struct {
		s    ports.Skip
		want string
	}{
		{ports.Skip{Why: ports.SkipTaken, Socket: ports.Socket{Proto: ports.TCP, Local: ap("0.0.0.0:3000"), State: ports.Listen, PID: 812}},
			"lo escucha node.exe (PID 812 · shop-web)"},
		{ports.Skip{Why: ports.SkipTaken, Socket: ports.Socket{Proto: ports.UDP, Local: ap("0.0.0.0:3000"), PID: 9}},
			"lo escucha el PID 9"},
		{ports.Skip{Why: ports.SkipTaken, Socket: ports.Socket{Proto: ports.TCP, Local: ap("10.0.0.2:3000"), State: ports.Established, PID: 812}},
			"lo usa node.exe (PID 812 · shop-web) en una conexión (conectado)"},
		{ports.Skip{Why: ports.SkipWinNAT, Excluded: ports.ExcludedRange{Range: ports.Range{Lo: 2980, Hi: 3079}}},
			"lo reservó WinNAT para Hyper-V, WSL o Docker (bloque 2980–3079)"},
		{ports.Skip{Why: ports.SkipAdministered, Excluded: ports.ExcludedRange{Range: ports.Range{Lo: 50000, Hi: 50059}, Administered: true}},
			"exclusión administrada 50000–50059: alguien lo apartó a mano; se puede usar, pero no lo ofrezco"},
		{ports.Skip{Why: ports.SkipDynamic, Dynamic: ports.DynamicRange{Start: 1025, Count: 3976}},
			"rango dinámico 1025–5000: Windows lo reparte a las conexiones salientes"},
		{ports.Skip{Why: ports.SkipProbe, Proto: ports.TCP, Family: ports.IPv6, Bind: ports.BindInUse},
			"alguien lo tiene atado sin escuchar (TCP IPv6)"},
		{ports.Skip{Why: ports.SkipProbe, Proto: ports.UDP, Family: ports.IPv4, Bind: ports.BindForbidden},
			"Windows no deja atarlo (UDP IPv4)"},
		{ports.Skip{Why: ports.SkipProbe, Proto: ports.TCP, Family: ports.IPv4, Bind: ports.BindError},
			"no lo pude sondear (TCP IPv4)"},
	}
	for _, c := range casos {
		if got := textoSalteo(c.s, nombre); got != c.want {
			t.Errorf("%v: %q, esperaba %q", c.s.Why, got, c.want)
		}
	}
	if got := listaPuertosTexto([]uint16{3002, 3003, 3005}); got != "3002, 3003 y 3005" {
		t.Errorf("lista: %q", got)
	}
	if got := protosTexto([]ports.Proto{ports.TCP, ports.UDP}); got != "TCP y UDP" {
		t.Errorf("protos: %q", got)
	}
}
