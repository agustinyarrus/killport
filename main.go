// killport libera el puerto que necesitás: muestra quién lo ocupa, lo cierra
// con cuidado y verifica que haya quedado libre.
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
