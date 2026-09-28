//go:build windows

package killport

// ayudante.go: los dos modos OCULTOS que tocan la consola de OTRO proceso, y
// el lado del padre que los lanza. killport vive en la terminal del usuario y
// no puede engancharse a otra consola sin perder la suya (un proceso está atado
// a una sola). Por eso relanza una copia de sí mismo con DETACHED_PROCESS —sin
// consola propia—, que se engancha a la del objetivo, hace lo justo y muere.
// El resultado vuelve por su salida estándar, que el padre captura.
//
//	--console-of PID          → lista los PID de esa consola (para agrupar)
//	--ctrl-c-helper PID --allow IDS → manda Ctrl+C si todos son permitidos
//
// El protocolo de salida es una línea:
//	MEMBERS p,p,p | NOCONSOLE            (--console-of)
//	SENT | NOCONSOLE | SHARED p,p        (--ctrl-c-helper)

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/win"
)

const detachedProcess = 0x00000008 // CREATE flag: el hijo arranca sin consola

// modoAyudante corre en la copia DETACHED. Devuelve (código de salida, true) si
// los argumentos pedían un modo ayudante; (0, false) si no (killport normal).
func modoAyudante(args []string) (int, bool) {
	consoleOf, hasConsoleOf := valorDe(args, "--console-of")
	ctrlcOf, hasCtrlc := valorDe(args, "--ctrl-c-helper")
	switch {
	case hasConsoleOf:
		return correrConsoleOf(consoleOf), true
	case hasCtrlc:
		allow, _ := valorDe(args, "--allow")
		return correrCtrlCHelper(ctrlcOf, allow), true
	}
	return 0, false
}

func correrConsoleOf(pidStr string) int {
	pid := atou32(pidStr)
	members, hay, err := win.ConsoleMembers(pid)
	if err != nil || !hay {
		fmt.Println("NOCONSOLE")
		return 0
	}
	fmt.Println("MEMBERS " + juntarPIDs(members))
	return 0
}

func correrCtrlCHelper(pidStr, allowStr string) int {
	pid := atou32(pidStr)
	allowed := map[uint32]time.Time{}
	for _, id := range decodeIDs(allowStr) {
		allowed[id.PID] = id.Created
	}
	res, intruders, err := win.SendCtrlCToConsole(pid, allowed)
	if err != nil {
		fmt.Println("NOCONSOLE")
		return 0
	}
	switch res {
	case win.CtrlCSent:
		fmt.Println("SENT")
	case win.CtrlCShared:
		fmt.Println("SHARED " + juntarPIDs(intruders))
	default:
		fmt.Println("NOCONSOLE")
	}
	return 0
}

// pedirConsola lanza el ayudante --console-of y parsea su respuesta.
func (s *sistema) pedirConsola(pid uint32) (members []uint32, hay bool, err error) {
	salida, err := s.correrAyudante("--console-of", strconv.FormatUint(uint64(pid), 10))
	if err != nil {
		return nil, false, err
	}
	if rest, ok := strings.CutPrefix(salida, "MEMBERS "); ok {
		return partirPIDs(rest), true, nil
	}
	return nil, false, nil // NOCONSOLE
}

// mandarCtrlC lanza el ayudante --ctrl-c-helper y traduce su veredicto a los
// centinelas del motor.
func (s *sistema) mandarCtrlC(pid uint32, allow []reaper.ID) error {
	salida, err := s.correrAyudante("--ctrl-c-helper", strconv.FormatUint(uint64(pid), 10), "--allow", idsEncoded(allow))
	if err != nil {
		return err
	}
	switch {
	case salida == "SENT":
		return nil
	case strings.HasPrefix(salida, "SHARED"):
		rest, _ := strings.CutPrefix(salida, "SHARED ")
		return &reaper.SharedConsoleError{Intruders: partirPIDs(rest)}
	default:
		return reaper.ErrNoConsole
	}
}

// correrAyudante lanza killport de nuevo con DETACHED_PROCESS y sin ventana, y
// devuelve la primera línea de su salida. Como es detached, su stdout se
// captura por un pipe (no tiene terminal donde escribir). Tope de tiempo por si
// el ayudante se cuelga enganchado a una consola trabada.
func (s *sistema) correrAyudante(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("el ayudante de consola falló: %w", err)
	}
	return strings.TrimSpace(primeraLinea(string(out))), nil
}

// --- helpers de texto ------------------------------------------------------

func valorDe(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			return v, true
		}
	}
	return "", false
}

func juntarPIDs(pids []uint32) string {
	parts := make([]string, len(pids))
	for i, p := range pids {
		parts[i] = strconv.FormatUint(uint64(p), 10)
	}
	return strings.Join(parts, ",")
}

func partirPIDs(s string) []uint32 {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []uint32
	for _, p := range strings.Split(s, ",") {
		if v := atou32(strings.TrimSpace(p)); v != 0 {
			out = append(out, v)
		}
	}
	return out
}

func atou32(s string) uint32 {
	v, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	return uint32(v)
}

func primeraLinea(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
