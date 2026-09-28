//go:build windows

package killport

// objetivos.go: el flujo con puertos (o con selectores). Recolecta, elige
// (seleccion.go), planifica (reaper.PlanKill), mira detrás de los
// intermediarios, se queda con lo pedido (reaper.Focus: una dirección, los
// selectores), muestra un bloque por objetivo y por negativa, pide
// confirmación, corre el motor de escalada narrando cada peldaño en vivo
// (t.StartLive) y cierra con la tarjeta de resumen. Si quedó algo que pide
// administrador y killport no corre elevado, ofrece reintentar elevado.

import (
	"errors"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

// correrPuertos es el comando con puertos: mostrar, confirmar, cerrar,
// verificar. req son los puertos pedidos con sus direcciones; con selectores
// (--orphans, --name, --project) los puertos salen de lo elegido, o se
// achican a eso si también se nombraron.
func correrPuertos(t *tui.Term, sys *sistema, req ports.Request, o opciones) int {
	ctx, stop := contexto()
	defer stop()

	forest, socks, svc, cfg, err := recolectar(ctx, sys, o)
	if err != nil {
		return fallo(t, err)
	}
	requested := req.Ports
	picked, ajenos := elegir(sys, o, forest, socks, svc, cfg)
	if picked != nil {
		elegidos := puertosElegidos(socks, o.filtro(), picked)
		if !requested.IsEmpty() {
			elegidos = elegidos.Intersect(requested)
		}
		if elegidos.IsEmpty() {
			return nadaElegido(t, o, ajenos)
		}
		requested = elegidos
	}
	hold := holding(socks, o.filtro(), requested)
	plan := reaper.PlanKill(requested, hold, forest, svc, cfg)
	// Detrás de un intermediario (Docker Desktop, wslrelay) está el objetivo
	// real: se mira y el plan pasa a apuntarle a él (el relay nunca se toca).
	plan = desenmascarar(ctx, sys, plan, socks, forest, o)
	plan = afinarNegativas(sys.svc, plan)
	// Lo pedido: el que contesta la dirección, lo que eligieron los
	// selectores. Lo demás queda al margen (después de desenmascarar: Docker
	// publica un mismo contenedor por dos intermediarios).
	plan = reaper.Focus(plan, req, picked)

	det := detallar(sys, plan, o)
	det.huerfanos = huerfanosDe(forest, plan, svc, cfg)
	if o.json {
		det.conProcesos(sys, forest, plan)
	} else {
		det.conContexto(sys, forest, plan, svc, cfg)
	}
	opts := reaper.Options{Timeout: o.timeout, Filter: o.filtro(), Settle: settleWindow}
	if !o.json && len(plan.Targets)+len(plan.Refusals) > 0 && !plan.Ownerless.IsEmpty() {
		// Lo que se promete revisar ("sin dueño en la tabla") se revisa antes
		// de preguntar: si lo reserva Windows no se entera recién al final.
		det.sinDueno = reaper.Verify(ctx, sys, forest, plan.Ownerless, plan, reaper.Report{}, reaper.Options{Filter: o.filtro()})
		det.altSinDueno = alternativasDe(ctx, sys, reaper.Report{Ports: det.sinDueno}, o)
	}

	// --json es para scripts: sale JSON puro, sin cabecera, bloques ni
	// escalada en vivo. Sin --yes solo describe; con --yes actúa (en silencio)
	// y devuelve el resultado. La elevación por UAC es interactiva: no se
	// intenta bajo --json (se reporta el acceso denegado y listo).
	if o.json {
		var rep reaper.Report
		var manuales []resultadoManual
		if o.yes && len(plan.Targets) > 0 && !todosTapados(plan, tapadosDe(plan)) {
			rep = reaper.Run(ctx, sys, forest, requested, plan, cfg, opts) // Emit nil: sin ruido
			if o.manual {
				manuales = aplicarManual(sys, plan, rep, cfg)
			}
		} else {
			rep = reaper.Report{Ports: reaper.Verify(ctx, sys, forest, requested, plan, reaper.Report{}, opts)}
		}
		doc := construirJSONReporte(rep, plan, !o.yes)
		det.anotar(&doc, plan)
		doc.Manual = manualJSON(manuales)
		doc.SinResolver = req.Names
		code := codigoJSON(rep, plan, o)
		if code != cli.ExitOK {
			doc.anotarAlternativas(alternativasDe(ctx, sys, rep, o))
		}
		escribirJSON(t, doc)
		return code
	}

	t.Lines(t.Header("killport", "libera el puerto que necesitás", o.version))
	for _, n := range req.Names {
		t.Line(t.Hint(n + ": killport no resuelve nombres (podría ser cualquier dirección); apunto a todo el puerto"))
	}

	if plan.Empty() {
		return informarLibre(t, ctx, sys, forest, requested, plan, opts, o)
	}

	t.Lines(bloques(t, sys, forest, plan, cfg, det))

	if o.list {
		t.Line(t.Hint("solo miré; killport <puerto> -y lo cierra"))
		t.Blank()
		return cli.ExitOK
	}

	var rep reaper.Report
	var manuales []resultadoManual
	que, avisado := decidir(t, sys, plan, o, det)
	switch que {
	case decisionCancelar:
		t.Blank()
		t.Line(t.Status(tui.Skip, "No cerré nada", "cancelaste"))
		t.Lines(lineasSinCerrar(t, ctx, sys, plan, o))
		t.Blank()
		return cli.ExitInterrupted
	case decisionMostrar:
		// Sin --yes ni consola para preguntar: quedó en un listado.
		t.Line(t.Hint("para cerrar hace falta --yes (no hay consola para preguntar)"))
		t.Blank()
		return cli.ExitOK
	case decisionNada, decisionInutil:
		// Solo negativas, o nada que se libere cerrando: los puertos siguen
		// ocupados. Se verifica igual para dar el veredicto y el código justo.
		if que == decisionInutil {
			t.Line(t.Status(tui.Skip, "No cerré a nadie", "aunque los cierre, "+textoTapados(tapadosDe(plan))))
		}
		rep = reaper.Report{Ports: reaper.Verify(ctx, sys, forest, requested, plan, reaper.Report{}, opts)}
	default: // decisionActuar
		rep = escalar(t, ctx, sys, forest, requested, plan, cfg, opts)
		if o.manual {
			manuales = aplicarManual(sys, plan, rep, cfg)
		}
		// Elevación: si quedó algo que pide administrador y no lo somos.
		if necesitaElevacion(rep, manuales) && !sys.esAdmin() && !rep.Canceled {
			rep, manuales = intentarElevar(t, ctx, sys, forest, requested, plan, cfg, opts, o, rep, manuales, avisado)
		}
	}

	t.Lines(lineasPuertosAlt(t, sinLosContados(rep, det.sinDueno), plan, alternativasDe(ctx, sys, rep, o)))
	t.Lines(lineasManual(t, manuales, o, plan))
	t.Lines(lineasVuelve(t, plan, rep, manuales, det))
	if que == decisionActuar {
		rep = ofrecerSupervisor(t, ctx, sys, requested, svc, cfg, opts, o, rep)
	}
	tambien := alcanzadosQueCerraron(ctx, sys, rep.Reached)
	t.Lines(lineasAlcanzados(t, tambien))
	t.Lines(tarjeta(t, rep, manuales, len(tambien)))
	return codigoDeSalida(rep)
}

// informarLibre atiende el caso "ya estaba libre": ningún socket ocupa los
// puertos pedidos (o ninguno de lo que escucha es lo pedido). Igual se
// verifica: puede estar reservado por Windows, o escucharlo alguien que no
// se pidió (y eso se dice).
func informarLibre(t *tui.Term, ctx contextoT, sys *sistema, forest *reaper.Forest, requested ports.Set, plan reaper.Plan, opts reaper.Options, o opciones) int {
	rep := reaper.Report{Ports: reaper.Verify(ctx, sys, forest, requested, plan, reaper.Report{}, opts)}
	alts := alternativasDe(ctx, sys, rep, o)
	t.Lines(lineasDiagnostico(t, rep.Ports, plan.Request(), alts))
	t.Blank()
	if o.list {
		return cli.ExitOK // solo mirar: salió bien si se pudo mirar (como el JSON)
	}
	return codigoDeSalida(rep)
}

// lineasDiagnostico cuentan puertos que nadie escucha: los libres sin nada
// que agregar van JUNTOS en una línea ("✓ 20000–20999 ya estaban libres": un
// rango libre no son mil renglones iguales), los libres con una salvedad y
// los que no se pueden usar (reservados por Windows), uno por uno, con la
// sugerencia de otro puerto. Una línea por cada uno de estos.
func lineasDiagnostico(t *tui.Term, res []reaper.PortResult, req ports.Request, alts map[uint16]uint16) []string {
	var libres []uint16
	for _, p := range res {
		if p.Status == reaper.PortFree && p.Reason == "" && len(req.Dests(p.Port)) == 0 {
			libres = append(libres, p.Port)
		}
	}
	var out []string
	switch set := ports.Of(libres...); {
	case len(libres) == 1:
		out = append(out, t.Status(tui.OK, "puerto "+u32(uint32(libres[0]))+" libre (ya estaba libre)", ""))
	case len(libres) > 1 && len(set.Ranges()) <= 6:
		out = append(out, t.Status(tui.OK, set.String()+" ya estaban libres", ""))
	case len(libres) > 1:
		out = append(out, t.Status(tui.OK, tui.Count(int64(len(libres)), "puerto", "puertos")+" ya estaban libres", ""))
	}
	for _, p := range res {
		if p.Status == reaper.PortFree {
			if p.Reason == "" && len(req.Dests(p.Port)) == 0 {
				continue
			}
			_, txt := textoPuertoDe(p, req.Label(p.Port))
			out = append(out, estado(t, tui.OK, txt+" (ya estaba libre)", p.Reason)...)
			continue
		}
		out = append(out, lineasPuertoDe(t, p, req.Label(p.Port))...)
		if a, ok := alts[p.Port]; ok {
			out = append(out, lineaAlternativa(t, a))
		}
	}
	return out
}

// cerradoEn nombra al objetivo que ocupaba ese puerto ("node.exe (PID 812)",
// o el externo), o "" si no hubo uno.
func cerradoEn(plan reaper.Plan, p uint16) string {
	for _, tg := range plan.Targets {
		if !tg.Ports.Contains(p) {
			continue
		}
		if tg.Kind == reaper.StopExternal {
			return nombreExterno(tg.External)
		}
		return quienEs(tg.Name, tg.Root.PID)
	}
	return ""
}

// sinLosContados saca del reporte los puertos que los bloques ya contaron
// (los que nadie escuchaba): después de actuar no se repiten.
func sinLosContados(rep reaper.Report, contados []reaper.PortResult) reaper.Report {
	if len(contados) == 0 {
		return rep
	}
	ya := map[uint16]bool{}
	for _, p := range contados {
		ya[p.Port] = true
	}
	var keep []reaper.PortResult
	for _, p := range rep.Ports {
		if !ya[p.Port] {
			keep = append(keep, p)
		}
	}
	rep.Ports = keep
	return rep
}

// codigoJSON es el código de salida de --json: el mismo que daría el texto en
// la misma situación. Solo mirar (--list, o sin --yes con algo que cerrar) es
// un listado que salió bien; un puerto que ya estaba libre, 0; uno que no se
// pudo liberar —negado, reservado, o que quedó ocupado después de actuar—, lo
// que diga el veredicto.
func codigoJSON(rep reaper.Report, plan reaper.Plan, o opciones) int {
	switch {
	case o.list:
		return cli.ExitOK // solo mirar: salió bien si se pudo mirar
	case plan.Empty():
		return codigoDeSalida(rep)
	case !o.yes && len(plan.Targets) > 0:
		return cli.ExitOK
	}
	return codigoDeSalida(rep)
}

// lineasPuertoDe cuenta cómo quedó un puerto que NO está libre, nombrado
// como lo pidió el usuario ("localhost:5173"): qué pasó, por qué, el consejo
// y, si hay más de un camino, cada salida con su comando.
func lineasPuertoDe(t *tui.Term, p reaper.PortResult, etiqueta string) []string {
	k, txt := textoPuertoDe(p, etiqueta)
	detalle := p.Reason
	if detalle == "" {
		detalle = p.Advice
	}
	out := estado(t, k, txt, detalle)
	if p.Reason != "" && p.Advice != "" && len(p.Remedies) == 0 {
		out = append(out, renglones(t, "    ", tui.Faint, "→ "+p.Advice)...)
	}
	return append(out, lineasRemedios(t, p.Remedies)...)
}

// lineasPuertosAlt son los renglones de los puertos que no quedaron libres
// —con la sugerencia del próximo puerto libre debajo de cada uno (alts, de
// alternativasDe)— y de los que quedaron libres PARA LO PEDIDO con alguien al
// margen: "nadie contesta [::1]; python.exe escucha en 127.0.0.1" es justo lo
// que el usuario vino a averiguar. plan dice qué se pidió y a quién se cerró.
func lineasPuertosAlt(t *tui.Term, rep reaper.Report, plan reaper.Plan, alts map[uint16]uint16) []string {
	req := plan.Request()
	var out []string
	for _, p := range rep.Ports {
		switch {
		case p.Status == reaper.PortAnswered:
			// Lo pedido era cerrar al que contestaba esa dirección: se hizo, y
			// la contesta otro. Las dos cosas, sin decir "libre".
			if quien := cerradoEn(plan, p.Port); quien != "" {
				out = append(out, t.Status(tui.OK, req.Label(p.Port)+" ya no llega a "+quien, ""))
			}
			principal, resto, _ := strings.Cut(p.Reason, ", que ")
			if resto != "" {
				resto = "que " + resto
			}
			out = append(out, estado(t, tui.Warn, principal, resto)...)
			if a, ok := alts[p.Port]; ok {
				out = append(out, lineaAlternativa(t, a))
			}
		case p.Status != reaper.PortFree:
			out = append(out, lineasPuertoDe(t, p, req.Label(p.Port))...)
			if a, ok := alts[p.Port]; ok {
				out = append(out, lineaAlternativa(t, a))
			}
		case !p.Bystander.Zero():
			_, txt := textoPuertoDe(p, req.Label(p.Port))
			out = append(out, estado(t, tui.OK, txt, p.Reason)...)
		}
	}
	return out
}

// lineasSinCerrar: el usuario dijo que no, así que los puertos siguen
// ocupados. Si le sirve otro, cuál está libre.
func lineasSinCerrar(t *tui.Term, ctx contextoT, sys *sistema, plan reaper.Plan, o opciones) []string {
	tapa := map[uint16]ports.Proto{}
	var ps []uint16
	for _, tg := range plan.Targets {
		for _, s := range tg.Sockets {
			if _, ok := tapa[s.Port()]; !ok && plan.Request().Ports.Contains(s.Port()) {
				tapa[s.Port()] = s.Proto
				ps = append(ps, s.Port())
			}
		}
	}
	slices.Sort(ps)
	alts := alternativas(ctx, sys, ps, func(p uint16) []ports.Proto { return protosAlternativa(o, tapa[p]) })
	var out []string
	for _, p := range ps {
		if a, ok := alts[p]; ok {
			out = append(out, "    "+t.Paint(tui.Faint, "→ el "+u32(uint32(p))+" sigue ocupado; ")+t.Paint(tui.Sage, "el "+u32(uint32(a))+" está libre")+t.Paint(tui.Faint, ", si te sirve otro puerto"))
		}
	}
	return out
}

// decision es qué hacer tras mostrar el plan.
type decision int

const (
	decisionActuar   decision = iota // cerrar (escalada)
	decisionNada                     // solo negativas: verificar y dar veredicto
	decisionMostrar                  // no se puede preguntar y falta --yes: solo listar
	decisionCancelar                 // el usuario dijo que no (o Ctrl+C en la pregunta)
	// decisionInutil: con --yes, cerrar no liberaría ninguno de los puertos
	// (una negativa los sigue ocupando): no se cierra a nadie por nada.
	decisionInutil
)

// decidir resuelve qué hacer. Con --yes, actuar. Con consola, preguntar [s/N].
// Sin consola ni --yes, solo listar. Un "no" del usuario es cancelación.
// avisado: la pregunta dijo que iba a hacer falta administrador y el usuario
// aceptó: no hay que volver a preguntárselo antes de elevar.
func decidir(t *tui.Term, sys *sistema, plan reaper.Plan, o opciones, det detalle) (que decision, avisado bool) {
	if len(plan.Targets) == 0 {
		return decisionNada, false // solo negativas: nada que cerrar, pero hay que informar
	}
	if o.yes {
		if todosTapados(plan, tapadosDe(plan)) {
			return decisionInutil, false
		}
		return decisionActuar, false
	}
	if !t.CanPrompt() {
		return decisionMostrar, false
	}
	admin := (planPideAdmin(plan) || det.otraSesion()) && !sys.esAdmin()
	pregunta := preguntaCierre(t, plan, o.manual, admin)
	if otros := det.alcanzados(); len(otros) > 0 {
		// Antes de confirmar: el Ctrl+C le llega también a quien comparte la
		// consola (y no es a quien se nombró).
		pregunta = strings.TrimSuffix(pregunta, t.Paint(tui.Subtle, " [s/N] ")) + t.Paint(tui.Peach, " (el Ctrl+C le llega también a "+unirY(otros)+")") + t.Paint(tui.Subtle, " [s/N] ")
	}
	resp, ok := t.Prompt(pregunta)
	if !ok || !esSi(resp) {
		return decisionCancelar, false
	}
	t.Blank()
	return decisionActuar, admin
}

// preguntaCierre arma la pregunta según cuántos objetivos y puertos hay.
// manual: la corrida es con --manual. admin: va a hacer falta administrador y
// killport no lo es (se dice ANTES de que el usuario confirme: el UAC no
// tiene que ser una sorpresa).
func preguntaCierre(t *tui.Term, plan reaper.Plan, manual, admin bool) string {
	var puertos ports.Set
	for _, tg := range plan.Targets {
		puertos = puertos.Union(tg.Ports)
	}
	// Lo que una negativa sigue ocupando no se promete liberar (tapados.go).
	ts := tapadosDe(plan)
	libres := puertos.Minus(puertosDeTapados(ts))
	que, el := "", "el "
	switch {
	case len(plan.Targets) == 1 && plan.Targets[0].Kind == reaper.StopExternal:
		x := plan.Targets[0].External
		que = dominioDe(x.Kind).verbo + " " + nombreExterno(x)
	case len(plan.Targets) == 1 && plan.Targets[0].Kind == reaper.StopThirdSvc:
		que = "Detener el servicio " + plan.Targets[0].Service
		if manual {
			que += ", dejarlo en inicio manual"
		}
	case len(plan.Targets) == 1:
		tg := plan.Targets[0]
		que = "Cerrar " + nombreVisible(tg.Name, tg.Root.PID)
	default:
		cosa := "proceso"
		for _, tg := range plan.Targets {
			if tg.Kind == reaper.StopExternal {
				cosa = "objetivo"
			}
		}
		que, el = "Cerrar "+tui.Count(int64(len(plan.Targets)), cosa, cosa+"s"), ""
	}
	cuerpo := "¿" + que + " y liberar " + el + libres.String() + "?"
	aviso := ""
	switch {
	case len(ts) > 0 && libres.IsEmpty():
		cuerpo = "¿" + que + " igual?"
		aviso = " no libera nada: " + textoTapados(ts)
	case len(ts) > 0:
		aviso = " (" + textoTapados(ts) + ")"
	}
	if admin {
		aviso += " (va a pedir permiso de administrador)"
	}
	return t.Paint(tui.Lavender, cuerpo) + t.Paint(tui.Faint, aviso) + t.Paint(tui.Subtle, " [s/N] ")
}

func esSi(s string) bool {
	switch s {
	case "s", "S", "si", "sí", "Si", "Sí", "SI", "y", "Y", "yes":
		return true
	}
	return false
}

// --- bloques "antes de actuar" ---------------------------------------------

// bloques arma un bloque por objetivo y por negativa: quién ocupa qué y por
// qué se toca o no.
func bloques(t *tui.Term, sys *sistema, forest *reaper.Forest, plan reaper.Plan, cfg reaper.Config, det detalle) []string {
	var out []string
	now := time.Now()
	for i, tg := range plan.Targets {
		if tg.Kind == reaper.StopExternal {
			out = append(out, bloqueExterno(t, tg, now, i)...)
			continue
		}
		out = append(out, bloqueObjetivo(t, sys, forest, tg, cfg, det, now, i)...)
	}
	for _, r := range negativasVistas(plan.Refusals) {
		out = append(out, bloqueNegativa(t, r, det)...)
	}
	out = append(out, lineasAlMargen(t, sys, plan)...)
	out = append(out, notasCompartidosPlan(t, plan)...)
	out = append(out, lineasTapados(t, plan)...)
	if len(det.sinDueno) > 0 {
		out = append(out, "", t.Status(tui.Info, "Sin dueño en la tabla: "+plan.Ownerless.String(), ""))
		out = append(out, lineasDiagnostico(t, det.sinDueno, plan.Request(), det.altSinDueno)...)
	}
	return append(out, "")
}

// nombreIface es sys.interfaz, tolerando un sistema nil (las pruebas).
func nombreIface(sys *sistema) func(netip.Addr) string {
	if sys == nil {
		return nil
	}
	return sys.interfaz
}

// lineasAlMargen cuentan lo que escucha un puerto pedido y no se pidió: no se
// toca, y sigue ahí (el usuario tiene que saberlo antes de confirmar: si
// después el navegador sigue mostrando algo, es eso).
func lineasAlMargen(t *tui.Term, sys *sistema, plan reaper.Plan) []string {
	var out []string
	req := plan.Request()
	for _, b := range plan.Bystanders {
		quien := b.Name
		if !b.External {
			quien = quienEs(b.Name, b.PID)
		}
		dirs := direccionesDe(b.Sockets)
		donde := "en " + textoDirecciones(dirs) + " · " + alcanceDe(dirs, nombreIface(sys)).texto
		porque := "no se pidió"
		var ds []string
		for p := range b.Ports.All() {
			for _, d := range req.Dests(p) {
				if s := d.String(); !slices.Contains(ds, s) {
					ds = append(ds, s)
				}
			}
		}
		if len(ds) > 0 {
			porque = "no contesta " + unirY(ds)
		}
		if len(out) == 0 {
			out = append(out, "")
		}
		out = append(out, estadoCon(t, tui.Margin, tui.Info, quien+" queda al margen", "escucha el "+b.Ports.String()+" "+donde+"; "+porque+": no se toca")...)
	}
	return out
}

// ocupantesDelPlan: cada objetivo, negativa y lo que quedó al margen, con sus
// sockets, para juzgar qué puertos están compartidos.
func ocupantesDelPlan(plan reaper.Plan) []ocupante {
	var ocs []ocupante
	for _, tg := range plan.Targets {
		o := ocupante{nombre: quienEs(tg.Name, tg.Root.PID), proceso: tg.Name, pid: tg.Root.PID, socks: tg.Sockets}
		if tg.Kind == reaper.StopExternal {
			o.nombre, o.proceso, o.pid, o.externo = nombreExterno(tg.External), tg.External.Name, tg.Via.PID, true
		}
		ocs = append(ocs, o)
	}
	for _, r := range negativasVistas(plan.Refusals) {
		ocs = append(ocs, ocupante{nombre: quienEs(r.Name, r.PID), proceso: r.Name, pid: r.PID, socks: r.Sockets})
	}
	for _, b := range plan.Bystanders {
		n := b.Name
		if !b.External {
			n = quienEs(b.Name, b.PID)
		}
		ocs = append(ocs, ocupante{nombre: n, proceso: b.Name, pid: b.PID, socks: b.Sockets, externo: b.External})
	}
	return ocs
}

// notasCompartidosPlan: un puerto pedido que escuchan varios, con a quién
// llega cada destino. Si el puerto se pidió entero, cómo apuntar a uno.
func notasCompartidosPlan(t *tui.Term, plan reaper.Plan) []string {
	var out []string
	for _, c := range compartidos(ocupantesDelPlan(plan)) {
		titulo, detalle := c.lineas()
		out = append(out, "", tui.Margin+t.Status(tui.Warn, titulo, ""))
		for _, d := range detalle {
			out = append(out, renglones(t, tui.Margin+"    ", tui.Faint, d)...)
		}
		if len(plan.Request().Dests(c.port)) == 0 {
			p := u32(uint32(c.port))
			out = append(out, renglones(t, tui.Margin+"    ", tui.Faint, "→ killport localhost:"+p+" o 127.0.0.1:"+p+" apuntan solo al que contesta esa dirección")...)
		}
	}
	return out
}

// lineasTapados avisan, antes de confirmar, lo que cerrar no va a liberar:
// un puerto pedido que una negativa sigue ocupando (tapados.go), y que un
// puerto UDP de multidifusión se comparte a propósito.
func lineasTapados(t *tui.Term, plan reaper.Plan) []string {
	var out []string
	ts := tapadosDe(plan)
	if len(ts) > 0 {
		aunque, cerrar := "Aunque los cierre, ", "cerrarlos"
		if len(plan.Targets) == 1 {
			aunque, cerrar = "Aunque lo cierre, ", "cerrarlo"
		}
		detalle := cerrar + " libera el resto"
		if todosTapados(plan, ts) {
			detalle = cerrar + " no libera nada"
		}
		out = append(out, "")
		out = append(out, estadoCon(t, tui.Margin, tui.Warn, aunque+textoTapados(ts), detalle)...)
	}
	visto := map[uint16]bool{}
	var socks []ports.Socket
	for _, tg := range plan.Targets {
		socks = append(socks, tg.Sockets...)
	}
	for _, r := range plan.Refusals {
		socks = append(socks, r.Sockets...)
	}
	for _, s := range socks {
		if m := multidifusion(s.Port(), s.Proto); m != "" && !visto[s.Port()] && plan.Request().Ports.Contains(s.Port()) {
			visto[s.Port()] = true
			out = append(out, renglones(t, tui.Margin+"    ", tui.Faint, "el "+u32(uint32(s.Port()))+" UDP es multidifusión ("+m+"): varios programas lo escuchan a la vez a propósito y cada uno recibe su copia; nadie necesita tenerlo solo")...)
		}
	}
	return out
}

// todosTapados: ningún puerto de los objetivos se libera cerrándolos.
func todosTapados(plan reaper.Plan, ts []tapado) bool {
	if len(ts) == 0 || len(plan.Targets) == 0 {
		return false
	}
	var ps ports.Set
	for _, tg := range plan.Targets {
		ps = ps.Union(tg.Ports)
	}
	return ps.Minus(puertosDeTapados(ts)).IsEmpty()
}

// huerfanosDe dice cuáles de los objetivos quedaron sueltos, y por qué:
// "su padre (PID 900) ya no existe". Con --orphans es el motivo de la
// elección; sin él, igual vale saberlo antes de confirmar.
func huerfanosDe(f *reaper.Forest, plan reaper.Plan, svc map[uint32][]reaper.Service, cfg reaper.Config) map[reaper.ID]reaper.Orphan {
	out := map[reaper.ID]reaper.Orphan{}
	for _, tg := range plan.Targets {
		if tg.Kind != reaper.KillProcess {
			continue // un servicio no tiene padre que lo cuide; un fantasma ya dice que su dueño murió
		}
		if p, ok := f.Lookup(tg.Root); ok {
			if o, ok := reaper.OrphanOf(f, p, svc, cfg); ok {
				out[tg.Root.Canon()] = o
			}
		}
	}
	return out
}

func bloqueObjetivo(t *tui.Term, sys *sistema, forest *reaper.Forest, tg reaper.Target, cfg reaper.Config, det detalle, now time.Time, idx int) []string {
	dot := tui.Accent(idx)
	titulo := t.Paint(dot, "●") + " " + t.Paint(tui.Text, nombreVisible(tg.Name, tg.Root.PID)) +
		t.Paint(tui.Faint, "  PID ") + t.Paint(tui.Subtle, u32(tg.Root.PID))
	if !tg.Root.Created.IsZero() {
		titulo += t.Paint(tui.Faint, " · ") + t.Paint(tui.Subtle, tui.Ago(tg.Root.Created, now))
	}
	out := []string{"", tui.Margin + titulo}
	linea := func(c tui.Color, s string) { out = append(out, renglones(t, tui.Margin+"    ", c, s)...) }

	for _, ln := range socketsResumidos(tg.Sockets, nombreIface(sys)) {
		out = append(out, tui.Margin+"    "+t.Paint(tui.Lavender, ln.puerto)+t.Paint(tui.Faint, " "+ln.proto)+t.Paint(tui.Subtle, ln.extra))
	}
	// Línea de comandos (legible: el programa sin ruta, las rutas acortadas),
	// o la ruta del ejecutable si no se pudo leer.
	ruta := ""
	if p, ok := forest.Lookup(tg.Root); ok {
		ruta = p.Path
	}
	if d, err := sys.detalle(tg.Root); err == nil {
		comando, _ := comandoLegible(unaLinea(d.CommandLine), rutasDe{cwd: d.Cwd, home: perfil()})
		if comando == "" {
			comando = ruta
		}
		if comando != "" {
			out = append(out, lineasComando(t, comando, unaLinea(d.CommandLine))...)
		}
		if d.User != "" && !strings.EqualFold(d.User, usuarioPropio()) {
			linea(tui.Faint, "usuario: "+d.User)
		}
	}
	if p, ok := det.proyectos[tg.Root.Canon()]; ok {
		linea(tui.Sky, "proyecto: "+p.largo())
	}
	if h, ok := det.huerfanos[tg.Root.Canon()]; ok {
		txt := "quedó suelto: " + h.Why
		if _, proy := det.proyectos[tg.Root.Canon()]; !h.Dev && !proy {
			// Lo que suele quedar así es una app de la bandeja cuando se
			// reinició el Explorador: se dice, para que no se cierre de más.
			txt += " (no parece de desarrollo)"
		}
		linea(tui.Peach, txt)
	}
	// Cadena de padres, si aporta.
	if anc := forest.Ancestors(tg.Root); len(anc) > 0 {
		cadena := ""
		for i := len(anc) - 1; i >= 0; i-- {
			cadena += nombreVisible(anc[i].Name, anc[i].PID) + " → "
		}
		cadena += nombreVisible(tg.Name, tg.Root.PID)
		linea(tui.Faint, "cadena: "+cadena)
	}
	if tg.Impostor != "" {
		// Lleva el nombre de un proceso de Windows y no lo es: se cierra como
		// lo que es, un programa cualquiera, y se dice por qué se lo trata así.
		linea(tui.Peach, "posible impostor: "+tg.Impostor)
	}
	// Detrás de http.sys: el socket es del kernel, el que se cierra es este.
	if !tg.Via.Zero() {
		for _, l := range lineasURL(t, det.urls[tg.Root.Canon()]) {
			out = append(out, tui.Margin+"    "+l)
		}
		linea(tui.Cream, "Windows lo ve como "+nombreVisible(tg.ViaName, tg.Via.PID)+" (PID "+u32(tg.Via.PID)+"): es "+tg.ViaClass.String()+", no se toca")
	}
	if cfg.Tree && len(tg.Victims) > 1 {
		// Qué incluye, con nombre y PID: es una acción destructiva sobre
		// procesos que el usuario no nombró.
		var hijos []string
		for _, v := range tg.Victims[1:] {
			p, _ := forest.Lookup(v)
			hijos = append(hijos, quienEs(p.Name, v.PID))
		}
		if len(hijos) > 6 {
			hijos = append(hijos[:6], tui.Count(int64(len(hijos)-6), "más", "más"))
		}
		linea(tui.Peach, "--tree: cierra también "+unirY(hijos))
	}
	if c, ok := det.contextos[tg.Root.Canon()]; ok {
		if len(c.tambien) > 0 {
			var otros []string
			for _, a := range c.tambien {
				otros = append(otros, quienEs(a.Name, a.PID)+", "+parentesco(forest, tg.Root, a))
			}
			linea(tui.Peach, "el Ctrl+C le llega también a "+strings.Join(otros, "; ")+": comparte la consola")
		}
		if s := c.lineaSinCtrlC(); s != "" {
			linea(tui.Peach, s)
		}
		if c.otraSesion {
			linea(tui.Peach, "es de otra sesión (otro usuario, o un servicio): cerrarlo pide administrador")
		}
		if c.servicio != "" {
			linea(tui.Cream, "lo lanzó el servicio "+c.servicio+" ("+quienEs(c.servProc.Name, c.servProc.PID)+"): puede volver a lanzarlo; si vuelve, `net stop "+c.servicio+"`")
		}
	}
	if tg.Reason != "" {
		linea(tui.Cream, tg.Reason)
	}
	if tg.Kind == reaper.StopThirdSvc {
		for _, l := range det.lineasServicio(t, tg.Service, tg.Ports) {
			out = append(out, tui.Margin+"    "+l)
		}
	}
	if c := lineaConocido(tg.Service, tg.Name, tg.Ports); c != "" {
		linea(tui.Cream, c)
	}
	if tg.Caution != "" {
		linea(tui.Peach, "ojo: "+tg.Caution)
	}
	return out
}

// usuarioPropio es la cuenta con la que corre killport ("EQUIPO\usuario"),
// leída una vez de su propio token; "" si no se pudo. En el bloque de un
// objetivo, la línea "usuario:" sale solo si el dueño es OTRO (un servicio,
// otra cuenta): que un proceso tuyo es tuyo no dice nada, y repetía el
// nombre del equipo y el de la cuenta en cada bloque (en una captura para
// compartir, en un issue).
var usuarioPropio = sync.OnceValue(func() string {
	d, err := win.QueryDetailWith(uint32(os.Getpid()), time.Time{}, nil)
	if err != nil {
		return ""
	}
	return d.User
})

// lineasComando son los renglones de la línea de comandos en el bloque de un
// objetivo: es lo que identifica al proceso antes de contestar "¿Cerrar?", así
// que no se recorta por el medio (se comía el script). Entera si entra; si
// no, partida en hasta tres renglones; si ni así, la forma corta (cada ruta,
// su último elemento: "node srv.js … m-A.txt"), y recién ahí recortada
// respetando el programa y el script. Sin consola, entera.
func lineasComando(t *tui.Term, legible, cruda string) []string {
	const tope = 3
	prefijo := tui.Margin + "    "
	ancho := t.Width() - tui.Width(prefijo)
	if !t.Bounded() || tui.Width(legible) <= ancho {
		return []string{prefijo + t.Paint(tui.Faint, legible)}
	}
	if len(envolver(legible, ancho)) <= tope {
		return renglones(t, prefijo, tui.Faint, legible)
	}
	corto, keep := comandoCompacto(cruda)
	if corto == "" {
		corto, keep = legible, 0
	}
	if len(envolver(corto, ancho)) <= tope {
		return renglones(t, prefijo, tui.Faint, corto)
	}
	return []string{prefijo + t.Paint(tui.Faint, tui.TruncateMiddleKeep(corto, keep, ancho))}
}

// bloqueExterno es el bloque de un objetivo que no es un proceso de Windows.
// Lo propio de cada dominio lo dice el externo mismo (un contenedor: imagen,
// compose, reinicio y todos los puertos que publica, porque se detiene
// ENTERO; un proceso Linux: distro, usuario y línea de comandos); acá van sus
// sockets del lado de Windows, quién lo sirve —y por qué ese no se toca— y la
// advertencia.
func bloqueExterno(t *tui.Term, tg reaper.Target, now time.Time, idx int) []string {
	x := tg.External
	titulo, lineas := t.Paint(tui.Text, nombreExterno(x)), []string(nil)
	if ex, ok := x.Ref.(externo); ok {
		titulo, lineas = ex.presentar(t, tg, now)
	}
	out := []string{"", tui.Margin + t.Paint(tui.Accent(idx), "●") + " " + titulo}
	for _, ln := range socketsResumidos(tg.Sockets, nil) {
		out = append(out, tui.Margin+"    "+t.Paint(tui.Lavender, ln.puerto)+t.Paint(tui.Faint, " "+ln.proto)+t.Paint(tui.Subtle, ln.extra))
	}
	for _, l := range lineas {
		out = append(out, tui.Margin+"    "+l)
	}
	if tg.ViaName != "" {
		que := tg.ViaClass.String()
		if tg.ViaClass == reaper.RelayContainer {
			que = x.Where
		}
		out = append(out, tui.Margin+"    "+t.Paint(tui.Cream, "Windows lo ve como "+tg.ViaName+" (PID "+u32(tg.Via.PID)+"): es "+que+", no se toca"))
	}
	if tg.Caution != "" {
		out = append(out, tui.Margin+"    "+t.Paint(tui.Peach, "ojo: "+tg.Caution))
	}
	return out
}

// lineaSocket es una fila de sockets ya resumida para el bloque.
type lineaSocket struct{ puerto, proto, extra string }

// socketsResumidos fusiona los sockets de un objetivo por (puerto, protocolo,
// estado) y dice DÓNDE: "5173 TCP escuchando en [::1] · esta PC (v6)", "5432
// TCP escuchando en ∗ v4+v6 · toda la red". Un dev server en 0.0.0.0 y [::]
// es una sola línea. nombre da el nombre de la interfaz de una dirección
// concreta (nil: se muestra la dirección). Mantiene el orden de aparición.
// O(sockets).
func socketsResumidos(socks []ports.Socket, nombre func(netip.Addr) string) []lineaSocket {
	type clave struct {
		port  uint16
		proto ports.Proto
		state ports.State
	}
	var orden []clave
	por := map[clave][]ports.Socket{}
	for _, s := range socks {
		st := s.State
		if s.Proto == ports.UDP {
			st = ports.StateNone
		}
		k := clave{s.Port(), s.Proto, st}
		if _, ok := por[k]; !ok {
			orden = append(orden, k)
		}
		por[k] = append(por[k], s)
	}
	out := make([]lineaSocket, 0, len(orden))
	for _, k := range orden {
		extra := ""
		if k.proto == ports.TCP {
			extra = " " + k.state.Label()
		}
		dirs := direccionesDe(por[k])
		extra += " en " + textoDirecciones(dirs)
		if k.proto == ports.UDP || k.state == ports.Listen {
			extra += " · " + alcanceDe(dirs, nombre).texto
		}
		out = append(out, lineaSocket{puerto: u32(uint32(k.port)), proto: k.proto.String(), extra: extra})
	}
	return out
}

func bloqueNegativa(t *tui.Term, r reaper.Refusal, det detalle) []string {
	titulo := t.Paint(tui.Faint, "●") + " " + t.Paint(tui.Subtle, r.Ports.String())
	if r.Name != "" {
		titulo += t.Paint(tui.Faint, " — ") + t.Paint(tui.Subtle, nombreVisible(r.Name, r.PID))
	}
	if !r.Via.Zero() {
		// El que figura en la tabla es el intermediario; el negado es el que
		// está detrás.
		titulo += t.Paint(tui.Faint, "  PID "+u32(r.PID)+" · a través de "+nombreVisible(r.ViaName, r.Via.PID))
	}
	out := []string{"", tui.Margin + titulo}
	linea := func(c tui.Color, s string) { out = append(out, renglones(t, tui.Margin+"    ", c, s)...) }
	for _, l := range lineasURL(t, det.urls[r.Owner.Canon()]) {
		out = append(out, tui.Margin+"    "+l)
	}
	if r.Impostor != "" {
		linea(tui.Peach, "posible impostor: "+r.Impostor)
	}
	if r.Reason != "" {
		linea(tui.Cream, "no se toca: "+r.Reason)
	}
	if r.Advice != "" {
		linea(tui.Faint, "→ "+r.Advice)
	}
	// De un servicio de Windows se dice cómo arranca; qué hacer con eso lo
	// dice, si lo conoce, la tabla de sospechosos de siempre. killport no le
	// cambia el inicio a un servicio de Windows ni lo aconseja por su cuenta.
	if cfg, ok := det.inicioDe(r.Service); ok && r.Service != "" {
		linea(tui.Faint, lineaServicio(r.Service, cfg))
	}
	if c := lineaConocido(r.Service, r.Name, r.Ports); c != "" {
		linea(tui.Cream, c)
	}
	return out
}

// --- escalada en vivo ------------------------------------------------------

type progreso struct {
	mu        sync.Mutex
	esperando bool
	verifica  bool
	deadline  time.Time
	metodo    reaper.Method
}

// escalar corre el motor narrando cada peldaño: una línea viva con el spinner y
// la cuenta regresiva, y líneas permanentes por cada novedad.
func escalar(t *tui.Term, ctx contextoT, sys *sistema, forest *reaper.Forest, requested ports.Set, plan reaper.Plan, cfg reaper.Config, opts reaper.Options) reaper.Report {
	pr := &progreso{}
	live := t.StartLive(func(width int) []string {
		pr.mu.Lock()
		defer pr.mu.Unlock()
		sp := tui.Spinner(time.Now())
		switch {
		case pr.verifica:
			return []string{tui.Margin + t.Paint(tui.Sky, sp+" verificando…")}
		case pr.esperando:
			restante := time.Until(pr.deadline)
			if restante < 0 {
				restante = 0
			}
			etiqueta := "esperando el cierre prolijo"
			switch pr.metodo {
			case reaper.MethodTerminate:
				etiqueta = "terminando"
			case reaper.MethodEngine:
				etiqueta = "deteniendo (después, en seco)"
			}
			return []string{tui.Margin + t.Paint(tui.Lavender, sp+" "+etiqueta+" ") + t.Paint(tui.Subtle, tui.Duration(restante.Round(100*time.Millisecond)))}
		default:
			return []string{tui.Margin + t.Paint(tui.Lavender, sp+" cerrando…")}
		}
	})

	como := nuevaNarracion(plan, forest, cfg.Force, sys.esAdmin(), opts.Timeout)
	opts.Emit = func(e reaper.Event) {
		if l := lineaEvento(t, e, como); l != "" {
			live.Println(l)
		}
		pr.mu.Lock()
		switch e.Kind {
		case reaper.EvWaiting:
			pr.esperando, pr.deadline, pr.metodo, pr.verifica = true, e.Deadline, e.Method, false
		case reaper.EvVerifying:
			pr.esperando, pr.verifica = false, true
		}
		pr.mu.Unlock()
	}

	rep := reaper.Run(ctx, sys, forest, requested, plan, cfg, opts)
	live.Stop(false)
	return rep
}

// narracion es lo que hay que saber de la corrida para contar bien un evento.
type narracion struct {
	// enSeco: la corrida es con --force, así que lo que se terminó en seco no
	// es porque "no cedió a tiempo": nunca se le pidió por las buenas.
	enSeco bool
	// admin: killport corre como administrador. Si no, un acceso denegado no es
	// una falla sino el paso previo al pedido de administrador; si sí, es el
	// final del camino (un servicio protegido) y hay que decirlo así.
	admin bool
	// timeout: el plazo del cierre prolijo ("no cedió al Ctrl+C en 3 s").
	timeout time.Duration
	// repetidos: nombres que llevan más de una víctima (--tree: tres
	// node.exe); sus líneas llevan el PID, o no se sabe cuál cerró.
	repetidos map[string]bool
	// pedidos: qué se le pidió a cada víctima antes de terminarla (Ctrl+C,
	// cierre de ventana) o por qué no ("omitido: …"). Lo llena la narración
	// misma, evento a evento; nil = no se lleva la cuenta.
	pedidos map[reaper.ID]string
}

// nuevaNarracion arma la narración de una corrida.
func nuevaNarracion(plan reaper.Plan, forest *reaper.Forest, enSeco, admin bool, timeout time.Duration) narracion {
	n := narracion{enSeco: enSeco, admin: admin, timeout: timeout, repetidos: map[string]bool{}, pedidos: map[reaper.ID]string{}}
	visto := map[string]bool{}
	for _, id := range plan.Victims() {
		if p, ok := forest.Lookup(id); ok {
			k := strings.ToLower(p.Name)
			n.repetidos[k] = visto[k]
			visto[k] = true
		}
	}
	for k := range visto {
		if !n.repetidos[k] {
			delete(n.repetidos, k)
		}
	}
	return n
}

// porQueEnSeco dice por qué una víctima se terminó en seco: nunca se le pidió
// por las buenas (--force), no cedió a lo que se le pidió en el plazo, o no
// se le pudo pedir.
func (n narracion) porQueEnSeco(id reaper.ID) string {
	switch p := n.pedidos[id.Canon()]; {
	case n.enSeco:
		return "--force"
	case strings.HasPrefix(p, "omitido: "):
		return "sin Ctrl+C: " + strings.TrimPrefix(p, "omitido: ")
	case p != "":
		return "no cedió al " + p + " en " + textoPlazo(n.timeout)
	case n.pedidos != nil:
		return "no tiene consola ni ventana a la que pedírselo"
	}
	return ""
}

// tiempoCorto es cuánto tardó algo en cerrar: "<1 ms" debajo del
// milisegundo (la hora de salida del kernel y la de la señal no dan para
// más; "0,00 ms" prometía una precisión que no hay).
func tiempoCorto(d time.Duration) string {
	if d < time.Millisecond {
		return "<1 ms"
	}
	return tui.Duration(d)
}

// lineaEvento traduce un evento del motor a una línea permanente (o "" si no
// merece una: los cambios de fase solo mueven la región viva).
func lineaEvento(t *tui.Term, e reaper.Event, como narracion) string {
	n := func(name string, pid uint32) string {
		if como.repetidos[strings.ToLower(name)] {
			return quienEs(name, pid)
		}
		return nombreVisible(name, pid)
	}
	anota := func(id reaper.ID, que string) {
		if como.pedidos != nil {
			como.pedidos[id.Canon()] = que
		}
	}
	switch e.Kind {
	case reaper.EvServiceStopping:
		return t.Status(tui.Step, "Deteniendo el servicio "+e.Name, "")
	case reaper.EvServiceStopped:
		return t.Status(tui.OK, "Servicio "+e.Name+" detenido", "")
	case reaper.EvServiceFailed:
		switch {
		case !errors.Is(e.Err, reaper.ErrServiceAccessDenied):
		case como.admin:
			return t.Status(tui.Fail, "No pude detener "+e.Name, "acceso denegado aun como administrador: es un servicio protegido")
		default:
			return t.Status(tui.Skip, "Detener el servicio "+e.Name+" pide administrador", "")
		}
		return t.Status(tui.Fail, "No pude detener "+e.Name, e.Detail)
	case reaper.EvCtrlC:
		anota(e.Victim, "Ctrl+C")
		for _, m := range e.Members {
			anota(m, "Ctrl+C")
		}
		detalle := ""
		if len(e.Also) > 0 {
			// Comparten la consola: apretar Ctrl+C en esa ventana haría lo
			// mismo, pero no se lo pidió el usuario por su nombre.
			var otros []string
			for _, a := range e.Also {
				otros = append(otros, quienEs(a.Name, a.PID))
			}
			detalle = "le llega también a " + unirY(otros) + ", que comparte la consola"
		}
		return t.Status(tui.Step, "Ctrl+C a "+n(e.Name, e.Victim.PID), detalle)
	case reaper.EvCtrlCSkipped:
		anota(e.Victim, "omitido: "+e.Detail)
		return t.Status(tui.Skip, "Ctrl+C omitido", e.Detail)
	case reaper.EvClose:
		if como.pedidos != nil && como.pedidos[e.Victim.Canon()] == "" {
			anota(e.Victim, "cierre de ventana")
		}
		return t.Status(tui.Step, "Cierre de ventana a "+n(e.Name, e.Victim.PID), "")
	case reaper.EvExited:
		if e.Method == reaper.MethodTerminate {
			return "" // ya lo dijo EvTerminated, con el porqué
		}
		return t.Status(tui.OK, n(e.Name, e.Victim.PID)+" cerró", tiempoCorto(e.Elapsed)+" · "+e.Method.String())
	case reaper.EvTerminated:
		return t.Status(tui.Warn, n(e.Name, e.Victim.PID)+" terminado en seco", como.porQueEnSeco(e.Victim))
	case reaper.EvTerminateFailed:
		return t.Status(tui.Fail, "No pude terminar "+n(e.Name, e.Victim.PID), e.Detail)
	case reaper.EvCanceled:
		return t.Status(tui.Skip, "Cancelaste", "dejo todo como está")
	case reaper.EvOpenFailed:
		// Lo que no se llega a abrir: se dice por qué no se lo toca. Un
		// acceso denegado no (lo que sigue es el pedido de administrador).
		switch {
		case errors.Is(e.Err, reaper.ErrGone):
			return t.Status(tui.Skip, n(e.Name, e.Victim.PID)+" ya no estaba", "")
		case errors.Is(e.Err, reaper.ErrAccessDenied):
			return ""
		}
		return t.Status(tui.Skip, n(e.Name, e.Victim.PID)+" no se toca", e.Detail)
	case reaper.EvExternalStopping:
		texto, detalle := dominioDe(e.External.Kind).pedido(nombreExterno(e.External), e.Method == reaper.MethodTerminate)
		return t.Status(tui.Step, texto, detalle)
	case reaper.EvExternalStopped:
		switch e.Outcome {
		case reaper.OutcomeGone:
			return t.Status(tui.Skip, nombreExterno(e.External)+" ya no estaba", "")
		case reaper.OutcomeTerminated:
			detalle := dominioDe(e.External.Kind).forzado
			if !como.enSeco {
				detalle = strings.TrimSuffix("no cedió a tiempo · "+detalle, " · ")
			}
			return t.Status(tui.Warn, nombreExterno(e.External)+" terminado en seco", detalle)
		}
		return t.Status(tui.OK, nombreExterno(e.External)+" "+dominioDe(e.External.Kind).hecho, tui.Duration(e.Elapsed)+" · "+e.Method.String())
	case reaper.EvExternalFailed:
		if e.Outcome == reaper.OutcomeDenied && pideAdmin(e.External) && !como.admin {
			// No es una falla: es lo que se sabía de antemano (la pregunta lo
			// avisó). Lo que sigue es el pedido de administrador.
			return t.Status(tui.Skip, dominioDe(e.External.Kind).verbo+" "+nombreExterno(e.External)+" pide administrador", "")
		}
		return t.Status(tui.Fail, "No pude "+strings.ToLower(dominioDe(e.External.Kind).verbo)+" "+nombreExterno(e.External), e.Detail)
	}
	return ""
}

// --- después de la escalada ------------------------------------------------

// aplicarManual es el paso de --manual: a cada servicio de terceros del plan
// que la escalada DETUVO se lo deja en inicio manual. A los que no se pudieron
// detener no se les toca la configuración; si fue por falta de permiso, el
// resultado lo dice y el servicio viaja en el trabajo elevado (Elevable), que
// lo detiene y recién entonces le cambia el inicio.
func aplicarManual(sys *sistema, plan reaper.Plan, rep reaper.Report, cfg reaper.Config) []resultadoManual {
	fin := map[reaper.ID]reaper.Outcome{}
	for _, v := range rep.Victims {
		fin[v.ID.Canon()] = v.Outcome
	}
	var out []resultadoManual
	visto := map[string]bool{}
	for _, tg := range plan.Targets {
		if tg.Kind != reaper.StopThirdSvc || visto[strings.ToLower(tg.Service)] {
			continue
		}
		visto[strings.ToLower(tg.Service)] = true
		antes, _ := sys.svc.de(tg.Service)
		switch fin[tg.Root.Canon()] {
		case reaper.OutcomeServiceStopped:
			out = append(out, sys.svc.dejarEnManual(tg.Service, cfg.SystemRoot))
		case reaper.OutcomeDenied:
			out = append(out, resultadoManual{Servicio: tg.Service, Fin: manualDenegado, Antes: antes, Elevable: true,
				Motivo: "detenerlo y cambiarle el inicio pide administrador"})
		default:
			out = append(out, resultadoManual{Servicio: tg.Service, Fin: manualSigueVivo, Antes: antes,
				Motivo: "no se pudo detener: no le cambio el inicio a un servicio que sigue corriendo"})
		}
	}
	return out
}

// lineasManual cuenta cómo le fue a --manual.
func lineasManual(t *tui.Term, rs []resultadoManual, o opciones, plan reaper.Plan) []string {
	var out []string
	if o.manual && len(rs) == 0 && len(plan.Targets) > 0 {
		out = append(out, t.Status(tui.Info, "--manual no hizo nada", "ninguno de los objetivos es un servicio que killport pueda dejar en inicio manual"))
	}
	for _, r := range rs {
		switch r.Fin {
		case manualHecho:
			out = append(out, t.Status(tui.OK, "El servicio "+r.Servicio+" quedó en inicio manual", "se deshace con `"+r.Deshacer+"`"))
		case manualYaEstaba, manualNoSeToca:
			out = append(out, t.Status(tui.Skip, "No le cambié el inicio a "+r.Servicio, r.Motivo))
		default:
			out = append(out, t.Status(tui.Fail, "No pude dejar "+r.Servicio+" en inicio manual", r.Motivo))
		}
		if r.Aviso != "" {
			out = append(out, "    "+t.Paint(tui.Peach, "ojo: "+r.Aviso))
		}
	}
	return out
}

// lineasVuelve avisa de los servicios que se detuvieron y van a volver solos:
// la pelea por el puerto se repite en el próximo arranque, y se evita con
// --manual. No dice nada de los que ya quedaron en manual.
func lineasVuelve(t *tui.Term, plan reaper.Plan, rep reaper.Report, manuales []resultadoManual, det detalle) []string {
	enManual := map[string]bool{}
	for _, m := range manuales {
		enManual[strings.ToLower(m.Servicio)] = m.Fin == manualHecho
	}
	detenidos := map[string]bool{}
	for _, s := range serviciosDetenidos(plan, rep) {
		detenidos[strings.ToLower(s)] = true
	}
	var out []string
	for _, tg := range plan.Targets {
		k := strings.ToLower(tg.Service)
		if tg.Kind != reaper.StopThirdSvc || !detenidos[k] || enManual[k] {
			continue
		}
		cfg, ok := det.inicioDe(tg.Service)
		if !ok {
			continue
		}
		if vuelve, porque := vuelveSolo(cfg); vuelve {
			consejo := "para que no vuelva: killport " + specDe(tg.Ports) + " --manual"
			if cfg.Start == win.StartDemand {
				consejo = "`sc qtriggerinfo " + citarSC(tg.Service) + "` muestra qué lo arranca"
			}
			out = append(out, t.Status(tui.Warn, "El servicio "+tg.Service+" va a volver", porque+"; "+consejo))
		}
	}
	return out
}

// --- tarjeta final ---------------------------------------------------------

// supervisorDe es quien relanzó el servidor de algún puerto (el LCA que
// encontró la verificación), si no es el administrador de servicios (para
// eso está detener el servicio) y sigue vivo en la foto de después.
func supervisorDe(rep reaper.Report) (reaper.ID, string, []uint16) {
	var sup reaper.ID
	nombre := ""
	var puertos []uint16
	for _, p := range rep.Ports {
		if p.Status != reaper.PortRespawned || p.Supervisor.Zero() || strings.EqualFold(p.SupervisorName, "services.exe") {
			continue
		}
		if sup.Zero() {
			sup, nombre = p.Supervisor, p.SupervisorName
		}
		if p.Supervisor == sup {
			puertos = append(puertos, p.Port)
		}
	}
	return sup, nombre, puertos
}

// ofrecerSupervisor: el puerto lo retomó un proceso que relanzó un supervisor
// (nodemon, pm2, un script) que no escucha nada, así que ningún selector lo
// alcanza. En consola se ofrece cerrarlo ahí, con sus hijos; sin consola o con
// --yes (que no autoriza a cerrar a alguien que no se nombró), se da el
// comando. Devuelve el reporte con lo que se hizo sumado.
func ofrecerSupervisor(t *tui.Term, ctx contextoT, sys *sistema, requested ports.Set, svc map[uint32][]reaper.Service,
	cfg reaper.Config, opts reaper.Options, o opciones, rep reaper.Report) reaper.Report {
	sup, nombre, puertos := supervisorDe(rep)
	if sup.Zero() || rep.Canceled {
		return rep
	}
	quien := quienEs(nombre, sup.PID)
	if o.yes || !t.CanPrompt() {
		t.Lines(lineasRemedios(t, []reaper.Remedy{{Steps: []string{"taskkill /PID " + u32(sup.PID) + " /T /F"},
			Effect: "cierra a " + quien + " con sus hijos, en seco (en una consola, killport lo ofrece y lo cierra prolijo)"}}))
		return rep
	}
	resp, ok := t.Prompt(t.Paint(tui.Lavender, "¿Cerrar también a "+quien+", que lo relanza, con sus hijos?") + t.Paint(tui.Subtle, " [s/N] "))
	if !ok || !esSi(resp) {
		return rep
	}
	t.Blank()
	procs, err := sys.Snapshot(ctx)
	if err != nil {
		t.Line(t.Status(tui.Fail, "No pude leer la lista de procesos", err.Error()))
		return rep
	}
	ahora := reaper.NewForest(procs)
	cfg.Tree = true
	libres := ports.Of(puertos...)
	plan := reaper.PlanProcess(sup, libres, ahora, svc, cfg)
	plan = reaper.Focus(plan, ports.Whole(libres), nil)
	for _, r := range plan.Refusals {
		t.Line(t.Status(tui.Skip, "No cierro a "+quien, r.Reason))
	}
	if len(plan.Targets) == 0 {
		return rep
	}
	otro := escalar(t, ctx, sys, ahora, libres, plan, cfg, opts)
	t.Lines(lineasPuertosAlt(t, otro, plan, nil))
	// Lo de antes más lo de ahora; los puertos, como quedaron al final.
	rep.Victims = append(rep.Victims, otro.Victims...)
	rep.Reached = append(rep.Reached, otro.Reached...)
	rep.Canceled = rep.Canceled || otro.Canceled
	final := map[uint16]reaper.PortResult{}
	for _, p := range otro.Ports {
		final[p.Port] = p
	}
	for i, p := range rep.Ports {
		if f, ok := final[p.Port]; ok {
			rep.Ports[i] = f
		}
	}
	return rep
}

// alcanzadosQueCerraron son los que recibieron el Ctrl+C sin ser víctimas
// (compartían la consola: el npm o el supervisor que lanzó al servidor) y ya
// no están. Una foto nueva de los procesos, por identidad.
func alcanzadosQueCerraron(ctx contextoT, sys *sistema, reached []reaper.Proc) []reaper.Proc {
	if len(reached) == 0 {
		return nil
	}
	procs, err := sys.Snapshot(ctx)
	if err != nil {
		return nil
	}
	ahora := reaper.NewForest(procs)
	var out []reaper.Proc
	for _, p := range reached {
		if _, vive := ahora.Lookup(p.ID); !vive {
			out = append(out, p)
		}
	}
	return out
}

// lineasAlcanzados cuentan a los que cerraron sin ser víctimas: se cerraron
// dos y se pidió uno, y eso se dice.
func lineasAlcanzados(t *tui.Term, ps []reaper.Proc) []string {
	var out []string
	for _, p := range ps {
		out = append(out, t.Status(tui.Warn, "También cerró "+quienEs(p.Name, p.PID), "le llegó el Ctrl+C: compartía la consola"))
	}
	return out
}

// tarjeta es la tarjeta de resumen. tambien: cuántos cerraron sin ser
// víctimas (compartían la consola y les llegó el Ctrl+C).
func tarjeta(t *tui.Term, rep reaper.Report, manuales []resultadoManual, tambien int) []string {
	free, total := rep.FreedPorts()
	var solos, seco, servicios, enManual int
	for _, v := range rep.Victims {
		switch v.Outcome {
		case reaper.OutcomeClosed, reaper.OutcomeGone:
			solos++
		case reaper.OutcomeTerminated:
			seco++
		case reaper.OutcomeServiceStopped:
			servicios++
		}
	}
	for _, m := range manuales {
		if m.Fin == manualHecho {
			enManual++
		}
	}
	externos, forzados := itemsExternos(rep.Externals)
	seco += forzados
	dotLibres := tui.Sage
	if free < total {
		dotLibres = tui.Peach
	}
	if free == 0 && total > 0 {
		dotLibres = tui.Rose
	}
	items := []tui.Item{
		{Label: "Puertos libres", Value: u32(uint32(free)) + " de " + u32(uint32(total)), Dot: dotLibres},
	}
	if solos > 0 {
		items = append(items, tui.Item{Label: "Cerraron solos", Value: u32(uint32(solos)), Dot: tui.Teal})
	}
	if tambien > 0 {
		items = append(items, tui.Item{Label: "También cerraron", Value: u32(uint32(tambien)), Dot: tui.Peach})
	}
	if seco > 0 {
		items = append(items, tui.Item{Label: "En seco", Value: u32(uint32(seco)), Dot: tui.Peach})
	}
	if servicios > 0 {
		items = append(items, tui.Item{Label: "Servicios detenidos", Value: u32(uint32(servicios)), Dot: tui.Lavender})
	}
	if enManual > 0 {
		items = append(items, tui.Item{Label: "En inicio manual", Value: u32(uint32(enManual)), Dot: tui.Lavender})
	}
	items = append(items, externos...)
	if rep.Canceled {
		items = append(items, tui.Item{Label: "Estado", Value: "cancelado", Dot: tui.Rose})
	}
	return t.Card("killport", items)
}

// codigoDeSalida traduce el reporte a los códigos de salida (cli.Exit*).
func codigoDeSalida(rep reaper.Report) int {
	if rep.Canceled {
		return cli.ExitInterrupted
	}
	free, total := rep.FreedPorts()
	switch {
	case total == 0 || free == total:
		return cli.ExitOK
	case free == 0:
		return cli.ExitFailure
	default:
		return cli.ExitPartial
	}
}
