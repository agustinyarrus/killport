//go:build windows

package killport

// espera_windows.go: `killport PUERTOS --wait TIEMPO` (hasta que escuchen: el
// reemplazo nativo de wait-on) y `--wait-free TIEMPO` (hasta que se liberen,
// sin cerrar nada). La máquina de estados y el bucle son reaper.WaitPorts;
// acá se arma el pedido, se cuenta en vivo lo que pasa (quién escucha, quién
// se fue, quién volvió) y se da el veredicto con el código de salida: 0 se
// cumplió, 3 venció (o no se va a poder cumplir), 130 cancelaste.

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

// esperaSondeoMax acota la falla rápida: con más puertos que esto (un rango
// enorme) no se sondea cada uno; se espera y listo.
const esperaSondeoMax = 256

// correrEspera es el modo espera.
func correrEspera(t *tui.Term, sys *sistema, pos []string, o opciones) int {
	req, err := ports.ParseRequest(pos)
	if err != nil {
		return falloUsage(t, err)
	}
	ctx, stop := contexto()
	defer stop()

	que, plazo := reaper.WatchListen, o.wait
	if o.waitFreeDado {
		que, plazo = reaper.WatchFree, o.waitFree
	}
	start := time.Now()
	spec := reaper.WatchSpec{For: que, Request: req, Filter: filtroEspera(o, que), Any: o.any, Stable: o.stable}
	if o.nuevos {
		spec.NewSince = start
	}
	if que == reaper.WatchListen && req.Ports.Len() <= esperaSondeoMax {
		spec.Blocked = bloqueados(sys, req.Ports, spec.Filter)
	}

	n := &narrador{t: t, sys: sys, ctx: ctx, spec: spec, start: start, deadline: start.Add(plazo), quien: nuevoNombrador(ctx, sys),
		antes: map[uint16][]ports.Socket{}, estado: map[uint16]reaper.WatchPort{}}
	if o.json {
		rep := reaper.WaitPorts(ctx, sys, spec, n.deadline, nil)
		escribirJSON(t, esperaJSON(spec, plazo, rep, req, n.quien, sys))
		return codigoEspera(rep)
	}

	t.Lines(t.Header("killport", tituloEspera(spec), o.version))
	for _, nombre := range req.Names {
		t.Line(t.Hint(nombre + ": killport no resuelve nombres (podría ser cualquier dirección); espero en todo el puerto"))
	}
	// Cómo se mira (la tabla de Windows, sin conectarse) lo dice la ayuda: acá
	// solo el plazo, para que la línea entre en la consola.
	t.Lines([]string{t.Status(tui.Info, n.queEspero(), "hasta "+textoPlazo(plazo))})
	live := t.StartLive(n.vivo)
	n.live = live
	rep := reaper.WaitPorts(ctx, sys, spec, n.deadline, n.evento)
	live.Stop(false)
	t.Lines(n.final(rep))
	t.Blank()
	return codigoEspera(rep)
}

// filtroEspera: --wait espera a un servidor nuevo, que casi siempre es TCP
// (como wait-on tcp:); --udp lo cambia y los dos flags juntos aceptan
// cualquiera. --wait-free mira lo que ya está, como cerrar: los dos
// protocolos salvo que se diga otra cosa, y --all-states suma las conexiones.
func filtroEspera(o opciones, que reaper.WatchFor) ports.Filter {
	f := o.filtro()
	if que == reaper.WatchListen {
		f.AllStates = false
		if !f.TCP && !f.UDP {
			f.TCP = true
		}
	}
	return f
}

// bloqueados son los puertos que no van a poder escuchar (reaper.Diagnoser.Blocked):
// sondea cada uno —microsegundos— y lee las exclusiones solo si algún bind
// fue negado.
func bloqueados(sys *sistema, ps ports.Set, f ports.Filter) map[uint16]reaper.PortResult {
	d := reaper.NewDiagnoser(sys, f, true)
	out := map[uint16]reaper.PortResult{}
	for p := range ps.All() {
		if r, ok := d.Blocked(p); ok {
			out[p] = r
		}
	}
	return out
}

// codigoEspera traduce el final de la espera a los códigos de salida (cli.Exit*).
func codigoEspera(rep reaper.WatchReport) int {
	switch rep.Outcome {
	case reaper.WatchDone:
		return cli.ExitOK
	case reaper.WatchCanceled:
		return cli.ExitInterrupted
	}
	return cli.ExitFailure
}

// tituloEspera es la cabecera: "esperar a que escuche el 5173".
func tituloEspera(spec reaper.WatchSpec) string {
	if spec.For == reaper.WatchFree {
		return "esperar a que se libere"
	}
	return "esperar a que escuche"
}

// etiquetasPuertos nombra los puertos pedidos como los pidió el usuario:
// "localhost:5173", "el 3000 y el 8080".
func etiquetasPuertos(req ports.Request, conector string) string {
	var out []string
	for p := range req.Ports.All() {
		if len(req.Dests(p)) > 0 {
			out = append(out, req.Label(p))
			continue
		}
		out = append(out, "el "+u32(uint32(p)))
	}
	if len(out) > 6 {
		return req.Ports.String()
	}
	if conector == "o" && len(out) > 1 {
		return strings.Join(out[:len(out)-1], ", ") + " o " + out[len(out)-1]
	}
	return unirY(out)
}

// narrador cuenta la espera en vivo. Los eventos llegan desde el bucle (la
// goroutine principal) y la región viva se redibuja desde la suya: lo que
// comparten va bajo el candado.
type narrador struct {
	t               *tui.Term
	sys             *sistema
	ctx             context.Context
	spec            reaper.WatchSpec
	start, deadline time.Time
	quien           *nombrador
	live            *tui.Live

	mu     sync.Mutex
	estado map[uint16]reaper.WatchPort
	antes  map[uint16][]ports.Socket // el último testigo de cada puerto (para decir quién se fue)
}

// queEspero es la primera línea: qué se espera.
func (n *narrador) queEspero() string {
	req := n.spec.Request
	conector := "y"
	if n.spec.Any {
		conector = "o"
	}
	lo := etiquetasPuertos(req, conector)
	if n.spec.For == reaper.WatchFree {
		if req.Ports.Len() > 1 && !n.spec.Any {
			return "Espero a que se liberen " + lo + ", sin cerrar nada"
		}
		return "Espero a que se libere " + lo + ", sin cerrar nada"
	}
	s := "Espero a que escuche " + lo
	if req.Ports.Len() > 1 && !n.spec.Any {
		s = "Espero a que escuchen " + lo + " a la vez"
	}
	if !n.spec.NewSince.IsZero() {
		s += " (algo nuevo)"
	}
	return s
}

// vivo es la región que se redibuja: el spinner, lo que falta y cuánto queda.
func (n *narrador) vivo(int) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := n.t
	restante := time.Until(n.deadline)
	if restante < 0 {
		restante = 0
	}
	texto := "esperando"
	for _, p := range n.estado {
		if p.State == reaper.WatchSettling {
			falta := n.spec.Stable - time.Since(p.Since)
			if falta < 0 {
				falta = 0
			}
			texto = u32(uint32(p.Port)) + " se sostiene · falta " + tui.Duration(falta.Round(10*time.Millisecond))
			break
		}
	}
	return []string{tui.Margin + t.Paint(tui.Lavender, tui.Spinner(time.Now())+" "+texto+" ") + t.Paint(tui.Subtle, "· quedan "+tui.Duration(restante.Round(100*time.Millisecond)))}
}

// evento cuenta un cambio de un puerto.
func (n *narrador) evento(e reaper.WatchEvent) {
	n.mu.Lock()
	prevTestigo := n.antes[e.Port.Port]
	n.estado[e.Port.Port] = e.Port
	if len(e.Port.Witness) > 0 {
		n.antes[e.Port.Port] = e.Port.Witness
	}
	n.mu.Unlock()
	for _, l := range n.lineasEvento(e, prevTestigo) {
		n.live.Println(l)
	}
}

// lineasEvento traduce un cambio a renglones permanentes.
func (n *narrador) lineasEvento(e reaper.WatchEvent, prev []ports.Socket) []string {
	t, p := n.t, e.Port
	puerto := n.spec.Request.Label(p.Port)
	if len(n.spec.Request.Dests(p.Port)) == 0 {
		puerto = u32(uint32(p.Port))
	}
	a := func(at time.Time) string { return "a los " + tui.Duration(at.Sub(n.start)) }
	if p.State == reaper.WatchBlocked {
		return append([]string{t.Status(tui.Fail, puerto+" no va a poder escuchar", p.Blocked.Reason)}, lineasRemedios(t, p.Blocked.Remedies)...)
	}
	if n.spec.For == reaper.WatchFree {
		switch {
		case e.Initial && p.State == reaper.WatchMet:
			return []string{t.Status(tui.OK, puerto+" ya estaba libre", "")}
		case e.Initial && p.State == reaper.WatchPending:
			return []string{t.Status(tui.Info, puerto+" lo ocupa "+n.quienes(p.Witness), "")}
		case p.State == reaper.WatchMet:
			return []string{t.Status(tui.OK, puerto+" libre", a(p.MetAt)+n.seFue(prev))}
		case p.State == reaper.WatchSettling && e.Prev == reaper.WatchPending:
			return []string{t.Status(tui.Step, puerto+" libre", n.seFue(prev)+"espero que siga libre "+textoPlazo(n.spec.Stable))}
		case p.State == reaper.WatchPending:
			return []string{t.Status(tui.Warn, puerto+" lo volvió a tomar "+n.quienes(p.Witness), "la cuenta vuelve a cero")}
		}
		return nil
	}
	switch {
	case e.Restart:
		return []string{t.Status(tui.Warn, puerto+" cambió de dueño", "ahora "+n.quienesEn(p.Witness)+": se reinició y la cuenta vuelve a cero")}
	case e.Initial && p.State == reaper.WatchMet:
		return []string{t.Status(tui.OK, puerto+" ya escuchaba", n.quienesEn(p.Witness)+desdeHace(p.Witness))}
	case p.State == reaper.WatchMet:
		return []string{t.Status(tui.OK, puerto+" escucha", n.quienesEn(p.Witness)+" · "+a(p.MetAt))}
	case p.State == reaper.WatchSettling && (e.Prev == reaper.WatchPending || e.Initial):
		return []string{t.Status(tui.Step, puerto+" escucha", n.quienesEn(p.Witness)+"; espero que se sostenga "+textoPlazo(n.spec.Stable))}
	case p.State == reaper.WatchPending && !e.Initial:
		return []string{t.Status(tui.Warn, puerto+" dejó de escuchar", n.quienes(prev)+" se fue; la cuenta vuelve a cero")}
	}
	return nil
}

// seFue dice quién soltó el puerto, si se sabe: " · node.exe (PID 812) se fue".
func (n *narrador) seFue(prev []ports.Socket) string {
	if len(prev) == 0 {
		return ""
	}
	return " · " + n.quienes(prev) + " lo soltó"
}

// quienes nombra a los dueños de unos sockets: "node.exe (PID 812 · shop-web)".
func (n *narrador) quienes(ss []ports.Socket) string {
	var out []string
	visto := map[uint32]bool{}
	for _, s := range ss {
		if visto[s.PID] {
			continue
		}
		visto[s.PID] = true
		out = append(out, n.quien.quienSock(s))
	}
	return unirY(out)
}

// quienesEn es quienes más dónde escuchan: "node.exe (PID 812) en [::1]".
func (n *narrador) quienesEn(ss []ports.Socket) string {
	return n.quienes(ss) + " en " + textoDirecciones(direccionesDe(ss))
}

// desdeHace dice desde cuándo escucha el socket más nuevo del testigo.
func desdeHace(ss []ports.Socket) string {
	var mas time.Time
	for _, s := range ss {
		if s.Since.After(mas) {
			mas = s.Since
		}
	}
	if mas.IsZero() {
		return ""
	}
	return " · desde " + tui.Ago(mas, time.Now())
}

// final es el veredicto.
func (n *narrador) final(rep reaper.WatchReport) []string {
	t := n.t
	dur := tui.Duration(rep.End.Sub(rep.Start))
	switch rep.Outcome {
	case reaper.WatchDone:
		if len(rep.Ports) > 1 {
			que := "escuchan"
			if n.spec.For == reaper.WatchFree {
				que = "están libres"
			}
			if n.spec.Any {
				que = "alcanzó con uno"
			}
			return []string{t.Status(tui.OK, "Listo", que+" · "+dur)}
		}
		return nil
	case reaper.WatchCanceled:
		return []string{t.Status(tui.Skip, "Cancelaste", "dejé de esperar")}
	case reaper.WatchImpossible:
		// El porqué ya lo dijo la línea del puerto tapado, con sus salidas.
		return []string{t.Status(tui.Fail, "No espero en vano", "")}
	case reaper.WatchFailed:
		return []string{t.Status(tui.Fail, "No pude seguir esperando", rep.Err.Error())}
	}
	out := []string{t.Status(tui.Fail, "Venció el plazo", "a los "+dur)}
	for _, p := range rep.Ports {
		if p.State == reaper.WatchMet || p.State == reaper.WatchBlocked {
			continue
		}
		out = append(out, "    "+t.Paint(tui.Faint, "→ ")+t.Paint(tui.Subtle, n.porQueNo(p)))
	}
	return out
}

// porQueNo cuenta por qué un puerto no llegó a cumplirse.
func (n *narrador) porQueNo(p reaper.WatchPort) string {
	puerto := "el " + u32(uint32(p.Port))
	if n.spec.For == reaper.WatchFree {
		return puerto + " sigue ocupado por " + n.quienes(p.Witness)
	}
	switch {
	case p.State == reaper.WatchSettling:
		s := puerto + " escucha pero no se sostuvo " + textoPlazo(n.spec.Stable)
		if p.Changes > 0 {
			s += " (cambió de dueño " + tui.Count(int64(p.Changes), "vez", "veces") + ")"
		}
		return s
	case !n.spec.NewSince.IsZero():
		return "nada nuevo escucha " + puerto
	}
	if otros := n.otrosEn(p.Port); otros != "" {
		return otros
	}
	return "nadie escucha " + puerto
}

// otrosEn dice, de un puerto pedido con dirección que nadie contesta, quién
// lo escucha en OTRA dirección: "nadie contesta [::1]:5173; node.exe (PID
// 812) escucha el 5173 en 127.0.0.1". Es la trampa de localhost: decir
// "nadie escucha el 5173" sería falso. "" si no hay dirección pedida o nadie
// escucha el puerto. Lee la tabla liviana una vez más (es el final).
func (n *narrador) otrosEn(port uint16) string {
	ds := n.spec.Request.Dests(port)
	if len(ds) == 0 {
		return ""
	}
	socks, err := n.sys.Listening(n.ctx)
	if err != nil {
		return ""
	}
	var ahi []ports.Socket
	for _, s := range socks {
		if s.Port() == port && n.spec.Filter.Wants(s.Proto) {
			ahi = append(ahi, s)
		}
	}
	if len(ahi) == 0 {
		return ""
	}
	return "nadie contesta " + n.spec.Request.Label(port) + "; " + n.quienes(ahi) + " escucha el " + u32(uint32(port)) + " en " + textoDirecciones(direccionesDe(ahi))
}

// quienSock nombra al dueño de un socket. La foto se renueva si el PID no
// está, o si el que está nació después del socket (se recicló): un servidor
// que arrancó mientras se esperaba no estaba en la primera.
func (n *nombrador) quienSock(s ports.Socket) string {
	p, ok := n.proc(s.PID)
	if !ok || (!s.Since.IsZero() && p.Created.After(s.Since)) {
		n.forest = nil
		p, ok = n.proc(s.PID)
	}
	if !ok {
		return quienEs("", s.PID)
	}
	return n.quien(s.PID)
}

// --- JSON -----------------------------------------------------------------------

// jsonEspera es el documento de --wait / --wait-free.
type jsonEspera struct {
	Espera      string             `json:"espera"`    // escuchar | libre
	Resultado   string             `json:"resultado"` // cumplido | vencio | cancelado | imposible | error
	Error       string             `json:"error,omitempty"`
	MS          int64              `json:"ms"` // cuánto duró
	PlazoMS     int64              `json:"plazo_ms"`
	EstableMS   int64              `json:"estable_ms,omitempty"`
	Alguno      bool               `json:"alguno,omitempty"` // --any
	Nuevos      bool               `json:"nuevos,omitempty"` // --new
	Lecturas    int                `json:"lecturas"`         // cuántas veces se leyó la tabla
	Puertos     []jsonPuertoEspera `json:"puertos"`
	SinResolver []string           `json:"sin_resolver,omitempty"`
}

// jsonPuertoEspera es un puerto de la espera. estado: cumplido |
// sosteniendose | pendiente | bloqueado. quienes: al esperar que escuche, los
// que escuchan (o contestan la dirección pedida); al esperar que se libere,
// los que todavía lo ocupan.
type jsonPuertoEspera struct {
	Puerto   uint16        `json:"puerto"`
	Destinos []string      `json:"destinos,omitempty"` // la dirección pedida: localhost, 127.0.0.1…
	Estado   string        `json:"estado"`
	YaEstaba bool          `json:"ya_estaba,omitempty"` // se cumplía en la primera lectura
	MS       int64         `json:"ms,omitempty"`        // a los cuántos ms del arranque se cumplió
	Cambios  int           `json:"cambios,omitempty"`   // veces que cambió de dueño o dejó de cumplirse
	Quienes  []jsonQuien   `json:"quienes"`
	Motivo   string        `json:"motivo,omitempty"` // bloqueado
	Remedios []jsonRemedio `json:"remedios,omitempty"`
}

// jsonQuien es un socket de la espera y su dueño.
type jsonQuien struct {
	PID       uint32        `json:"pid"`
	Proceso   string        `json:"proceso,omitempty"`
	Proto     string        `json:"proto"`
	Direccion string        `json:"direccion"`
	Desde     string        `json:"desde,omitempty"`
	Proyecto  *jsonProyecto `json:"proyecto,omitempty"`
}

func esperaJSON(spec reaper.WatchSpec, plazo time.Duration, rep reaper.WatchReport, req ports.Request, quien *nombrador, sys *sistema) jsonEspera {
	doc := jsonEspera{Espera: "escuchar", Resultado: map[reaper.WatchOutcome]string{reaper.WatchDone: "cumplido", reaper.WatchTimeout: "vencio",
		reaper.WatchCanceled: "cancelado", reaper.WatchImpossible: "imposible", reaper.WatchFailed: "error"}[rep.Outcome],
		MS: rep.End.Sub(rep.Start).Milliseconds(), PlazoMS: plazo.Milliseconds(), EstableMS: spec.Stable.Milliseconds(),
		Alguno: spec.Any, Nuevos: !spec.NewSince.IsZero(), Lecturas: rep.Reads, Puertos: []jsonPuertoEspera{}, SinResolver: req.Names}
	if spec.For == reaper.WatchFree {
		doc.Espera = "libre"
	}
	if rep.Err != nil {
		doc.Error = rep.Err.Error()
	}
	for _, p := range rep.Ports {
		jp := jsonPuertoEspera{Puerto: p.Port, Cambios: p.Changes, Quienes: []jsonQuien{},
			Estado: map[reaper.WatchPortState]string{reaper.WatchPending: "pendiente", reaper.WatchSettling: "sosteniendose",
				reaper.WatchMet: "cumplido", reaper.WatchBlocked: "bloqueado"}[p.State]}
		for _, d := range req.Dests(p.Port) {
			jp.Destinos = append(jp.Destinos, d.String())
		}
		if p.State == reaper.WatchMet {
			jp.YaEstaba = p.Already
			if !p.Already {
				jp.MS = p.MetAt.Sub(rep.Start).Milliseconds()
			}
		}
		if p.State == reaper.WatchBlocked {
			jp.Motivo = p.Blocked.Reason
			jp.Remedios = remediosJSON(p.Blocked.Remedies)
		}
		for _, s := range p.Witness {
			jq := jsonQuien{PID: s.PID, Proto: s.Proto.String(), Direccion: textoDireccion(s.Local.Addr())}
			if pr, ok := quien.proc(s.PID); ok {
				jq.Proceso = pr.Name
				if proy, ok := sys.proyectoDe(pr.ID); ok {
					jq.Proyecto = proyectoJSON(&proy)
				}
			}
			if !s.Since.IsZero() {
				jq.Desde = s.Since.Format(formatoHora)
			}
			jp.Quienes = append(jp.Quienes, jq)
		}
		doc.Puertos = append(doc.Puertos, jp)
	}
	return doc
}
