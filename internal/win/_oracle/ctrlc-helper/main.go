//go:build windows

// ctrlc-helper es un banco de pruebas del LADO AYUDANTE de killport: relanzado
// con DETACHED_PROCESS (sin consola propia), se engancha a la consola de otro
// proceso y le manda Ctrl+C con win.SendCtrlCToConsole, respetando el conjunto
// permitido. Escribe el resultado en stdout (que el orquestador redirige a un
// archivo, porque un proceso detached no tiene terminal donde mostrarlo).
//
//	ctrlc-helper PID  PID_PERMITIDO[,PID_PERMITIDO...]
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/agustinyarrus/killport/internal/win"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("uso: ctrlc-helper PID PERMITIDOS")
		os.Exit(2)
	}
	pid, _ := strconv.ParseUint(os.Args[1], 10, 32)
	allowed := map[uint32]time.Time{} // hora cero: basta el PID (el oráculo no la conoce)
	for _, s := range strings.Split(os.Args[2], ",") {
		if s == "" {
			continue
		}
		n, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
		allowed[uint32(n)] = time.Time{}
	}
	res, intruders, err := win.SendCtrlCToConsole(uint32(pid), allowed)
	name := map[win.CtrlCResult]string{
		win.CtrlCSent:      "ENVIADO",
		win.CtrlCNoConsole: "SIN_CONSOLA",
		win.CtrlCShared:    "COMPARTIDA",
	}[res]
	fmt.Printf("resultado=%s intrusos=%v err=%v\n", name, intruders, err)
}
