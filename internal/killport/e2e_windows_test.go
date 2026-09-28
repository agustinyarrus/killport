//go:build windows

package killport

// PRUEBAS DE PUNTA A PUNTA con el killport.exe de verdad y dobles fieles de
// lo que esta máquina no tiene (ni Docker ni distros de WSL):
//
//   - Docker Desktop: un proceso que se llama com.docker.backend.exe, tiene
//     un puerto de verdad en la tabla de Windows y sirve la API de Docker en
//     un named pipe de verdad; "detener el contenedor" le hace soltar el
//     puerto. killport lo encuentra por DOCKER_HOST (el camino de producción,
//     sin ganchos).
//   - WSL: un wslrelay.exe que tiene el puerto y lo suelta cuando muere el
//     proceso "Linux" de atrás, que es un sleep de MSYS de verdad; y un
//     wsl.exe doble que corre los scripts de killport con dash (el /bin/sh de
//     Ubuntu) y un `ss` falso que nombra a ese sleep. killport se compila con
//     la etiqueta killport_e2e, que es lo único que le permite usar ese
//     wsl.exe (un ejecutable normal no puede).
//
// Todo proceso lanzado se mata en t.Cleanup; ninguno abre ventana.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/win/pipetest"
)

const sinVentana = 0x08000000 // CREATE_NO_WINDOW

var (
	e2eOnce sync.Once
	e2eDir  string
	e2eErr  error
)

// binariosE2E compila (una vez por corrida) killport con la etiqueta de las
// pruebas y los dobles, con sus nombres de imagen.
func binariosE2E(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("-short: sin pruebas de punta a punta")
	}
	e2eOnce.Do(func() {
		e2eDir, e2eErr = os.MkdirTemp("", "killport-e2e-")
		if e2eErr != nil {
			return
		}
		builds := []struct{ out, pkg, tags string }{
			{"killport.exe", "github.com/agustinyarrus/killport", "killport_e2e"},
			{"com.docker.backend.exe", "github.com/agustinyarrus/killport/internal/killport/testdata/e2e/relayfalso", ""},
			{"wslrelay.exe", "github.com/agustinyarrus/killport/internal/killport/testdata/e2e/relayfalso", ""},
			{"wsl.exe", "github.com/agustinyarrus/killport/internal/killport/testdata/e2e/wslfalso", ""},
			{"httpfalso.exe", "github.com/agustinyarrus/killport/internal/killport/testdata/e2e/httpfalso", ""},
			{"escuchador.exe", "github.com/agustinyarrus/killport/internal/killport/testdata/e2e/escuchador", ""},
			// Los impostores: el mismo doble inofensivo, con el nombre de un
			// proceso de Windows y en una carpeta que no es System32.
			{"impostores/svchost.exe", "github.com/agustinyarrus/killport/internal/killport/testdata/e2e/relayfalso", ""},
			{"impostores/lsass.exe", "github.com/agustinyarrus/killport/internal/killport/testdata/e2e/relayfalso", ""},
		}
		for _, b := range builds {
			args := []string{"build", "-o", filepath.Join(e2eDir, filepath.FromSlash(b.out))}
			if b.tags != "" {
				args = append(args, "-tags", b.tags)
			}
			cmd := exec.Command("go", append(args, b.pkg)...)
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: sinVentana}
			if out, err := cmd.CombinedOutput(); err != nil {
				e2eErr = fmt.Errorf("go build %s: %v\n%s", b.out, err, out)
				return
			}
		}
	})
	if e2eErr != nil {
		t.Fatal(e2eErr)
	}
	return e2eDir
}

// proceso es un fixture lanzado sin ventana, con la salida en un canal de líneas.
type proceso struct {
	cmd    *exec.Cmd
	lineas chan string
	salio  chan struct{}
}

func lanzar(t *testing.T, env []string, exe string, args ...string) *proceso {
	t.Helper()
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: sinVentana}
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proceso{cmd: cmd, lineas: make(chan string, 64), salio: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			select {
			case p.lineas <- sc.Text():
			default:
			}
		}
		cmd.Wait()
		close(p.salio)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-p.salio
	})
	return p
}

// esperarLinea espera una línea con ese prefijo y devuelve el resto.
func (p *proceso) esperarLinea(t *testing.T, prefijo string) string {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case l := <-p.lineas:
			if v, ok := strings.CutPrefix(l, prefijo); ok {
				return strings.TrimSpace(v)
			}
		case <-p.salio:
			t.Fatalf("el fixture terminó sin decir %q", prefijo)
		case <-deadline:
			t.Fatalf("el fixture no dijo %q", prefijo)
		}
	}
}

func (p *proceso) vivo() bool {
	select {
	case <-p.salio:
		return false
	default:
		return true
	}
}

// killport corre el killport.exe de pruebas SIN consola (la salida va a un
// pipe: texto plano, sin preguntas) y devuelve salida y código.
func killportE2E(t *testing.T, dir string, env []string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(dir, "killport.exe"), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: sinVentana}
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if e, ok := err.(*exec.ExitError); ok {
		code = e.ExitCode()
	} else if err != nil {
		t.Fatalf("killport: %v", err)
	}
	if strings.Contains(string(out), "\x1b[") {
		t.Fatalf("sin consola no puede haber escapes de terminal:\n%s", out)
	}
	return string(out), code
}

// libre dice si el puerto se puede tomar en loopback (v4) ahora mismo.
func libre(port int) bool {
	l, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// C1 + Docker, de punta a punta: killport ve el puerto en manos de
// com.docker.backend.exe, NO lo mata (ni con --no-docker ni con -y), le
// pregunta al motor por el pipe, encuentra el contenedor y lo detiene por la
// API; el relay suelta el puerto y sigue vivo.
func TestE2EDocker(t *testing.T) {
	dir := binariosE2E(t)
	pipe := pipetest.Name("e2e-docker")
	id := strings.Repeat("0123456789abcdef", 4)
	relay := lanzar(t, nil, filepath.Join(dir, "com.docker.backend.exe"), "-modo", "docker", "-pipe", pipe, "-id", id)
	port, _ := strconv.Atoi(relay.esperarLinea(t, "PUERTO "))
	P := strconv.Itoa(port)
	env := []string{"DOCKER_HOST=npipe:" + strings.ReplaceAll(pipe, `\`, "/")}

	// 1. Describir (--json sin -y): el objetivo es el contenedor, no el relay.
	out, code := killportE2E(t, dir, env, P, "--tcp", "--json")
	var rep struct {
		Objetivos []struct {
			Tipo       string `json:"tipo"`
			Contenedor struct {
				ID, Nombre, Imagen, Proyecto, Servicio, Reinicio string
			} `json:"contenedor"`
			Via struct {
				PID     int    `json:"pid"`
				Proceso string `json:"proceso"`
			} `json:"via"`
		} `json:"objetivos"`
		SoloListado bool `json:"solo_listado"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 {
		t.Fatalf("--json: %v (código %d)\n%s", err, code, out)
	}
	if len(rep.Objetivos) != 1 || rep.Objetivos[0].Tipo != "contenedor" || rep.Objetivos[0].Contenedor.Nombre != "shop-db-1" ||
		rep.Objetivos[0].Contenedor.ID != id || rep.Objetivos[0].Via.Proceso != "com.docker.backend.exe" ||
		rep.Objetivos[0].Via.PID != relay.cmd.Process.Pid || !rep.SoloListado {
		t.Fatalf("plan en JSON = %+v\n%s", rep, out)
	}

	// 2. El listado general nombra al contenedor.
	out, _ = killportE2E(t, dir, env, "--json")
	if !strings.Contains(out, `"nombre": "shop-db-1"`) || !strings.Contains(out, `"via": "com.docker.backend.exe"`) {
		t.Fatalf("listado sin el contenedor:\n%s", out)
	}

	// 3. C1: con --no-docker no se pregunta al motor y el relay NO se toca.
	out, code = killportE2E(t, dir, env, P, "--tcp", "-y", "--no-docker")
	if code != 3 || !strings.Contains(out, "no se toca") || !relay.vivo() || libre(port) {
		t.Fatalf("--no-docker: código %d, relay vivo %v, libre %v\n%s", code, relay.vivo(), libre(port), out)
	}

	// 4. De verdad: detener el contenedor.
	out, code = killportE2E(t, dir, env, P, "--tcp", "-y")
	if code != 0 || !strings.Contains(out, "shop-db-1") || !strings.Contains(out, "se detuvo") {
		t.Fatalf("killport %s -y: código %d\n%s", P, code, out)
	}
	if got := relay.esperarLinea(t, "STOP t="); got != "3" {
		t.Fatalf("docker stop con t=%s, esperaba 3 (el --timeout por defecto)", got)
	}
	if !relay.vivo() {
		t.Fatal("killport mató a com.docker.backend.exe: eso es exactamente lo que no tiene que pasar")
	}
	if !libre(port) {
		t.Fatalf("el puerto %d sigue ocupado", port)
	}
	t.Logf("salida real de killport %s -y:\n%s", P, out)
}

// sleeperMSYS lanza un proceso MSYS que duerme y devuelve su PID MSYS, su
// hora de arranque y el PID de Windows del sleep (el exec lo cambia).
func sleeperMSYS(t *testing.T) (p *proceso, msysPID string, winPID int) {
	t.Helper()
	dash := dashOSkip(t)
	p = lanzar(t, []string{"PATH=" + gitUsrBin + ";" + os.Getenv("PATH")}, dash, "-c", "echo PID $$; exec sleep 300")
	msysPID = p.esperarLinea(t, "PID ")
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		out := correrSh(t, "cat /proc/"+msysPID+"/winpid 2>/dev/null; tr '\\n' ' ' < /proc/"+msysPID+"/stat 2>/dev/null; exit 0")
		f := strings.SplitN(out, "\n", 2)
		if len(f) == 2 {
			if comm, _, ok := parseStat(f[1]); ok && comm == "sleep" {
				winPID, _ = strconv.Atoi(strings.TrimSpace(f[0]))
				return p, msysPID, winPID
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("el sleep de MSYS no arrancó")
	return
}

// WSL de punta a punta: wslrelay tiene el puerto; killport mira dentro de la
// distro (solo la que corre), encuentra el sleep que "escucha", le manda
// SIGINT con su identidad, y el relay suelta el puerto (y sigue vivo). Después,
// un relay colgado (sin nadie detrás) se cierra él mismo.
func TestE2EWSL(t *testing.T) {
	dir := binariosE2E(t)
	sleeper, msysPID, winPID := sleeperMSYS(t)
	relay := lanzar(t, nil, filepath.Join(dir, "wslrelay.exe"), "-modo", "wsl", "-winpid", strconv.Itoa(winPID))
	port, _ := strconv.Atoi(relay.esperarLinea(t, "PUERTO "))
	P := strconv.Itoa(port)

	ssDir := t.TempDir()
	fake := "#!/bin/sh\necho 'tcp   LISTEN 0      511          0.0.0.0:" + P + "       0.0.0.0:*    users:((\"sleep\",pid=" + msysPID + ",fd=3))'\n"
	if err := os.WriteFile(filepath.Join(ssDir, "ss"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "wsl.log")
	env := []string{
		"KILLPORT_E2E_WSL_EXE=" + filepath.Join(dir, "wsl.exe"),
		"KILLPORT_E2E_DISTROS=FakeDistro",
		"KILLPORT_E2E_SS=" + ssDir,
		"KILLPORT_E2E_DASH=" + dashOSkip(t),
		"KILLPORT_E2E_LOG=" + logPath,
	}

	// 1. --wsl en el listado: la fila del relay dice quién escucha adentro.
	out, code := killportE2E(t, dir, env, "--wsl", "--json")
	if code != 0 || !strings.Contains(out, `"distro": "FakeDistro"`) || !strings.Contains(out, `"pid": `+msysPID) || !strings.Contains(out, `"via": "wslrelay.exe"`) {
		t.Fatalf("killport --wsl --json: código %d\n%s", code, out)
	}

	// 2. Cerrar el proceso Linux de verdad.
	out, code = killportE2E(t, dir, env, P, "--tcp", "-y")
	if code != 0 || !strings.Contains(out, "sleep en FakeDistro") || !strings.Contains(out, "SIGINT") {
		t.Fatalf("killport %s -y: código %d\n%s", P, code, out)
	}
	select {
	case <-sleeper.salio:
	case <-time.After(5 * time.Second):
		t.Fatal("el sleep de adentro sigue vivo")
	}
	if !relay.vivo() {
		t.Fatal("killport mató a wslrelay.exe teniendo un proceso vivo detrás")
	}
	if !libre(port) {
		t.Fatalf("el puerto %d sigue ocupado", port)
	}
	t.Logf("salida real de killport %s -y:\n%s", P, out)

	// 3. Relay colgado: nadie escucha adentro el puerto que tiene.
	zombi := lanzar(t, nil, filepath.Join(dir, "wslrelay.exe"), "-modo", "wsl", "-winpid", "0")
	pz := zombi.esperarLinea(t, "PUERTO ")
	out, code = killportE2E(t, dir, env, pz, "--tcp", "-y")
	if code != 0 || !strings.Contains(out, "relay colgado") {
		t.Fatalf("killport %s -y (zombi): código %d\n%s", pz, code, out)
	}
	select {
	case <-zombi.salio:
	case <-time.After(5 * time.Second):
		t.Fatal("el relay colgado sigue vivo")
	}
	t.Logf("salida real de killport %s -y (relay colgado):\n%s", pz, out)

	// 4. Nunca se nombró una distro que no corría.
	log, _ := os.ReadFile(logPath)
	for _, l := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		if strings.HasPrefix(l, "-d ") && !strings.HasPrefix(l, "-d FakeDistro ") {
			t.Fatalf("killport nombró otra distro: %q", l)
		}
	}
}

// Sin distros corriendo: killport NO ejecuta nada en WSL (no hay -d en el
// registro), y el relay sin nadie detrás es un relay colgado.
func TestE2EWSLSinDistros(t *testing.T) {
	dir := binariosE2E(t)
	zombi := lanzar(t, nil, filepath.Join(dir, "wslrelay.exe"), "-modo", "wsl", "-winpid", "0")
	pz := zombi.esperarLinea(t, "PUERTO ")
	logPath := filepath.Join(t.TempDir(), "wsl.log")
	env := []string{"KILLPORT_E2E_WSL_EXE=" + filepath.Join(dir, "wsl.exe"), "KILLPORT_E2E_DISTROS=", "KILLPORT_E2E_LOG=" + logPath}
	out, code := killportE2E(t, dir, env, pz, "--tcp", "--list")
	if code != 0 || !strings.Contains(out, "relay colgado") {
		t.Fatalf("killport %s --list: código %d\n%s", pz, code, out)
	}
	if !zombi.vivo() {
		t.Fatal("--list no toca nada")
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "-d ") {
		t.Fatalf("sin distros corriendo no se nombra ninguna:\n%s", log)
	}
	if n := strings.Count(strings.TrimSpace(string(log)), "\n") + 1; n != 1 {
		t.Fatalf("esperaba una sola llamada a wsl.exe (listar), hubo %d:\n%s", n, log)
	}
}

// TestMain borra los binarios de las pruebas de punta a punta al terminar
// (nada queda en disco ni corriendo).
func TestMain(m *testing.M) {
	code := m.Run()
	if e2eDir != "" {
		os.RemoveAll(e2eDir)
	}
	os.Exit(code)
}
