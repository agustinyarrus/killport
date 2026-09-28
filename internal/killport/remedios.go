package killport

// remedios.go: las salidas de un puerto que no se libera cerrando nada (un
// bloque de WinNAT, el rango dinámico corrido). reaper dice QUÉ salida es
// (reaper.RemedyKind); el comando de killport se arma acá, con los mismos
// nombres de flag con los que se registran (killport.go): si uno cambia de
// nombre, cambia en los dos lados.
//
// Puro.

import (
	"strconv"
	"strings"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

// Los flags que aparecen en los consejos, sin los guiones.
const (
	flagReserve    = "reserve"
	flagFixDynamic = "fix-dynamic"
	flagUDP        = "udp"
	flagWSL        = "wsl"
)

// comandoRemedio es el comando que se muestra (y el que va en el JSON) de una
// salida. Los pasos de una salida van unidos con "&&": en cmd y en PowerShell
// 7 corren en orden y el segundo solo si el primero anduvo; el PowerShell 5.1
// que trae Windows no entiende "&&" y NO corre nada, que es lo seguro (correr
// solo el primer paso es lo peligroso).
func comandoRemedio(r reaper.Remedy) string {
	switch r.Kind {
	case reaper.RemedyReserve:
		// --reserve reserva TCP si no se le dice otra cosa: para un bloque de
		// UDP tiene que decir --udp, o reservaría el protocolo que no es.
		s := "killport " + strconv.Itoa(int(r.Port)) + " --" + flagReserve
		if r.Proto == ports.UDP {
			s += " --" + flagUDP
		}
		return s
	case reaper.RemedyFixDynamic:
		return "killport --" + flagFixDynamic
	}
	return strings.Join(r.Steps, " && ")
}

// lineasRemedios son los renglones de las salidas, con los comandos
// alineados: "→ killport 3000 --reserve   lo reserva para vos…".
func lineasRemedios(t *tui.Term, rs []reaper.Remedy) []string {
	ancho := 0
	for _, r := range rs {
		ancho = max(ancho, tui.Width(comandoRemedio(r)))
	}
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		cabeza := "    " + t.Paint(tui.Faint, "→ ") + t.Paint(tui.Lavender, tui.PadRight(comandoRemedio(r), ancho)) + "  "
		col := 4 + 2 + ancho + 2 // donde empieza el efecto
		lugar := t.Width() - tui.Width(tui.Margin) - col
		if !t.Bounded() || tui.Width(r.Effect) <= lugar {
			out = append(out, cabeza+t.Paint(tui.Faint, r.Effect))
			continue
		}
		if lugar < 30 {
			// El comando es largo: el efecto va debajo, entero.
			out = append(out, strings.TrimRight(cabeza, " "))
			col, lugar = 8, t.Width()-tui.Width(tui.Margin)-8
			for _, l := range envolver(r.Effect, lugar) {
				out = append(out, strings.Repeat(" ", col)+t.Paint(tui.Faint, l))
			}
			continue
		}
		for i, l := range envolver(r.Effect, lugar) {
			if i == 0 {
				out = append(out, cabeza+t.Paint(tui.Faint, l))
				continue
			}
			out = append(out, strings.Repeat(" ", col)+t.Paint(tui.Faint, l))
		}
	}
	return out
}

// jsonRemedio es una salida en el JSON. comando es la salida entera, lista
// para ejecutar; pasos, si son varios, cada uno por separado (para una
// consola que no entiende "&&": se corren TODOS, en orden).
type jsonRemedio struct {
	Comando string   `json:"comando"`
	Pasos   []string `json:"pasos,omitempty"`
	Efecto  string   `json:"efecto"`
}

func remediosJSON(rs []reaper.Remedy) []jsonRemedio {
	var out []jsonRemedio
	for _, r := range rs {
		jr := jsonRemedio{Comando: comandoRemedio(r), Efecto: r.Effect}
		if len(r.Steps) > 1 {
			jr.Pasos = r.Steps
		}
		out = append(out, jr)
	}
	return out
}
