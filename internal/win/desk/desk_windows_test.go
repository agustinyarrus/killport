//go:build windows

package desk

import (
	"os"
	"testing"
)

// Q-03: el paquete que usan tui y clip2qr anda solo. Un archivo no es una
// consola (así detecta tui que la salida va a un pipe o a un archivo).
func TestArchivoNoEsConsola(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "x-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsConsole(f.Fd()) {
		t.Fatal("un archivo no es una consola")
	}
	if _, _, err := ConsoleSize(f.Fd()); err == nil {
		t.Fatal("un archivo no tiene tamaño de consola")
	}
	if systemDirectory() == "" {
		t.Fatal("sin carpeta del sistema")
	}
}
