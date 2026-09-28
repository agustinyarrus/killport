package reaper

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Lo que dice la verificación cuando no se actuó, o cuando en un puerto hay
// más de una negativa. "Sigue ocupado por el mismo proceso" y "volvé a correr
// killport" son frases de DESPUÉS de actuar: dichas cuando solo se miró (--json
// sin --yes), o sobre alguien que ya era objetivo, mienten.

// Dos negativas en el mismo puerto (el motor de contenedores y el wslrelay de
// la misma publicación, con --no-docker): cualquiera de los dos que la tabla
// muestre primero es una negativa, no "sigue ocupado por el mismo proceso".
func TestVerifyDosNegativasEnElMismoPuerto(t *testing.T) {
	motor := ID{PID: 300, Created: at(1)}
	relay := ID{PID: 400, Created: at(2)}
	f := NewForest([]Proc{
		{ID: motor, Name: "com.docker.backend.exe", Parent: 1, Path: `C:\Program Files\Docker\Docker\resources\com.docker.backend.exe`},
		{ID: relay, Name: "wslrelay.exe", Parent: 1, Path: `C:\Program Files\WSL\wslrelay.exe`},
	})
	sm := tcpListen(8770, 300, at(5))
	sr := ports.Socket{Proto: ports.TCP, Local: netip.AddrPortFrom(netip.IPv6Loopback(), 8770), State: ports.Listen, PID: 400, Since: at(6)}
	pl := PlanKill(ports.Of(8770), []ports.Socket{sm, sr}, f, nil, Config{SystemRoot: winRoot})
	if len(pl.Refusals) != 2 {
		t.Fatalf("negativas = %+v", pl.Refusals)
	}
	idx := newPortIndex(f, pl, nil, nil)
	for _, s := range []ports.Socket{sm, sr} {
		res := classifyOne(f, f, nil, idx, nil, map[uint16]ports.Socket{8770: s}, 8770)
		if res.Status != PortRefused || strings.Contains(res.Reason, "mismo proceso") {
			t.Fatalf("con el socket de %d primero: %+v", s.PID, res)
		}
	}
}

// Solo mirar: el objetivo no fue víctima. Se dice quién lo tiene.
func TestVerifySinActuarDiceQuienLoTiene(t *testing.T) {
	s := tcpListen(3000, 100, at(10))
	f := NewForest(con(proc(100, 5, "node.exe", 1)))
	pl := PlanKill(ports.Of(3000), []ports.Socket{s}, f, nil, Config{SystemRoot: winRoot})
	idx := newPortIndex(f, pl, nil, nil)
	res := classifyOne(f, f, nil, idx, nil, map[uint16]ports.Socket{3000: s}, 3000)
	if res.Status != PortBusy || res.Reason != "lo tiene node.exe (PID 100)" || res.Advice != "" {
		t.Fatalf("dictamen = %+v", res)
	}
	// Con escalada, el destino dice por qué sigue.
	idx = newPortIndex(f, pl, []VictimResult{{ID: ID{PID: 100, Created: at(5)}, Name: "node.exe", Outcome: OutcomeDenied}}, nil)
	res = classifyOne(f, f, nil, idx, nil, map[uint16]ports.Socket{3000: s}, 3000)
	if res.Reason != "acceso denegado" {
		t.Fatalf("con escalada: %+v", res)
	}
}

// Un fantasma cuyo heredero YA es el objetivo de esta corrida: "volvé a correr
// killport para cerrarlo" sobra (es lo que se está haciendo).
func TestVerifyFantasmaConHerederoObjetivo(t *testing.T) {
	s := tcpListen(3000, 900, at(300))
	f := NewForest(con(proc(100, 450, "python.exe", 900)))
	pl := PlanKill(ports.Of(3000), []ports.Socket{s}, f, nil, Config{SystemRoot: winRoot})
	if len(pl.Targets) != 1 || pl.Targets[0].Kind != KillPhantom {
		t.Fatalf("plan = %+v", pl.Targets)
	}
	idx := newPortIndex(f, pl, nil, nil)
	res := classifyOne(f, f, nil, idx, nil, map[uint16]ports.Socket{3000: s}, 3000)
	if res.Status != PortPhantom || res.Owner.PID != 100 || strings.Contains(res.Advice, "volvé a correr") ||
		!strings.Contains(res.Reason, "python.exe (PID 100), que heredó el socket") {
		t.Fatalf("dictamen = %+v", res)
	}
	// Un heredero que NO es objetivo (el plan era de otro puerto): ahí sí.
	idx = newPortIndex(f, Plan{}, nil, nil)
	res = classifyOne(f, f, nil, idx, nil, map[uint16]ports.Socket{3000: s}, 3000)
	if !strings.Contains(res.Advice, "volvé a correr killport") {
		t.Fatalf("heredero ajeno al plan: %+v", res)
	}
}
