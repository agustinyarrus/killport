//go:build windows

// shellexec prueba la plomería de ShellExecute con el verbo "open" (que NO
// dispara UAC) contra cmd /c exit N: verifica que el handle del proceso vuelve
// y que el código de salida se lee bien. El verbo "runas" (UAC) no se puede
// automatizar; esta prueba cubre todo salvo el clic del usuario.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/agustinyarrus/killport/internal/win"
)

func main() {
	want := 42
	if len(os.Args) > 1 {
		want, _ = strconv.Atoi(os.Args[1])
	}
	res, err := win.ShellExecute("open", "cmd.exe", []string{"/c", "exit", strconv.Itoa(want)}, true, true)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	fmt.Printf("exit code = %d (esperaba %d) match=%v\n", res.ExitCode, want, int(res.ExitCode) == want)
	if int(res.ExitCode) != want {
		os.Exit(1)
	}
}
