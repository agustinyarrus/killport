//go:build !windows

package tui

import (
	"os"
	"strconv"
)

// Fuera de Windows se asume un terminal VT si stdout es un dispositivo de
// caracteres; el ancho sale de $COLUMNS (sin ioctl, para no sumar dependencias).
func setupConsole(f *os.File) (vt bool, restore func(), isConsole bool) {
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false, nil, false
	}
	return true, nil, true
}

func consoleWidth(*os.File) int {
	n, _ := strconv.Atoi(os.Getenv("COLUMNS"))
	return n
}

func consoleHeight(*os.File) int {
	n, _ := strconv.Atoi(os.Getenv("LINES"))
	return n
}
