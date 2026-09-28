package killport

// Reglas que antes estaban copiadas en varios lugares y ahora viven en uno:
// qué protocolos pidió el usuario con --tcp/--udp (la forma del JSON de
// http.sys está en reglas_windows_test.go).

import (
	"slices"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
)

func TestProtosPedidos(t *testing.T) {
	tcp, udp, los2 := []ports.Proto{ports.TCP}, []ports.Proto{ports.UDP}, []ports.Proto{ports.TCP, ports.UDP}
	casos := []struct {
		tcp, udp bool
		def      []ports.Proto
		want     []ports.Proto
	}{
		{false, false, tcp, tcp},
		{false, false, udp, udp},
		{false, false, los2, los2},
		{true, false, udp, tcp},
		{false, true, tcp, udp},
		{true, true, tcp, los2},
	}
	for _, c := range casos {
		if got := protosPedidos(c.tcp, c.udp, c.def...); !slices.Equal(got, c.want) {
			t.Errorf("tcp=%v udp=%v def=%v: %v, esperaba %v", c.tcp, c.udp, c.def, got, c.want)
		}
	}
	// --free y --config con los mismos flags buscan en los mismos protocolos.
	for _, fl := range [][2]bool{{false, false}, {true, false}, {false, true}, {true, true}} {
		q, err := consultaLibre(nil, fl[0], fl[1], 1)
		if err != nil {
			t.Fatal(err)
		}
		cfg := pedidoConfig{TCP: fl[0], UDP: fl[1]}.protocolos(ports.TCP)
		if !slices.Equal(q.Protos, cfg) {
			t.Errorf("flags %v: --free busca en %v y --config cambia %v", fl, q.Protos, cfg)
		}
	}
}
