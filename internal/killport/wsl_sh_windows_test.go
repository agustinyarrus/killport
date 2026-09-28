//go:build windows

package killport

// Los scripts que killport corre DENTRO de una distro, ejecutados de verdad
// por dash —el /bin/sh de Debian y Ubuntu— sin WSL: el dash de Git for
// Windows corre sobre MSYS2, cuyo /proc imita al de Linux en lo que los
// scripts leen (/proc/PID/stat con starttime en el campo 22, /proc/PID/status
// con Uid:, /proc/PID/cmdline, /proc/stat con btime) y cuyo kill manda
// señales POSIX de verdad entre procesos MSYS. Así se prueba la escalera
// SIGINT → SIGTERM → SIGKILL, la verificación de identidad antes de cada
// señal y el sondeo (con un `ss` falso en el PATH que nombra a un proceso
// real), en el mismo intérprete que usará Ubuntu. Si no está Git for Windows,
// se saltean con explicación: los parsers ya están probados con salidas
// grabadas.

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/win"
)

const gitUsrBin = `C:\Program Files\Git\usr\bin`

func dashOSkip(t *testing.T) string {
	t.Helper()
	dash := filepath.Join(gitUsrBin, "dash.exe")
	if _, err := os.Stat(dash); err != nil {
		t.Skip("no hay dash de Git for Windows: los scripts se prueban solo con salidas grabadas")
	}
	return dash
}

// correrSh ejecuta un script con dash por la entrada estándar, como lo hace
// `wsl.exe -e /bin/sh -s`, con el ejecutor acotado de verdad. El PATH lleva
// las herramientas de MSYS (tr, awk, sleep…) aunque la prueba corra desde
// PowerShell, más las carpetas extra que se pidan (un `ss` falso).
func correrSh(t *testing.T, script string, extraPath ...string) string {
	t.Helper()
	res, err := correrShCrudo(t, script, extraPath...)
	if err != nil {
		t.Fatalf("dash: %v (salida %q, errores %q)", err, res.Stdout, res.Stderr)
	}
	return string(res.Stdout)
}

// correrShCrudo es correrSh sin veredicto: la salida (también la parcial) y
// el error, para la prueba que tiene que distinguir de qué se trata.
func correrShCrudo(t *testing.T, script string, extraPath ...string) (win.BoundedResult, error) {
	t.Helper()
	path := gitUsrBin + ";" + os.Getenv("PATH")
	for _, p := range extraPath {
		path += ";" + p
	}
	return win.RunBounded(context.Background(), dashOSkip(t), []string{"-s"},
		win.Bounded{Timeout: 30 * time.Second, Stdin: []byte(script), Env: []string{"PATH=" + path}})
}

// durmiente lanza un proceso MSYS que duerme (fuera de todo Job: lo tiene que
// matar el script, no el ejecutor) y devuelve su PID MSYS y su hora de
// arranque, leídos del /proc de MSYS como los leería el script.
type durmiente struct {
	cmd      *exec.Cmd
	pid      uint32
	arranque uint64
	salio    chan struct{}
}

func dormir(t *testing.T, ignorarSenales bool) *durmiente {
	t.Helper()
	dash := dashOSkip(t)
	// $$ es el PID MSYS, que sobrevive al exec (en MSYS, exec lanza otro
	// proceso de Windows pero conserva el PID POSIX): así se lo conoce sin
	// adivinar por el PID de Windows, que el exec cambia.
	code := "echo $$; exec sleep 60"
	if ignorarSenales {
		code = "trap '' INT TERM; echo $$; exec sleep 60" // lo ignorado se hereda a través de exec
	}
	cmd := exec.Command(dash, "-c", code)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	cmd.Env = append(os.Environ(), "PATH="+gitUsrBin+";"+os.Getenv("PATH"))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	d := &durmiente{cmd: cmd, salio: make(chan struct{})}
	pidCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString(byte(10)) // hasta el fin de línea
		pidCh <- strings.TrimSpace(line)
		io.Copy(io.Discard, stdout)
		cmd.Wait()
		close(d.salio)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		if d.pid != 0 {
			correrSh(t, "kill -KILL "+u32(d.pid)+" 2>/dev/null; exit 0")
		}
		<-d.salio
	})
	var pidStr string
	select {
	case pidStr = <-pidCh:
	case <-time.After(10 * time.Second):
		t.Fatal("el durmiente no dijo su PID")
	}
	pid, err := strconv.ParseUint(pidStr, 10, 32)
	if err != nil {
		t.Fatalf("PID MSYS ilegible %q", pidStr)
	}
	d.pid = uint32(pid)
	// Esperar a que el exec a sleep haya ocurrido (el comm pasa a "sleep").
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out := correrSh(t, "tr '\n' ' ' < /proc/"+pidStr+"/stat 2>/dev/null; exit 0")
		if comm, start, ok := parseStat(out); ok && comm == "sleep" {
			d.arranque = start
			return d
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("el durmiente (PID MSYS %s) no llegó a ser sleep", pidStr)
	return nil
}

func (d *durmiente) murio(espera time.Duration) bool {
	select {
	case <-d.salio:
		return true
	case <-time.After(espera):
		return false
	}
}

// El caso feliz: cede al SIGINT (el Ctrl+C de Linux) y el script lo dice.
func TestShCierreSIGINT(t *testing.T) {
	d := dormir(t, false)
	out := correrSh(t, scriptCierre(procesoLinux{PID: d.pid, Arranque: d.arranque}, 3*time.Second, false))
	if fin, err := resultadoCierre(out); err != nil || fin != "INT" {
		t.Fatalf("resultado = %q/%v (salida %q)", fin, err, out)
	}
	if !d.murio(5 * time.Second) {
		t.Fatal("el durmiente sigue vivo")
	}
}

// Ignora SIGINT y SIGTERM: la escalera llega a SIGKILL, respetando los plazos.
func TestShCierreSordoLlegaASIGKILL(t *testing.T) {
	d := dormir(t, true)
	start := time.Now()
	out := correrSh(t, scriptCierre(procesoLinux{PID: d.pid, Arranque: d.arranque}, 300*time.Millisecond, false))
	if fin, err := resultadoCierre(out); err != nil || fin != "KILL" {
		t.Fatalf("resultado = %q/%v (salida %q)", fin, err, out)
	}
	if el := time.Since(start); el < 300*time.Millisecond+graciaTerm {
		t.Fatalf("mandó SIGKILL a los %v: no esperó el plazo de SIGINT y la gracia de SIGTERM", el)
	}
	if !d.murio(5 * time.Second) {
		t.Fatal("el durmiente sigue vivo")
	}
}

// Identidad: si la hora de arranque no coincide (el PID lo reusó otro), el
// script NO manda nada y el proceso sigue vivo.
func TestShCierreIdentidadDistinta(t *testing.T) {
	d := dormir(t, false)
	out := correrSh(t, scriptCierre(procesoLinux{PID: d.pid, Arranque: d.arranque + 1}, 3*time.Second, false))
	if fin, err := resultadoCierre(out); err != nil || fin != "GONE" {
		t.Fatalf("resultado = %q/%v (salida %q)", fin, err, out)
	}
	if d.murio(300 * time.Millisecond) {
		t.Fatal("mató a un proceso que no era el pedido")
	}
}

// --force: SIGKILL directo, sin pasar por SIGINT.
func TestShCierreForce(t *testing.T) {
	d := dormir(t, false)
	start := time.Now()
	out := correrSh(t, scriptCierre(procesoLinux{PID: d.pid, Arranque: d.arranque}, 30*time.Second, true))
	if fin, err := resultadoCierre(out); err != nil || fin != "KILL" {
		t.Fatalf("resultado = %q/%v (salida %q)", fin, err, out)
	}
	if time.Since(start) > 10*time.Second || !d.murio(5*time.Second) {
		t.Fatal("con --force tenía que morir al toque")
	}
}

// El sondeo entero en dash: un `ss` falso (en el PATH, después de las
// carpetas del sistema, como quedaría uno real) dice que el durmiente escucha
// el 3000; el script extrae los PID, describe al proceso desde /proc y la
// salida se lee con el parser de siempre.
func TestShSondeoConSS(t *testing.T) {
	d := dormir(t, false)
	dir := t.TempDir()
	fake := "#!/bin/sh\n[ \"$1\" = -Hltnup ] || exit 1\n" +
		"echo 'tcp   LISTEN 0      511          0.0.0.0:3000       0.0.0.0:*    users:((\"sleep\",pid=" + u32(d.pid) + ",fd=3))'\n" +
		"echo 'tcp   LISTEN 0      511          0.0.0.0:3001       0.0.0.0:*    users:((\"fantasma\",pid=999999,fd=3))'\n"
	if err := os.WriteFile(filepath.Join(dir, "ss"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	out := correrSh(t, scriptSondeo, dir)
	sd, faltan, err := parseSondeo("MSYS", out)
	if err != nil {
		t.Fatalf("%v\nsalida:\n%s", err, out)
	}
	if sd.Fuente != "ss" || len(faltan) != 0 || len(sd.Sockets) != 2 {
		t.Fatalf("fuente %q, sockets %d", sd.Fuente, len(sd.Sockets))
	}
	if len(sd.Procesos) != 1 {
		t.Fatalf("procesos = %+v (el PID inexistente no tiene identidad y no cuenta)\n%s", sd.Procesos, out)
	}
	p := sd.Procesos[0]
	// (El usuario sale de /etc/passwd, que MSYS no llena: acá alcanza con el UID.)
	if p.PID != d.pid || p.Arranque != d.arranque || p.Comm != "sleep" || p.Cmdline != "sleep 60" || p.UID == 0 {
		t.Fatalf("proceso = %+v", p)
	}
	if p.Desde.IsZero() || time.Since(p.Desde) > 10*time.Minute || time.Until(p.Desde) > time.Minute {
		t.Fatalf("desde = %v (ahora %v): btime + starttime/CLK_TCK no cuadra", p.Desde, time.Now())
	}
	// Y la identidad que salió del sondeo es la que el cierre acepta.
	if fin, _ := resultadoCierre(correrSh(t, scriptCierre(p, 3*time.Second, false))); fin != "INT" || !d.murio(5*time.Second) {
		t.Fatalf("cerrar con la identidad del sondeo dio %q", fin)
	}
}

// Sin ss, el respaldo /proc corre entero en dash (en MSYS no hay
// /proc/net: las tablas vienen vacías, que es lo que se verifica que no rompa).
func TestShSondeoRespaldoProc(t *testing.T) {
	res, err := correrShCrudo(t, scriptSondeo)
	out := string(res.Stdout)
	if errors.Is(err, win.ErrTimeout) && strings.HasSuffix(out, "\n@fd\n") {
		// Colgado en `ls -l /proc/[0-9]*/fd/`. En Linux eso lee el kernel;
		// en MSYS, los fd de cada proceso MSYS de la máquina se le PREGUNTAN a
		// ese proceso por su tubería de señales, y uno que no contesta
		// (colgado, o saliendo) frena la lista entera. Pasa en una PC con
		// muchas consolas de Git Bash abiertas: es de MSYS, no del script.
		// Lo que el script escribe en este camino se prueba igual con las
		// salidas grabadas (wsl_sondeo_proc*.txt).
		t.Skipf("el /proc de MSYS se colgó listando los fd de los procesos MSYS de esta máquina (%v)", err)
	}
	if err != nil {
		t.Fatalf("dash: %v (salida %q, errores %q)", err, res.Stdout, res.Stderr)
	}
	sd, _, err := parseSondeo("MSYS", out)
	if err != nil {
		t.Fatalf("%v\nsalida:\n%s", err, out)
	}
	if sd.Fuente != "/proc" {
		t.Fatalf("fuente = %q", sd.Fuente)
	}
	out = correrSh(t, scriptProcesos([]uint32{1}))
	if _, err := leerSalida("MSYS", out); err != nil {
		t.Fatalf("segunda ida: %v\n%s", err, out)
	}
}
