package ports

import (
	"net/netip"
	"testing"
)

func sock(proto Proto, local string, st State, pid uint32) Socket {
	return Socket{Proto: proto, Local: netip.MustParseAddrPort(local), State: st, PID: pid}
}

func TestRelate(t *testing.T) {
	f := Filter{Ports: Of(3000)}
	all := Filter{Ports: Of(3000), AllStates: true}
	casos := []struct {
		name string
		f    Filter
		s    Socket
		want Relation
	}{
		{"LISTEN ocupa", f, sock(TCP, "0.0.0.0:3000", Listen, 10), Holds},
		{"UDP ocupa siempre", f, sock(UDP, "[::]:3000", StateNone, 10), Holds},
		{"otro puerto", f, sock(TCP, "0.0.0.0:3001", Listen, 10), Unrelated},
		{"conexión sin --all-states", f, sock(TCP, "127.0.0.1:3000", Established, 10), Unrelated},
		{"conexión con --all-states", all, sock(TCP, "127.0.0.1:3000", Established, 10), Holds},
		{"TIME_WAIT del PID 0: resto", all, sock(TCP, "127.0.0.1:3000", TimeWait, 0), Residue},
		{"TIME_WAIT del PID 0 sin --all-states", f, sock(TCP, "127.0.0.1:3000", TimeWait, 0), Unrelated},
		{"solo --udp descarta TCP", Filter{Ports: Of(3000), UDP: true}, sock(TCP, "0.0.0.0:3000", Listen, 10), Unrelated},
		{"solo --tcp descarta UDP", Filter{Ports: Of(3000), TCP: true}, sock(UDP, "0.0.0.0:3000", StateNone, 10), Unrelated},
		{"filtro sin puertos = todos", Filter{}, sock(TCP, "0.0.0.0:49152", Listen, 10), Holds},
	}
	for _, c := range casos {
		if got := c.f.Relate(c.s); got != c.want {
			t.Errorf("%s: Relate = %v, esperaba %v", c.name, got, c.want)
		}
	}
	// El puerto REMOTO no cuenta: un cliente conectado a un 3000 ajeno no
	// ocupa el 3000 local.
	cli := sock(TCP, "127.0.0.1:51000", Established, 10)
	cli.Remote = netip.MustParseAddrPort("127.0.0.1:3000")
	if all.Relate(cli) != Unrelated {
		t.Error("una conexión saliente hacia el 3000 no ocupa el 3000 local")
	}
}

func TestEstados(t *testing.T) {
	if Listen.String() != "LISTEN" || Listen.Label() != "escuchando" || Established.Label() != "conectado" {
		t.Error("nombres de estado")
	}
	if TimeWait.Label() != "TIME_WAIT" || DeleteTCB.String() != "DELETE_TCB" {
		t.Error("TIME_WAIT / DELETE_TCB")
	}
	for s := State(0); s <= DeleteTCB; s++ {
		if !s.Valid() || (s != StateNone && (s.String() == "" || s.Label() == "")) {
			t.Errorf("estado %d sin nombre", s)
		}
	}
	if State(13).Valid() || State(13).String() != "State(13)" {
		t.Error("un estado fuera de MIB_TCP_STATE no es válido")
	}
	if sock(TCP, "[::ffff:127.0.0.1]:1", Listen, 1).Family() != IPv6 || sock(TCP, "127.0.0.1:1", Listen, 1).Family() != IPv4 {
		t.Error("familia")
	}
}
