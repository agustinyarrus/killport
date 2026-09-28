//go:build windows

// killport libera el puerto que necesitás: muestra quién lo ocupa, lo cierra
// con cuidado y verifica que haya quedado libre. Es solo para Windows: en otro
// sistema no hay nada que compilar (lo puro de internal/ sí compila y se prueba).
package main

import (
	"os"

	"github.com/agustinyarrus/killport/internal/killport"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/version"
)

func main() {
	t := tui.Open()
	defer t.Close()
	os.Exit(killport.Main(t, version.String(), os.Args[1:]))
}
