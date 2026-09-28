package killport

// texto.go: helpers de presentación en español rioplatense, compartidos por el
// listado, los bloques de objetivo y la tarjeta final.

import (
	"strconv"
	"strings"

	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

func u32(n uint32) string { return strconv.FormatUint(uint64(n), 10) }

// nombreVisible y quienEs son las del núcleo (reaper.NameOrPID y
// reaper.NameWithPID): una sola forma de nombrar a un proceso.
var (
	nombreVisible = reaper.NameOrPID
	quienEs       = reaper.NameWithPID
)

// comandoCorto recorta una línea de comandos larga por el medio (se ve el
// principio y el último argumento, que suele ser lo que identifica al
// proceso). Para las líneas de Linux; las de Windows pasan antes por
// comandoLegible.
func comandoCorto(cmd string, ancho int) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	return tui.TruncateMiddle(cmd, ancho)
}

// textoPuertoDe describe cómo quedó un puerto, nombrado como lo pidió el
// usuario: "localhost:5173 libre", "puerto 3000 sigue ocupado".
func textoPuertoDe(p reaper.PortResult, etiqueta string) (tui.Kind, string) {
	proto := ""
	if p.Proto != 0 {
		proto = " " + p.Proto.String()
	}
	switch p.Status {
	case reaper.PortFree:
		return tui.OK, etiqueta + proto + " libre"
	case reaper.PortPhantom:
		return tui.Warn, etiqueta + " sigue ocupado por un fantasma"
	case reaper.PortRespawned:
		if p.Starting {
			return tui.Warn, etiqueta + " lo están relanzando"
		}
		return tui.Warn, etiqueta + " lo retomó otro proceso"
	case reaper.PortReserved:
		return tui.Info, etiqueta + " lo reserva Windows"
	case reaper.PortBusy:
		return tui.Fail, etiqueta + " sigue ocupado"
	case reaper.PortRefused:
		return tui.Skip, etiqueta + " no se toca"
	case reaper.PortAnswered:
		return tui.Warn, etiqueta + " lo contesta otro"
	}
	return tui.Info, etiqueta
}
