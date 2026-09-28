package killport

// netsh.go: el contrato de netsh para el resto del paquete, sin tocar el
// sistema (quien lo corre de verdad está en red.go). Todo lo que killport sabe
// de la red de Windows que no sale de una API —los rangos excluidos, el rango
// dinámico, las colas de http.sys— y todo lo que cambia —una exclusión, una
// regla de portproxy— pasa por una función de este tipo, que las pruebas
// reemplazan por un doble.

import (
	"context"
	"fmt"
	"strings"
)

// netshFn corre netsh con esos argumentos y devuelve su salida como texto. Un
// código de salida distinto de cero es un *errorNetsh, que lleva lo que netsh
// alcanzó a decir.
type netshFn func(ctx context.Context, args ...string) (string, error)

// errorNetsh es un netsh que corrió y dijo que no.
type errorNetsh struct {
	Codigo uint32
	Dijo   string // la primera línea con texto de su salida (en el idioma de Windows)
}

func (e *errorNetsh) Error() string {
	if e.Dijo != "" {
		return "netsh: " + e.Dijo
	}
	return fmt.Sprintf("netsh salió con el código %d", int32(e.Codigo))
}

// primeraConTexto es la primera línea que dice algo.
func primeraConTexto(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
