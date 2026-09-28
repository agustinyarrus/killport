package reaper

// El conjunto permitido del Ctrl+C, en un solo lugar.
//
// Un Ctrl+C no se dirige a un proceso: le llega a TODA la consola. Por eso el
// motor lo manda solo si cada miembro de la consola está en el conjunto
// permitido (sendGraceful), y la herramienta, ANTES de preguntar, avisa a
// quién más le va a llegar o que no se va a poder mandar (CtrlCOutlook). Las
// dos cosas tienen que salir de la misma regla: si el aviso usara una propia,
// podría prometer "el Ctrl+C le llega también a su padre" cuando el motor, por
// tener al padre al margen del pedido, no lo manda y termina al hijo en seco
// (pasó: lo vio la regresión final, con un padre en 0.0.0.0 y su hijo en
// [::1] en la misma consola).

// ctrlCAllow suma a allow el conjunto permitido de un Ctrl+C a la consola de
// la víctima id: apretar Ctrl+C en esa ventana cerraría a id, a sus ancestros
// (comparten la consola) y a su subárbol. Nadie más, y por identidad (el
// ayudante compara también la hora de creación).
//
// Lo que quedó al margen del pedido (aside) no entra, sea ancestro o
// descendiente: `killport localhost:5173` sobre un hijo que comparte la
// consola con su padre —el que escucha 0.0.0.0, que no se pidió— no puede
// mandar un Ctrl+C que cierre también al padre. Con él en la consola, el
// Ctrl+C no se manda y el hijo se termina en seco. Tampoco entra un
// descendiente que skip excluya (el motor: uno marcado crítico, que el Ctrl+C
// bajaría con los demás). O(profundidad + subárbol).
func ctrlCAllow(before *Forest, id ID, aside map[ID]bool, skip func(ID) bool, allow map[ID]bool) {
	allow[id] = true
	for _, a := range before.Ancestors(id) {
		if !aside[a.ID] {
			allow[a.ID] = true
		}
	}
	for _, d := range before.Subtree(id) {
		if aside[d.ID] || (skip != nil && skip(d.ID)) {
			continue
		}
		allow[d.ID] = true
	}
}

// asideOf es el conjunto de identidades que quedaron al margen del pedido.
func asideOf(plan Plan) map[ID]bool {
	aside := map[ID]bool{}
	for _, b := range plan.Bystanders {
		if !b.Owner.Zero() {
			aside[b.Owner.Canon()] = true
		}
	}
	return aside
}

// CtrlCOutlook es lo que va a pasar con el Ctrl+C del cierre prolijo en la
// consola de un objetivo, dicho ANTES de actuar.
type CtrlCOutlook struct {
	// Also son los que lo recibirían sin ser víctimas: los ancestros que
	// comparten la consola (npm, un supervisor lanzado en la misma ventana).
	// Cierran con el objetivo, como si se apretara Ctrl+C a mano ahí.
	Also []Proc
	// Blocked son los miembros de la consola que quedan FUERA del conjunto
	// permitido: uno al margen del pedido, un proceso ajeno, uno que nació
	// después de la instantánea (va sin nombre). Con cualquiera de ellos ahí
	// el Ctrl+C no se manda y el objetivo se termina en seco.
	Blocked []Proc
}

// CtrlCOutlook predice el Ctrl+C a una consola cuyos miembros son pids (la
// lista de GetConsoleProcessList, sin el ayudante que la leyó). Usa la regla
// del motor (ctrlCAllow) y su agrupamiento: el motor manda UN Ctrl+C por
// consola, con el conjunto permitido de todas las víctimas vivas que están en
// ella, así que acá se une el de cada víctima del plan cuyo PID figura en la
// consola (salvo las de un servicio: esas las baja el administrador de
// servicios, no un Ctrl+C). No sabe lo que el motor recién ve al abrir los
// handles (un descendiente marcado crítico): es un aviso, y la narración de
// la escalada cuenta lo que pasó de verdad.
func (p Plan) CtrlCOutlook(before *Forest, pids []uint32) CtrlCOutlook {
	aside := asideOf(p)
	victim := map[ID]bool{}
	allow := map[ID]bool{}
	for _, t := range p.Targets {
		for _, id := range t.Victims {
			id = id.Canon()
			victim[id] = true
			if t.Kind == StopThirdSvc || !consoleHas(pids, id.PID) {
				continue
			}
			if cur, ok := before.LivePID(id.PID); !ok || cur.ID != id {
				continue // ya no es ese proceso: el motor no lo abriría
			}
			ctrlCAllow(before, id, aside, nil, allow)
		}
	}
	var out CtrlCOutlook
	if len(allow) == 0 {
		return out // ninguna víctima en esa consola: no es de este plan
	}
	for _, pid := range pids {
		cur, ok := before.LivePID(pid)
		switch {
		case !ok:
			out.Blocked = append(out.Blocked, Proc{ID: ID{PID: pid}})
		case !allow[cur.ID]:
			out.Blocked = append(out.Blocked, cur)
		case !victim[cur.ID]:
			out.Also = append(out.Also, cur)
		}
	}
	return out
}
