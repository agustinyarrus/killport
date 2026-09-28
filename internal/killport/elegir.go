//go:build windows

package killport

// elegir.go: los selectores (seleccion.go) contra el sistema de verdad: de
// cada dueño de un socket que escucha se averigua lo que haga falta —si quedó
// suelto, su carpeta, su proyecto— y solo si algún selector lo pregunta.

import (
	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

// elegir devuelve el predicado de lo elegido (para reaper.Focus), o nil si
// no hay selectores: sockets cuyo dueño —el real, o los que heredaron el
// socket de un dueño muerto— cumple todo lo pedido. La fila elegida del
// listado interactivo (o.elegido) es un selector más: ese proceso y ningún
// otro. ajenos cuenta los que cumplían lo pedido pero corren en otra sesión
// (otro usuario, un servicio): -n y --project se quedan en la sesión de
// killport salvo --system, como --orphans. Nombrar un programa no es pedir
// que se cierre el de otro usuario, ni que se lance el UAC para eso.
func elegir(sys *sistema, o opciones, forest *reaper.Forest, socks []ports.Socket, svc map[uint32][]reaper.Service, cfg reaper.Config) (picked func(ports.Socket) bool, ajenos int) {
	if !o.sel.activa() && o.elegido.Zero() {
		return nil, 0
	}
	huerfanos := map[reaper.ID]bool{}
	if o.sel.huerfanos {
		// El criterio "de desarrollo" que el núcleo no ve: la carpeta del
		// proceso es la de un proyecto.
		dev := func(p reaper.Proc) bool { _, ok := sys.proyectoDe(p.ID); return ok }
		for _, h := range reaper.Orphans(forest, socks, svc, cfg, dev) {
			if h.Dev || o.system {
				huerfanos[h.ID] = true
			}
		}
	}
	enSesion := sesionDe(forest, cfg)
	filtro := o.filtro()
	elegido := map[reaper.ID]bool{}
	visto := map[reaper.ID]bool{}
	for _, s := range socks {
		if filtro.Relate(s) != ports.Holds {
			continue
		}
		// Un dueño fantasma no se elige por su nombre ni su carpeta: su
		// heredero sí (no tiene filas propias: el socket figura a nombre del
		// muerto).
		duenos, _ := duenosSocket(forest, s)
		for _, p := range duenos {
			if visto[p.ID] {
				continue
			}
			visto[p.ID] = true
			if !o.elegido.Zero() && p.ID != o.elegido.Canon() {
				continue
			}
			c := candidato{nombre: p.Name, ruta: p.Path, huerfano: huerfanos[p.ID]}
			if o.sel.carpeta != "" {
				if d, err := sys.detalle(p.ID); err == nil {
					c.cwd = d.Cwd
				}
				if pr, ok := sys.proyectoDe(p.ID); ok {
					c.proyecto = &pr
				}
			}
			if !o.sel.cumple(c) {
				continue
			}
			if o.elegido.Zero() && !o.system && !enSesion(p) {
				ajenos++ // la fila elegida a mano sí vale: el usuario la señaló
				continue
			}
			elegido[p.ID] = true
		}
	}
	return func(s ports.Socket) bool {
		duenos, _ := duenosSocket(forest, s)
		for _, p := range duenos {
			if elegido[p.ID] {
				return true
			}
		}
		return false
	}, ajenos
}

// sesionDe dice si un proceso corre en la sesión de killport. Si killport no
// aparece en la foto (rarísimo), se descarta al menos la sesión 0, la de los
// servicios (el mismo criterio que reaper.Orphans).
func sesionDe(forest *reaper.Forest, cfg reaper.Config) func(reaper.Proc) bool {
	if self, ok := forest.Lookup(cfg.Self); ok {
		return func(p reaper.Proc) bool { return p.SessionID == self.SessionID }
	}
	return func(p reaper.Proc) bool { return p.SessionID != 0 }
}

// puertosElegidos son los puertos que ocupan los sockets elegidos.
func puertosElegidos(socks []ports.Socket, filtro ports.Filter, picked func(ports.Socket) bool) ports.Set {
	var ps []uint16
	for _, s := range socks {
		if filtro.Relate(s) == ports.Holds && picked(s) {
			ps = append(ps, s.Port())
		}
	}
	return ports.Of(ps...)
}

// nadaElegido: los selectores no encontraron a nadie que escuche. No es un
// error —no hay nada que liberar—, así que sale 0. ajenos son los que
// cumplían lo pedido en otra sesión (se dice, y cómo incluirlos).
func nadaElegido(t *tui.Term, o opciones, ajenos int) int {
	if o.json {
		return escribirJSON(t, construirJSONReporte(reaper.Report{}, reaper.Plan{}, !o.yes))
	}
	t.Lines(t.Header("killport", "libera el puerto que necesitás", o.version))
	que := o.sel.describir()
	if que == "" {
		que = "lo elegido"
	}
	detalle := "nada de lo que escucha un puerto es " + que
	switch {
	case ajenos > 0:
		detalle += " en tu sesión; " + tui.Int(int64(ajenos)) + " en otras sesiones (otro usuario o servicios): --system los incluye"
	case o.sel.huerfanos && !o.system:
		detalle += " (--system suma los sueltos que no parecen de desarrollo)"
	}
	t.Line(t.Status(tui.Info, "No hay nada que cerrar", detalle))
	t.Blank()
	return cli.ExitOK
}
