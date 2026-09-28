//go:build windows && killport_e2e

package killport

import "os"

// Solo en el binario de las pruebas de punta a punta (go build -tags
// killport_e2e): permite reemplazar wsl.exe por un doble, porque en la
// máquina de pruebas no hay distros. Un ejecutable normal no tiene este
// archivo: no hay variable de entorno que cambie qué wsl.exe corre killport.
func init() {
	if p := os.Getenv("NAVAJA_E2E_WSL_EXE"); p != "" {
		wslExe = p
	}
}
