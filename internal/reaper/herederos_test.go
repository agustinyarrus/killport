package reaper

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Herederos de un socket cuyo dueño murió (C-01). El socket del 3000 lo creó
// el PID 900 en t=300 y ese proceso ya no está. Windows le dio el 900 a otro
// proceso (t=500), que tiene un hijo propio (901, t=600). El hijo del muerto
// (100, t=450) nació con el muerto vivo: ese es el heredero.
func bosqueReciclado() *Forest {
	return NewForest([]Proc{
		proc(900, 500, "chrome.exe", 1),
		proc(901, 600, "chrome.exe", 900),
		proc(100, 450, "node.exe", 900),
		proc(102, 200, "viejo.exe", 900), // nació ANTES del socket: no lo pudo heredar
	})
}

func TestHeirsSoloSinPadreValido(t *testing.T) {
	f := bosqueReciclado()
	var got []uint32
	for _, h := range f.Heirs(900, at(300)) {
		got = append(got, h.PID)
	}
	if !slices.Equal(got, []uint32{100}) {
		t.Fatalf("herederos = %v, esperaba [100] (901 es hijo del que reusó el PID; 102 es anterior al socket)", got)
	}
	// RawChildren, en cambio, los da a todos los nacidos después: por eso no
	// alcanza para elegir a quién cerrar.
	if n := len(f.RawChildren(900, at(300))); n != 2 {
		t.Fatalf("RawChildren = %d", n)
	}
}

func TestHold(t *testing.T) {
	f := bosqueReciclado()
	if h := f.Hold(900, at(300)); h.Live || len(h.Heirs) != 1 || h.Heirs[0].PID != 100 {
		t.Fatalf("socket anterior al proceso que tiene el PID: %+v", h)
	}
	if h := f.Hold(900, at(550)); !h.Live || h.Owner.PID != 900 || len(h.Heirs) != 0 {
		t.Fatalf("socket posterior: el dueño es el de hoy: %+v", h)
	}
	if h := f.Hold(900, time.Time{}); !h.Live {
		t.Fatalf("sin hora del socket (cero) se le cree al PID: %+v", h)
	}
	if h := f.Hold(777, at(300)); h.Live || len(h.Heirs) != 0 {
		t.Fatalf("PID que no existe y sin hijos: %+v", h)
	}
}

// La verificación dice lo mismo: un puerto que sigue en manos del PID muerto
// no se atribuye al hijo del que reusó el PID.
func TestVerifyFantasmaNoNombraAlHijoDelQueReuso(t *testing.T) {
	s := tcpListen(3000, 900, at(300))
	f := NewForest(con(proc(900, 500, "chrome.exe", 1), proc(901, 600, "chrome.exe", 900)))
	res := classifyOne(f, f, nil, portIndex{}, nil, map[uint16]ports.Socket{3000: s}, 3000)
	if res.Status != PortPhantom || len(res.Heirs) != 0 || res.Owner.PID == 901 || !strings.Contains(res.Advice, "liberarse solo") {
		t.Fatalf("dictamen = %+v", res)
	}
	// Y si el heredero de verdad está negado, no se aconseja volver a correr
	// killport (no lo va a cerrar): se dice por qué no se toca.
	self := ID{PID: 700, Created: at(350)}
	f = NewForest(con(Proc{ID: self, Name: "killport.exe", Parent: 900}))
	pl := PlanKill(ports.Of(3000), []ports.Socket{s}, f, nil, Config{Self: self, SystemRoot: winRoot})
	idx := newPortIndex(f, pl, nil, nil)
	res = classifyOne(f, f, nil, idx, nil, map[uint16]ports.Socket{3000: s}, 3000)
	if res.Status != PortRefused || res.Owner != self || !strings.Contains(res.Reason, "killport mismo") {
		t.Fatalf("heredero negado: %+v", res)
	}
}

// La espera de --wait-free abre a los herederos de verdad, no a los hijos del
// que reusó el PID.
func TestEsperaFantasmaAbreSoloHerederos(t *testing.T) {
	s := newWatchSim(at(1000))
	s.procs = []procW{
		{id: ID{PID: 900, Created: at(500)}, parent: 1},
		{id: ID{PID: 901, Created: at(600)}, parent: 900},
		{id: ID{PID: 100, Created: at(450)}, parent: 900},
	}
	o := newOwnerWaits(s)
	o.resolve(context.Background(), []ports.Socket{tcpListen(3000, 900, at(300))})
	var got []uint32
	for id := range o.open {
		got = append(got, id.PID)
	}
	if !slices.Equal(got, []uint32{100}) {
		t.Fatalf("abrió %v, esperaba solo al heredero 100", got)
	}
	o.close()
}
