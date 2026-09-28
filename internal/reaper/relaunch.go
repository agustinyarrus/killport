package reaper

import (
	"context"
	"strings"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Relanzados que todavía no escuchan.
//
// La ventana Settle de la verificación (unos 300 ms) atrapa al supervisor que
// retoma el puerto al toque. Pero un supervisor de verdad (pm2, nodemon, el
// recargador de uvicorn) relanza en unos 50 ms a un proceso que tarda de
// medio segundo a varios en volver a escuchar: cuando se confirma que el
// puerto sigue libre, el relanzado todavía está arrancando, killport dice
// "libre", sale 0, y el `killport 3000 -y && npm start` choca un segundo
// después con el puerto tomado.
//
// La evidencia está en el bosque de después: un proceso NUEVO (no estaba en
// la foto con la que se planificó) con la imagen de la víctima, que cuelga de
// un ancestro de la víctima que sobrevivió. Con eso se lo espera —a que
// escuche, a que muera o al tope— y si sigue arrancando se dice quién lo está
// relanzando, sin dar el puerto por libre. Si el relanzado todavía no nació
// (un supervisor que espera antes de relanzar), la única pista es el padre
// vivo de la víctima con su misma imagen (la forma de pm2, nodemon, uvicorn y
// werkzeug): ahí se le da un rato para aparecer. Con un padre de otra imagen
// (la terminal, cmd.exe) no se espera: no hay motivo para pensar que relanza.

const (
	// relaunchAppear es cuánto se espera, desde que empieza la verificación,
	// a que un padre vivo de la misma imagen relance a la víctima.
	relaunchAppear = 1500 * time.Millisecond
	// relaunchListen es el tope para que el relanzado vuelva a escuchar,
	// desde que se lo vio nacer.
	relaunchListen = 3 * time.Second
	// relaunchPoll es el paso de la relectura (tablas y procesos) mientras
	// se espera: el kernel no avisa cuándo alguien empieza a escuchar.
	relaunchPoll = 100 * time.Millisecond
)

// relaunch es un proceso relanzado que todavía no escucha, y quién lo relanza.
type relaunch struct {
	child, sup Proc
}

// fallen es una víctima raíz que salió, con los puertos pedidos que tenía.
type fallen struct {
	root  Proc
	ports ports.Set
}

// fallenRoots son las raíces de los objetivos de procesos que salieron de
// verdad (cerraron o se las terminó): las únicas que un supervisor puede
// estar relanzando.
func fallenRoots(before *Forest, plan Plan, done Report) []fallen {
	fate := map[ID]VictimResult{}
	for _, v := range done.Victims {
		fate[v.ID.Canon()] = v
	}
	var out []fallen
	for _, t := range plan.Targets {
		if !t.process() {
			continue
		}
		v, ok := fate[t.Root.Canon()]
		if !ok || !(v.Outcome == OutcomeClosed || v.Outcome == OutcomeTerminated) {
			continue
		}
		if p, ok := before.Lookup(t.Root); ok {
			out = append(out, fallen{root: p, ports: t.Ports})
		}
	}
	return out
}

// relaunchOf busca la evidencia de que alguien relanzó a root: un proceso
// del bosque de después que no estaba antes, con la misma imagen, que nació
// después de root y que cuelga (como hijo o nieto: un supervisor suele poner
// un cmd.exe en el medio) de un ancestro cercano de root que sigue vivo —su
// padre o su abuelo—: el supervisor. Más arriba no se busca: el Explorador o
// la terminal son ancestros de todo, y un node que lance VS Code en ese
// momento no es un relanzamiento. Tampoco cuenta la cadena de killport (su
// terminal). O(n · 2) sobre los procesos nuevos de esa imagen.
func relaunchOf(before, after *Forest, root Proc, self map[ID]bool) (relaunch, bool) {
	alive := map[ID]int{} // ancestro cercano vivo → distancia a root (0 = padre)
	for i, a := range before.Ancestors(root.ID) {
		if i > 1 {
			break
		}
		if _, ok := after.Lookup(a.ID); ok && !self[a.ID] {
			alive[a.ID] = i
		}
	}
	if len(alive) == 0 {
		return relaunch{}, false
	}
	best, found, dist := relaunch{}, false, 1<<30
	for _, p := range after.All() {
		if !strings.EqualFold(p.Name, root.Name) || !p.Created.After(root.Created) {
			continue
		}
		if _, old := before.Lookup(p.ID); old {
			continue
		}
		for j, a := range after.Ancestors(p.ID) {
			if j > 1 {
				break
			}
			if d, ok := alive[a.ID]; ok {
				if d < dist || (d == dist && p.Created.Before(best.child.Created)) {
					best, found, dist = relaunch{child: p, sup: a}, true, d
				}
				break
			}
		}
	}
	return best, found
}

// mayRelaunch: el padre de root sigue vivo y tiene su misma imagen. Es la
// forma de los supervisores de desarrollo; la única pista mientras el
// relanzado todavía no nació.
func mayRelaunch(before, after *Forest, root Proc, self map[ID]bool) bool {
	p, ok := before.Parent(root.ID)
	if !ok || self[p.ID] || !strings.EqualFold(p.Name, root.Name) {
		return false
	}
	_, alive := after.Lookup(p.ID)
	return alive
}

// watchRelaunch espera a los relanzados de los puertos atacados que quedaron
// libres (ver arriba). Devuelve la última lectura de las tablas, el bosque de
// después y los puertos que siguen libres con un relanzado vivo que todavía
// no escucha. Relee con un paso fijo corto, hasta que nadie esté arrancando
// o al tope; respeta la cancelación.
func watchRelaunch(ctx context.Context, os OS, before, after *Forest, roots []fallen, self map[ID]bool, sc *verifyScope, socks []ports.Socket) ([]ports.Socket, *Forest, map[uint16]relaunch) {
	start := os.Now()
	var listenBy time.Time
	for {
		held := sc.holders(socks)
		pending := map[uint16]relaunch{}
		waitAppear := false
		for _, r := range roots {
			var free []uint16
			for p := range r.ports.All() {
				if _, h := held[p]; !h {
					free = append(free, p)
				}
			}
			if len(free) == 0 {
				continue
			}
			if rl, ok := relaunchOf(before, after, r.root, self); ok {
				for _, p := range free {
					pending[p] = rl
				}
			} else if mayRelaunch(before, after, r.root, self) {
				waitAppear = true
			}
		}
		now := os.Now()
		switch {
		case len(pending) > 0:
			if listenBy.IsZero() {
				listenBy = now.Add(relaunchListen)
			}
			if !now.Before(listenBy) {
				return socks, after, pending
			}
		case !waitAppear || !now.Before(start.Add(relaunchAppear)):
			return socks, after, nil
		}
		if !os.Sleep(ctx, relaunchPoll) {
			return socks, after, pending
		}
		socks, _ = os.Sockets(ctx)
		if procs, err := os.Snapshot(ctx); err == nil {
			after = NewForest(procs)
		}
	}
}

// relaunchResult es el dictamen de un puerto libre que un supervisor está
// por volver a tomar: no se lo da por libre.
func relaunchResult(p uint16, rl relaunch) PortResult {
	return PortResult{
		Port: p, Status: PortRespawned, Starting: true,
		Owner: rl.child.ID, OwnerName: rl.child.Name,
		Supervisor: rl.sup.ID, SupervisorName: rl.sup.Name,
		Reason: procLabel(rl.sup) + " lo relanzó: " + procLabel(rl.child) + " arrancó de nuevo y todavía no escucha",
		Advice: "cerrá ese supervisor, " + procLabel(rl.sup) + ", y después el puerto queda libre",
	}
}
