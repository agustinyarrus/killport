//go:build windows

package killport

// externo.go: el lado de la herramienta de los OBJETIVOS QUE NO SON PROCESOS
// DE WINDOWS (reaper.External): un contenedor, un proceso Linux, una regla de
// portproxy. (El que registró una URL en http.sys no es un externo: es un
// proceso de Windows, y entra al plan como cualquier otro.)
//
// El núcleo los trata a todos igual —un External con una clave, un nombre y
// un Ref opaco que devuelve tal cual en OS.StopExternal— y la herramienta
// también: todo lo que cambia de un dominio a otro está en DOS lugares y en
// ninguno más.
//
//   - El tipo que viaja en External.Ref implementa `externo`: sabe detenerse,
//     presentarse en el bloque "antes de actuar" y anotarse en el JSON.
//   - Su clase (reaper.ExtKind) tiene una entrada en `dominios`: el
//     vocabulario de la pregunta, la narración, la tarjeta final y las claves
//     del JSON. (Cómo se lo NOMBRA lo dice el núcleo: reaper.External.Label.)
//
// Ni el adaptador (sistema.StopExternal), ni los bloques, ni la salida JSON
// conocen los dominios por nombre. Sumar uno es escribir un tipo, una entrada
// de vocabulario y quien conteste el reaper.Unmasking (ver desenmascarar.go).

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

// externo es lo que la herramienta sabe hacer con un objetivo externo.
type externo interface {
	// detener lo detiene con la operación de su dominio. deadline es el fin
	// del cierre prolijo: pasado eso el dominio fuerza, y con force va directo
	// a la fuerza. ok=false si se canceló antes de saber cómo terminó (el
	// motor lo deja pendiente, sin veredicto). Corre en su propia goroutine:
	// no puede tocar estado compartido sin candado.
	detener(ctx context.Context, s *sistema, deadline time.Time, force bool) (r reaper.ExternalStop, ok bool)

	// presentar arma su parte del bloque "antes de actuar": el título (sin el
	// punto de color) y las líneas propias. Los sockets, el intermediario que
	// lo sirve y la advertencia (Target.Caution) los pone el bloque.
	presentar(t *tui.Term, tg reaper.Target, now time.Time) (titulo string, lineas []string)

	// anotar suma sus datos al objetivo del documento --json.
	anotar(o *jsonObjetivo)
}

// conPermiso lo implementan los externos que solo se pueden detener como
// administrador (una regla de portproxy). Sirve para dos cosas: la pregunta lo
// avisa ANTES de que el usuario confirme, y si killport no corre elevado el
// objetivo viaja en el trabajo elevado en vez de darse por perdido.
type conPermiso interface {
	pideAdmin() bool
}

// pideAdmin dice si detener ese externo necesita administrador.
func pideAdmin(x reaper.External) bool {
	p, ok := x.Ref.(conPermiso)
	return ok && p.pideAdmin()
}

// dominio es el vocabulario de una clase de objetivos externos.
type dominio struct {
	tipo  string // clave estable del JSON: "contenedor", "linux"
	verbo string // el de la pregunta: "Detener", "Cerrar"
	// pedido es la línea que se narra al pedir la detención; enSeco con
	// --force (sin cierre prolijo).
	pedido  func(nombre string, enSeco bool) (texto, detalle string)
	hecho   string    // lo que se narra cuando salió bien: "se detuvo", "quedó borrada"
	forzado string    // con qué se lo fuerza cuando no cede: "SIGKILL"
	tarjeta string    // renglón de la tarjeta final
	punto   tui.Color // su color en la tarjeta
}

// dominios es el vocabulario de cada clase. El orden de la tarjeta final es
// el de reaper.ExtKind.
var dominios = map[reaper.ExtKind]dominio{
	reaper.ExtContainer: {
		tipo: "contenedor", verbo: "Detener",
		pedido: func(nombre string, enSeco bool) (string, string) {
			if enSeco {
				return "Deteniendo " + nombre, "en seco: sin esperar a que cierre"
			}
			return "Deteniendo " + nombre, ""
		},
		hecho: "se detuvo", forzado: "SIGKILL", tarjeta: "Contenedores detenidos", punto: tui.Sky,
	},
	reaper.ExtLinux: {
		tipo: "linux", verbo: "Cerrar",
		pedido: func(nombre string, enSeco bool) (string, string) {
			if enSeco {
				return "SIGKILL a " + nombre, ""
			}
			return "SIGINT a " + nombre, "si no cede: SIGTERM y después SIGKILL"
		},
		hecho: "se detuvo", forzado: "SIGKILL", tarjeta: "Procesos Linux cerrados", punto: tui.Teal,
	},
	reaper.ExtRule: {
		// Una regla no se cierra ni se fuerza: se borra, y --force no cambia nada.
		tipo: "regla", verbo: "Borrar",
		pedido:  func(nombre string, _ bool) (string, string) { return "Borrando " + nombre, "" },
		hecho:   "quedó borrada",
		tarjeta: "Reglas borradas", punto: tui.Peach,
	},
}

// dominioDe devuelve el vocabulario de una clase. Una clase sin entrada (un
// dominio nuevo al que todavía no se le escribió la suya) cae en uno genérico
// que se arma con el nombre que le da el núcleo: nada se rompe ni queda mudo.
func dominioDe(k reaper.ExtKind) dominio {
	if d, ok := dominios[k]; ok {
		return d
	}
	clase := k.String()
	if clase == "" {
		clase = "externo"
	}
	return dominio{
		tipo: clase, verbo: "Detener",
		pedido:  func(nombre string, _ bool) (string, string) { return "Deteniendo " + nombre, "" },
		hecho:   "se detuvo",
		tarjeta: "Otros detenidos", punto: tui.Lavender,
	}
}

// nombreExterno nombra un externo para títulos, preguntas y narración: "el
// contenedor db-1", "node en Ubuntu". Es el nombre que le da el núcleo (el
// mismo que aparece en los motivos de un puerto que no se liberó).
func nombreExterno(x reaper.External) string { return x.Label() }

// tipoExterno es la clave estable de la clase en el JSON.
func tipoExterno(k reaper.ExtKind) string { return dominioDe(k).tipo }

// errSinDominio: llegó un externo cuyo Ref no sabe detenerse. No debería
// pasar (lo arma la herramienta misma), pero el motor merece un veredicto.
var errSinDominio = errors.New("no sé detener ese objetivo")

// detenerExterno detiene UN externo por su dominio.
func detenerExterno(ctx context.Context, s *sistema, x reaper.External, deadline time.Time, force bool) (reaper.ExternalStop, bool) {
	ex, ok := x.Ref.(externo)
	if !ok {
		return reaper.ExternalStop{Outcome: reaper.OutcomeError, Err: errSinDominio}, true
	}
	r, ok := ex.detener(ctx, s, deadline, force)
	return r, ok && ctx.Err() == nil
}

// itemsExternos son los renglones de la tarjeta final para los externos que
// se detuvieron prolijo, uno por clase y en el orden de las clases. Los que
// hubo que forzar se cuentan aparte (van con los "en seco" de los procesos).
func itemsExternos(xs []reaper.ExternalResult) (items []tui.Item, enSeco int) {
	porClase := map[reaper.ExtKind]int{}
	for _, x := range xs {
		switch {
		case x.Outcome == reaper.OutcomeTerminated:
			enSeco++
		case x.Outcome.Done():
			porClase[x.External.Kind]++
		}
	}
	clases := make([]reaper.ExtKind, 0, len(porClase))
	for k := range porClase {
		clases = append(clases, k)
	}
	sort.Slice(clases, func(i, j int) bool { return clases[i] < clases[j] })
	for _, k := range clases {
		d := dominioDe(k)
		items = append(items, tui.Item{Label: d.tarjeta, Value: u32(uint32(porClase[k])), Dot: d.punto})
	}
	return items, enSeco
}
