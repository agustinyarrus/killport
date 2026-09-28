//go:build windows

package tui

import (
	"os"

	"github.com/agustinyarrus/killport/internal/win/desk"
)

// setupConsole activa ENABLE_VIRTUAL_TERMINAL_PROCESSING. No toca la página de
// códigos: el runtime de Go escribe en la consola con WriteConsoleW (UTF-16),
// así que los acentos y los bloques Unicode salen bien sin cambiar nada que
// después haya que restaurar en la shell del user.
func setupConsole(f *os.File) (vt bool, restore func(), isConsole bool) {
	h := f.Fd()
	mode, err := desk.ConsoleMode(h)
	if err != nil {
		return false, nil, false
	}
	if mode&desk.EnableVirtualTerminalProcessing != 0 {
		return true, nil, true
	}
	if err := desk.SetConsoleMode(h, mode|desk.EnableVirtualTerminalProcessing); err != nil {
		return false, nil, true // consola anterior a Windows 10: sin escapes
	}
	return true, func() { _ = desk.SetConsoleMode(h, mode) }, true
}

// stdinIsConsole dice si la entrada estándar es un teclado de consola (no un
// pipe ni un archivo redirigido): GetConsoleMode solo tiene éxito sobre una
// consola real.
func stdinIsConsole(f *os.File) bool { return desk.IsConsole(f.Fd()) }

func consoleWidth(f *os.File) int {
	cols, _, err := desk.ConsoleSize(f.Fd())
	if err != nil {
		return 0
	}
	return cols
}

func consoleHeight(f *os.File) int {
	_, rows, err := desk.ConsoleSize(f.Fd())
	if err != nil {
		return 0
	}
	return rows
}
