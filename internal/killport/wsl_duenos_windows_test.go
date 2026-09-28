//go:build windows

package killport

// Quién es de verdad el dueño adentro de WSL: el PID 1
// de la distro y la confirmación de dueños por inodo (S-03), y las barras
// invertidas en el sondeo (C-03).

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
)

// sondeoDe arma la salida del script de sondeo en una distro con ss: las
// filas de ss, las tablas de /proc/net (tcp) y los procesos descriptos.
func sondeoDe(ss []string, tcp []string, procs ...string) string {
	out := []string{"@killport 1", "@ss"}
	out = append(out, ss...)
	if tcp != nil {
		out = append(out, "@net tcp", "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode")
		out = append(out, tcp...)
		out = append(out, "@net tcp6", "@net udp", "@net udp6")
	}
	out = append(out, "@clock 1759000000 100")
	out = append(out, procs...)
	out = append(out, "@fin")
	return strings.Join(out, "\n") + "\n"
}

// procDe describe un proceso como lo hace scriptDescribir.
func procDe(pid int, comm, uid, user, cmd, inodos string) string {
	p := strconv.Itoa(pid)
	return strings.Join([]string{
		"@pid " + p,
		"S " + p + " (" + comm + ") S 1 1 1 0 -1 4194560 1 1 0 0 1 1 0 0 20 0 1 0 " + strconv.Itoa(1000+pid) + " 1 1",
		"U " + uid + " " + user,
		"C " + cmd,
		"F " + inodos,
	}, "\n")
}

// filaNet es una fila de /proc/net/tcp en LISTEN (0A) en 0.0.0.0:puerto.
func filaNet(port uint16, inode int) string {
	return "   0: 00000000:" + strings.ToUpper(strconv.FormatUint(uint64(port)+0x10000, 16)[1:]) +
		" 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 " + strconv.Itoa(inode) + " 1 0000000000000000 100 0 0 10 0"
}

func objetivoWSL(t *testing.T, salida string, port uint16) reaper.Unmasking {
	t.Helper()
	sd, _, err := parseSondeo("Ubuntu", salida)
	if err != nil {
		t.Fatal(err)
	}
	w := &wsl{sondeos: []sondeo{sd}, distros: []string{"Ubuntu"}}
	_, duenos := w.escuchaEn(port, ports.TCP)
	return w.encontrado(context.Background(), reaper.Unmasking{Port: port, Proto: ports.TCP}, duenos, nil, nil, true)
}

// S-03: con la activación por sockets, ss nombra a sshd y a systemd (PID 1),
// y los dos tienen el socket. El PID 1 no se toca, y como systemd vuelve a
// escuchar, tampoco se ofrece cerrar a sshd: se dice cómo se detiene la unidad.
func TestWSLSystemdPID1NoEsObjetivo(t *testing.T) {
	salida := sondeoDe(
		[]string{`tcp LISTEN 0 4096 0.0.0.0:22 0.0.0.0:* users:(("sshd",pid=812,fd=3),("systemd",pid=1,fd=111))`},
		[]string{filaNet(22, 5555)},
		procDe(1, "systemd", "0", "root", "/sbin/init", "5555 111 112"),
		procDe(812, "sshd", "0", "root", "sshd: /usr/sbin/sshd -D", "5555"),
	)
	u := objetivoWSL(t, salida, 22)
	if u.Verdict != reaper.UnmaskUnavailable || len(u.Behind) != 0 || !strings.Contains(u.Reason, "systemd") || !strings.Contains(u.Advice, "systemctl stop") {
		t.Fatalf("22 = %+v", u)
	}
	// Lo mismo sin tablas de /proc/net (lo que ss dice, sin confirmar).
	salida = sondeoDe(
		[]string{`tcp LISTEN 0 4096 0.0.0.0:22 0.0.0.0:* users:(("sshd",pid=812,fd=3),("systemd",pid=1,fd=111))`}, nil,
		procDe(1, "systemd", "0", "root", "/sbin/init", ""),
		procDe(812, "sshd", "0", "root", "sshd: /usr/sbin/sshd -D", ""),
	)
	if u := objetivoWSL(t, salida, 22); u.Verdict != reaper.UnmaskUnavailable || len(u.Behind) != 0 {
		t.Fatalf("22 sin /proc/net = %+v", u)
	}
}

// S-03: un proceso SIN privilegios se pone de nombre `a",pid=1,fd=3` (prctl) y
// ss imprime users:(("a",pid=1,fd=3",pid=322,fd=4)). Leído al pie de la
// letra, el dueño es el PID 1 y el de verdad desaparece. Con la confirmación
// por inodo, el dueño es el que tiene el socket abierto: el 322.
func TestWSLNombreFalsoNoEngaña(t *testing.T) {
	salida := sondeoDe(
		[]string{`tcp LISTEN 0 128 0.0.0.0:8791 0.0.0.0:* users:(("a",pid=1,fd=3",pid=322,fd=4))`},
		[]string{filaNet(8791, 7777)},
		procDe(1, "systemd", "0", "root", "/sbin/init", "111 112"),
		procDe(322, `a",pid=1,fd=3`, "1000", "mauro", "./a", "7777"),
	)
	u := objetivoWSL(t, salida, 8791)
	if u.Verdict != reaper.UnmaskFound || len(u.Behind) != 1 {
		t.Fatalf("8791 = %+v", u)
	}
	if p := u.Behind[0].Ref.(*procesoLinux); p.PID != 322 || p.Usuario != "mauro" {
		t.Fatalf("objetivo = %+v", p)
	}
	// Y ss nombrando a un root que existe (sshd) sin que tenga el socket:
	// tampoco se le cree.
	salida = sondeoDe(
		[]string{`tcp LISTEN 0 128 0.0.0.0:8792 0.0.0.0:* users:(("sshd",pid=812,fd=",pid=323,fd=4))`},
		[]string{filaNet(8792, 7778), filaNet(22, 5555)},
		procDe(812, "sshd", "0", "root", "sshd", "5555"),
		procDe(323, `sshd",pid=812,`, "1000", "mauro", "./a", "7778"),
	)
	u = objetivoWSL(t, salida, 8792)
	if u.Verdict != reaper.UnmaskFound || len(u.Behind) != 1 || u.Behind[0].Ref.(*procesoLinux).PID != 323 {
		t.Fatalf("8792 = %+v", u)
	}
}

// lanzarMSYS corre `code` en dash (oculto) y espera a que el proceso cuyo PID
// MSYS imprime ($$) pase a llamarse comm. Limpia en t.Cleanup: mata a sus
// hijos MSYS y a él, por PID MSYS.
func lanzarMSYS(t *testing.T, code, comm string) (pid uint32, arranque uint64) {
	t.Helper()
	dash := dashOSkip(t)
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
	salio := make(chan struct{})
	pidCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString(byte(10))
		pidCh <- strings.TrimSpace(line)
		io.Copy(io.Discard, stdout)
		cmd.Wait()
		close(salio)
	}()
	var pidStr string
	select {
	case pidStr = <-pidCh:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("no dijo su PID")
	}
	v, _ := strconv.ParseUint(pidStr, 10, 32)
	pid = uint32(v)
	t.Cleanup(func() {
		correrSh(t, "for d in /proc/[0-9]*; do [ \"$(cat $d/ppid 2>/dev/null)\" = "+pidStr+" ] && kill -KILL ${d#/proc/} 2>/dev/null; done; kill -KILL "+pidStr+" 2>/dev/null; exit 0")
		cmd.Process.Kill()
		<-salio
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out := correrSh(t, "tr '\\n' ' ' < /proc/"+pidStr+"/stat 2>/dev/null; exit 0")
		if c, start, ok := parseStat(out); ok && c == comm {
			return pid, start
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("PID MSYS %s no llegó a ser %s", pidStr, comm)
	return 0, 0
}

// C-03: el sondeo en dash de verdad, con un proceso cuya línea de comandos
// tiene barras invertidas (`awk … C:\code\app.js`: "\c" corta el echo de
// dash). Los dos procesos salen enteros y cada uno con lo suyo; da igual cuál
// quede primero en el orden del script.
func TestShSondeoBarraInvertida(t *testing.T) {
	a, aStart := lanzarMSYS(t, `echo $$; exec awk 'BEGIN{system("sleep 20")}' 'C:\code\app.js'`, "awk")
	d := dormir(t, false)
	dir := t.TempDir()
	fake := "#!/bin/sh\n[ \"$1\" = -Hltnup ] || exit 1\n" +
		"echo 'tcp   LISTEN 0      511          0.0.0.0:3000       0.0.0.0:*    users:((\"awk\",pid=" + u32(a) + ",fd=3))'\n" +
		"echo 'tcp   LISTEN 0      511          0.0.0.0:3001       0.0.0.0:*    users:((\"sleep\",pid=" + u32(d.pid) + ",fd=3))'\n"
	if err := os.WriteFile(filepath.Join(dir, "ss"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	out := correrSh(t, scriptSondeo, dir)
	sd, _, err := parseSondeo("MSYS", out)
	if err != nil {
		t.Fatalf("%v\nsalida:\n%s", err, out)
	}
	var okA, okD bool
	for _, p := range sd.Procesos {
		if p.PID == a && p.Arranque == aStart && p.Comm == "awk" && strings.Contains(p.Cmdline, `C:\code\app.js`) && p.puertos().Equal(ports.Of(3000)) {
			okA = true
		}
		if p.PID == d.pid && p.Arranque == d.arranque && p.Comm == "sleep" && p.puertos().Equal(ports.Of(3001)) {
			okD = true
		}
	}
	if !okA || !okD || len(sd.Procesos) != 2 {
		t.Fatalf("procesos mal atribuidos (awk bien: %v, sleep bien: %v): %+v\nsalida:\n%s", okA, okD, sd.Procesos, out)
	}
	// El de la barra también puede quedar último: la marca de fin no se pega.
	out = correrSh(t, scriptProcesos([]uint32{a}))
	if _, err := leerSalida("MSYS", out); err != nil {
		t.Fatalf("con el de la barra al final: %v\n%s", err, out)
	}
}
