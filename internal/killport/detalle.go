//go:build windows

package killport

// detalle.go: lo que se MUESTRA de un plan además de lo que el plan ya dice.
// El plan (reaper.Plan) es puro y decide; acá se junta lo que hace falta para
// contarlo bien y que sale de preguntarle cosas al sistema:
//
//   - de un proceso que está detrás de http.sys, las URL que registró;
//   - de un servicio, cómo arranca: si vuelve solo después de detenerlo;
//   - de un sospechoso de siempre, qué es y cómo se lo saca de ese puerto.
//
// Nada de esto cambia qué se toca: un detalle que no se pudo leer es un
// renglón que no aparece.

import (
	"slices"
	"strings"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

// pidKernel es el PID de System: el que figura como dueño de los puertos de
// http.sys.
const pidKernel = 4

// detalle es lo que se averiguó para mostrar un plan.
type detalle struct {
	urls   map[reaper.ID][]string       // proceso → las URL que registró en http.sys
	inicio map[string]win.ServiceConfig // servicio (en minúsculas) → cómo arranca
	manual bool                         // la corrida es con --manual
	// proyectos: de qué proyecto es cada objetivo (su carpeta, su rama);
	// huerfanos: los objetivos que quedaron sueltos, con el motivo.
	proyectos map[reaper.ID]proyecto
	huerfanos map[reaper.ID]reaper.Orphan
	// sinDueno: el diagnóstico de los puertos pedidos que nadie escucha (libres,
	// o reservados por Windows), con la sugerencia de otro puerto para los que
	// no se pueden usar. Se cuenta en los bloques, antes de preguntar: así lo
	// ven también --list, la salida sin consola y el que contesta que no.
	sinDueno    []reaper.PortResult
	altSinDueno map[uint16]uint16
	// comandos: la línea de comandos y la ruta de cada objetivo y negativa
	// (proceso de Windows), para el JSON (conProcesos).
	comandos map[reaper.ID]comandoReal
	// contextos: lo que conviene saber de cada objetivo antes de confirmar
	// (conContexto).
	contextos map[reaper.ID]contextoObjetivo
}

// contextoObjetivo es lo que conviene saber de un objetivo ANTES de confirmar y que
// no sale del plan: a quién más le llega el Ctrl+C (los que comparten su
// consola), si es de otra sesión (otro usuario, SYSTEM: cerrarlo pide
// administrador) y si lo lanzó un servicio de terceros (que puede volver a
// lanzarlo).
type contextoObjetivo struct {
	tambien []reaper.Proc // no víctimas que recibirían el Ctrl+C (comparten la consola)
	// sinCtrlC: los de la consola que impiden el Ctrl+C (uno al margen del
	// pedido, un ajeno): no se manda y el objetivo se cierra en seco.
	sinCtrlC   []reaper.Proc
	alMargen   map[reaper.ID]bool // de sinCtrlC, los que quedaron al margen del pedido
	otraSesion bool
	servicio   string // el servicio de terceros que lo lanzó ("" si no)
	servProc   reaper.Proc
}

// conContexto junta el contexto de los objetivos que son procesos de Windows.
// Pregunta por la consola de cada uno (el ayudante: unos milisegundos), salvo
// con --force (no se manda Ctrl+C). Qué va a pasar con el Ctrl+C lo dice
// reaper (Plan.CtrlCOutlook), con la MISMA regla que usa el motor al
// mandarlo: lo que se avisa antes de preguntar es lo que después pasa. No es
// parte del plan: si algo falla, no se dice nada.
func (d *detalle) conContexto(sys *sistema, forest *reaper.Forest, plan reaper.Plan, svc map[uint32][]reaper.Service, cfg reaper.Config) {
	d.contextos = map[reaper.ID]contextoObjetivo{}
	alMargen := map[reaper.ID]bool{}
	for _, b := range plan.Bystanders {
		alMargen[b.Owner.Canon()] = true
	}
	var sesion uint32
	yo, soyYo := forest.Lookup(cfg.Self)
	if soyYo {
		sesion = yo.SessionID
	}
	for _, tg := range plan.Targets {
		if tg.Kind != reaper.KillProcess && tg.Kind != reaper.KillPhantom {
			continue
		}
		root, ok := forest.Lookup(tg.Root)
		if !ok {
			continue
		}
		var c contextoObjetivo
		c.otraSesion = soyYo && root.SessionID != sesion
		anc := forest.Ancestors(tg.Root)
		for _, a := range anc {
			if s, hay := svc[a.PID]; hay && len(s) > 0 && !reaper.IsServiceHost(a, forest, cfg.SystemRoot) && !reaper.WindowsImage(a, cfg.SystemRoot) {
				c.servicio, c.servProc = s[0].Name, a
				break
			}
		}
		if !cfg.Force {
			if pids, err := sys.ConsolePIDs(tg.Root); err == nil && !compartePropia(pids, cfg.OwnConsole) {
				c.ctrlC(plan.CtrlCOutlook(forest, pids), cfg.Self, alMargen)
			}
		}
		d.contextos[tg.Root.Canon()] = c
	}
}

// ctrlC pasa al contexto lo que reaper predice del Ctrl+C. Si alguien lo
// impide, no le llega a nadie: solo se dice quién lo impide (y el objetivo va
// en seco). killport mismo nunca figura (su consola no se toca: compartePropia).
func (c *contextoObjetivo) ctrlC(x reaper.CtrlCOutlook, self reaper.ID, alMargen map[reaper.ID]bool) {
	if len(x.Blocked) > 0 {
		c.sinCtrlC = x.Blocked
		c.alMargen = map[reaper.ID]bool{}
		for _, b := range x.Blocked {
			if alMargen[b.ID] {
				c.alMargen[b.ID] = true
			}
		}
		return
	}
	for _, a := range x.Also {
		if a.ID != self.Canon() {
			c.tambien = append(c.tambien, a)
		}
	}
}

// parentesco dice qué es a del objetivo root, para el aviso del Ctrl+C: el
// que comparte la consola es un ancestro (la terminal, npm, un supervisor) o,
// sin --tree, un hijo que no se nombró.
func parentesco(forest *reaper.Forest, root reaper.ID, a reaper.Proc) string {
	p, _ := forest.Lookup(root)
	switch {
	case p.Parent == a.PID && forest.IsAncestor(a.ID, root):
		return "su padre"
	case forest.IsAncestor(a.ID, root):
		return "un ancestro"
	case a.Parent == root.PID && forest.IsAncestor(root, a.ID):
		return "su hijo"
	}
	return "un descendiente"
}

// lineaSinCtrlC es el aviso del bloque cuando el Ctrl+C no se va a poder
// mandar: quién comparte la consola y no se toca. "" si no pasa.
func (c contextoObjetivo) lineaSinCtrlC() string {
	if len(c.sinCtrlC) == 0 {
		return ""
	}
	var quienes []string
	todosAlMargen := true
	for _, b := range c.sinCtrlC {
		quienes = append(quienes, quienEs(b.Name, b.PID))
		todosAlMargen = todosAlMargen && c.alMargen[b.ID]
	}
	que := "que no se toca"
	switch {
	case todosAlMargen && len(quienes) == 1:
		que = "que queda al margen"
	case todosAlMargen:
		que = "que quedan al margen"
	case len(quienes) > 1:
		que = "que no se tocan"
	}
	return "sin Ctrl+C: la consola la comparte también " + unirY(quienes) + ", " + que + "; se cierra en seco"
}

// compartePropia: la consola es la de killport (el motor no le manda Ctrl+C).
func compartePropia(pids []uint32, propia map[uint32]bool) bool {
	for _, p := range pids {
		if propia[p] {
			return true
		}
	}
	return false
}

// alcanzados son los que recibirían el Ctrl+C sin ser víctimas, de todos los
// objetivos (para la pregunta).
func (d detalle) alcanzados() []string {
	var out []string
	for _, c := range d.contextos {
		for _, a := range c.tambien {
			if s := quienEs(a.Name, a.PID); !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	slices.Sort(out)
	return out
}

// otraSesion: algún objetivo es de otra sesión (cerrarlo pide administrador).
func (d detalle) otraSesion() bool {
	for _, c := range d.contextos {
		if c.otraSesion {
			return true
		}
	}
	return false
}

// comandoReal es la línea de comandos y el ejecutable de un proceso, tal cual.
type comandoReal struct{ linea, ruta string }

// conProcesos junta la línea de comandos y la ruta de los objetivos y las
// negativas que son procesos de Windows (no del kernel): es lo que el JSON
// del reporte dice de cada uno, como el del listado.
func (d *detalle) conProcesos(sys *sistema, forest *reaper.Forest, plan reaper.Plan) {
	d.comandos = map[reaper.ID]comandoReal{}
	junta := func(id reaper.ID) {
		if id.PID <= pidKernel {
			return
		}
		var c comandoReal
		if p, ok := forest.Lookup(id); ok {
			c.ruta = p.Path
		}
		if dd, err := sys.detalle(id); err == nil {
			c.linea = unaLinea(dd.CommandLine)
		}
		d.comandos[id.Canon()] = c
	}
	for _, tg := range plan.Targets {
		if tg.Kind != reaper.StopExternal {
			junta(tg.Root)
		}
	}
	for _, r := range plan.Refusals {
		if !r.Owner.Zero() {
			junta(r.Owner)
		}
	}
}

// detallar junta los detalles de un plan. No pregunta nada nuevo que el
// desenmascarado no haya preguntado ya, salvo la configuración de los
// servicios (una lectura por servicio, sin administrador).
func detallar(sys *sistema, plan reaper.Plan, o opciones) detalle {
	d := detalle{urls: map[reaper.ID][]string{}, inicio: map[string]win.ServiceConfig{}, manual: o.manual,
		proyectos: map[reaper.ID]proyecto{}}
	servicio := func(nombre string) {
		if cfg, ok := sys.svc.de(nombre); ok {
			d.inicio[strings.ToLower(nombre)] = cfg
		}
	}
	for _, tg := range plan.Targets {
		if tg.Kind == reaper.StopExternal {
			continue
		}
		if tg.ViaClass == reaper.RelayHTTP && !tg.Via.Zero() {
			d.urls[tg.Root.Canon()] = sys.http.urlsDe(tg.Root.PID, tg.Ports)
		}
		if tg.Kind == reaper.StopThirdSvc {
			servicio(tg.Service)
		}
		if p, ok := sys.proyectoDe(tg.Root); ok {
			d.proyectos[tg.Root.Canon()] = p
		}
	}
	for _, r := range plan.Refusals {
		if r.Via.PID == pidKernel && !r.Owner.Zero() {
			d.urls[r.Owner.Canon()] = sys.http.urlsDe(r.PID, r.Ports)
		}
		if r.Service != "" {
			servicio(r.Service)
		}
	}
	return d
}

func (d detalle) inicioDe(servicio string) (win.ServiceConfig, bool) {
	c, ok := d.inicio[strings.ToLower(servicio)]
	return c, ok
}

// lineasURL son los renglones de las URL de un proceso que está detrás de
// http.sys: hasta cuatro, y cuántas quedan.
func lineasURL(t *tui.Term, urls []string) []string {
	const tope = 4
	var out []string
	for i, u := range urls {
		if i == tope {
			out = append(out, t.Paint(tui.Faint, "y "+tui.Count(int64(len(urls)-tope), "URL más", "URL más")))
			break
		}
		out = append(out, t.Paint(tui.Faint, "registró ")+t.Paint(tui.Subtle, u))
	}
	return out
}

// lineasServicio son los renglones de un servicio en el bloque de un
// OBJETIVO: cómo arranca, y qué hace falta para que no vuelva.
func (d detalle) lineasServicio(t *tui.Term, servicio string, puertos ports.Set) []string {
	cfg, ok := d.inicioDe(servicio)
	if !ok {
		return nil
	}
	out := []string{t.Paint(tui.Faint, lineaServicio(servicio, cfg))}
	vuelve, _ := vuelveSolo(cfg)
	switch {
	case d.manual && cfg.Start == win.StartDemand:
		out = append(out, t.Paint(tui.Faint, "--manual: ya está en inicio manual"))
	case d.manual:
		out = append(out, t.Paint(tui.Peach, "--manual: después de detenerlo queda en inicio manual (se deshace con `"+comandoInicio(servicio, cfg)+"`)"))
	case vuelve && cfg.Start == win.StartAuto:
		out = append(out, t.Paint(tui.Faint, "para que no vuelva: killport "+specDe(puertos)+" --manual"))
	}
	return out
}

// lineaConocido es el renglón de la tabla de sospechosos de siempre, o "".
func lineaConocido(servicio, imagen string, puertos ports.Set) string {
	if s, ok := conocido(servicio, imagen, puertos); ok {
		return s.texto()
	}
	return ""
}

// anotar suma los detalles al documento --json: los objetivos y las negativas
// del documento están en el mismo orden que los del plan.
func (d detalle) anotar(doc *jsonReporte, plan reaper.Plan) {
	for i, tg := range plan.Targets {
		if i >= len(doc.Objetivos) || tg.Kind == reaper.StopExternal {
			continue
		}
		o := &doc.Objetivos[i]
		o.URLs = d.urls[tg.Root.Canon()]
		c := d.comandos[tg.Root.Canon()]
		o.Comando, o.Ruta = c.linea, c.ruta
		if p, ok := d.proyectos[tg.Root.Canon()]; ok {
			o.Proyecto = proyectoJSON(&p)
		}
		if h, ok := d.huerfanos[tg.Root.Canon()]; ok {
			o.Huerfano = h.Why
		}
		o.Conocido = lineaConocido(tg.Service, tg.Name, tg.Ports)
		if cfg, ok := d.inicioDe(tg.Service); ok && tg.Kind == reaper.StopThirdSvc {
			o.Inicio = inicioJSON(cfg)
		}
	}
	for i, r := range plan.Refusals {
		if i >= len(doc.Negativas) {
			continue
		}
		n := &doc.Negativas[i]
		n.URLs = d.urls[r.Owner.Canon()]
		c := d.comandos[r.Owner.Canon()]
		n.Comando, n.Ruta = c.linea, c.ruta
		n.Conocido = lineaConocido(r.Service, r.Name, r.Ports)
		if cfg, ok := d.inicioDe(r.Service); ok && r.Service != "" {
			n.Inicio = inicioJSON(cfg)
		}
	}
}
