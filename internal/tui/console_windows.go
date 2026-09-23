//go:build windows

package tui

import (
	"os"

	"github.com/agustinyarrus/killport/internal/win"
)

// setupConsole activa ENABLE_VIRTUAL_TERMINAL_PROCESSING. No toca la página de
// códigos: el runtime de Go escribe en la consola con WriteConsoleW (UTF-16),
// así que los acentos y los bloques Unicode salen bien sin cambiar nada que
// después haya que restaurar en la shell del user.
func setupConsole(f *os.File) (vt bool, restore func(), isConsole bool) {
	h := f.Fd()
	mode, err := win.ConsoleMode(h)
	if err != nil {
		return false, nil, false
	}
	if mode&win.EnableVirtualTerminalProcessing != 0 {
		return true, nil, true
	}
	if err := win.SetConsoleMode(h, mode|win.EnableVirtualTerminalProcessing); err != nil {
		return false, nil, true // consola anterior a Windows 10: sin escapes
	}
	return true, func() { _ = win.SetConsoleMode(h, mode) }, true
}

func consoleWidth(f *os.File) int {
	cols, _, err := win.ConsoleSize(f.Fd())
	if err != nil {
		return 0
	}
	return cols
}

func consoleHeight(f *os.File) int {
	_, rows, err := win.ConsoleSize(f.Fd())
	if err != nil {
		return 0
	}
	return rows
}
