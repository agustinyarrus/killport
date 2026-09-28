//go:build windows

// wslfalso es el DOBLE de wsl.exe para las pruebas de punta a punta. Contesta
// las dos formas en que killport llama a wsl.exe, igual que el de verdad:
//
//	-l --running -q                   → las distros de NAVAJA_E2E_DISTROS, en
//	                                    UTF-16LE con CRLF (como wsl.exe)
//	-d D -u root -e /bin/sh -s        → corre el script de la entrada estándar
//	                                    con el dash de Git for Windows (el
//	                                    /bin/sh de Ubuntu), con el `ss` falso de
//	                                    NAVAJA_E2E_SS en el PATH
//
// Anota cada llamada en NAVAJA_E2E_LOG, para que la prueba verifique que
// killport nunca nombró una distro que no estaba corriendo.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"unicode/utf16"
)

func main() {
	args := os.Args[1:]
	if f, err := os.OpenFile(os.Getenv("NAVAJA_E2E_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
	}
	var distros []string
	for _, d := range strings.Split(os.Getenv("NAVAJA_E2E_DISTROS"), ",") {
		if d != "" {
			distros = append(distros, d)
		}
	}
	switch {
	case strings.Join(args, " ") == "-l --running -q":
		var out []byte
		for _, u := range utf16.Encode([]rune(strings.Join(distros, "\r\n") + "\r\n")) {
			out = append(out, byte(u), byte(u>>8))
		}
		if len(distros) == 0 {
			out = nil
		}
		os.Stdout.Write(out)
	case len(args) == 7 && args[0] == "-d" && strings.Join(args[2:], " ") == "-u root -e /bin/sh -s":
		if !slices.Contains(distros, args[1]) {
			// El wsl.exe de verdad la ARRANCARÍA: la prueba lo trata como falla.
			fmt.Fprintln(os.Stderr, "ARRANCARÍA", args[1])
			os.Exit(99)
		}
		cmd := exec.Command(os.Getenv("NAVAJA_E2E_DASH"), "-s")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Env = append(os.Environ(), "PATH="+os.Getenv("NAVAJA_E2E_SS")+";"+os.Getenv("PATH"))
		if err := cmd.Run(); err != nil {
			if e, ok := err.(*exec.ExitError); ok {
				os.Exit(e.ExitCode())
			}
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "llamada inesperada:", strings.Join(args, " "))
		os.Exit(2)
	}
}
