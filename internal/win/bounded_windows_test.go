//go:build windows

package win

// Pruebas del ejecutor acotado contra procesos REALES. El hijo es el mismo
// binario de pruebas relanzado en un "modo" (TestMain): así cada prueba
// controla con precisión qué hace el hijo —informar si tiene consola, lanzar
// un nieto y quedarse, inundar la salida— sin depender de programas externos.
// Todo lo que se lanza termina dentro de la prueba (lo mata el Job, que es
// justamente lo que se prueba) y se verifica que no quede nada vivo.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

const modoHijo = "KILLPORT_WIN_HIJO"

var procGetConsoleWindow = modkernel32.NewProc("GetConsoleWindow")

func TestMain(m *testing.M) {
	switch os.Getenv(modoHijo) {
	case "":
		os.Exit(m.Run())
	case "informe":
		hwnd, _ := call(procGetConsoleWindow)
		fmt.Printf("consola=%v extra=%s args=%s\n", hwnd != 0, os.Getenv("KILLPORT_EXTRA"), strings.Join(os.Args[1:], "|"))
		os.Exit(7)
	case "nieto":
		// Lanza un nieto SIN ningún cuidado (como haría un programa ajeno),
		// informa su PID y se queda esperando: solo el Job puede con los dos.
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), modoHijo+"=dormir")
		if err := cmd.Start(); err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Printf("nieto=%d\n", cmd.Process.Pid)
		time.Sleep(time.Minute)
		os.Exit(0)
	case "dormir":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "inundar":
		chunk := []byte(strings.Repeat("x", 64<<10))
		for i := 0; i < 256; i++ { // 16 MiB
			os.Stdout.Write(chunk)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	case "ruido":
		os.Stderr.Write([]byte(strings.Repeat("e", 200<<10)))
		fmt.Print("hola")
		os.Exit(0)
	}
	os.Exit(2)
}

// hijo corre el binario de pruebas en un modo, acotado.
func hijo(t *testing.T, ctx context.Context, modo string, b Bounded, args ...string) (BoundedResult, error) {
	t.Helper()
	b.Env = append(b.Env, modoHijo+"="+modo)
	return RunBounded(ctx, os.Args[0], append([]string{"-test.run=^$"}, args...), b)
}

// muerto dice si el proceso pid ya no corre (o nunca existió).
func muerto(t *testing.T, pid uint32) bool {
	t.Helper()
	h, err := openProcess(synchronize, pid)
	if errors.Is(err, ErrGone) {
		return true
	}
	if err != nil {
		t.Fatalf("no pude abrir %d: %v", pid, err)
	}
	defer closeHandle(h)
	return waitOne(h, 2000) == waitObject0
}

// El caso normal: salida, código, entorno extra, argumentos con espacios, y
// el hijo SIN consola (CREATE_NO_WINDOW: ni ventana ni consola heredada).
func TestRunBoundedInforme(t *testing.T) {
	res, err := hijo(t, context.Background(), "informe", Bounded{Timeout: 20 * time.Second, Env: []string{"KILLPORT_EXTRA=ñandú"}}, "a b", `c"d`)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("código = %d, esperaba 7", res.ExitCode)
	}
	got := strings.TrimSpace(string(res.Stdout))
	if want := `consola=false extra=ñandú args=-test.run=^$|a b|c"d`; got != want {
		t.Fatalf("salida = %q\nesperaba   %q", got, want)
	}
}

// El plazo vence con un hijo que lanzó un nieto: mueren LOS DOS (el nieto no
// sabe nada del Job; lo alcanza por haber nacido adentro).
func TestRunBoundedPlazoMataElArbol(t *testing.T) {
	start := time.Now()
	res, err := hijo(t, context.Background(), "nieto", Bounded{Timeout: 3 * time.Second})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("esperaba ErrTimeout, dio %v (salida %q)", err, res.Stdout)
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("tardó %v en cortar un plazo de 3 s", d)
	}
	line := strings.TrimSpace(string(res.Stdout))
	v, ok := strings.CutPrefix(line, "nieto=")
	if !ok {
		t.Fatalf("el hijo no informó al nieto: %q", line)
	}
	pid, _ := strconv.Atoi(v)
	if !muerto(t, uint32(pid)) {
		exec.Command("taskkill", "/F", "/PID", v).Run()
		t.Fatalf("el nieto %d sobrevivió al Job", pid)
	}
}

// Cancelar el contexto (el Ctrl+C del usuario) también mata el árbol, al toque.
func TestRunBoundedCancelacion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(500 * time.Millisecond); cancel() }()
	start := time.Now()
	res, err := hijo(t, ctx, "nieto", Bounded{Timeout: time.Minute})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("esperaba context.Canceled, dio %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("tardó %v en cortar", d)
	}
	if v, ok := strings.CutPrefix(strings.TrimSpace(string(res.Stdout)), "nieto="); ok {
		pid, _ := strconv.Atoi(v)
		if !muerto(t, uint32(pid)) {
			exec.Command("taskkill", "/F", "/PID", v).Run()
			t.Fatalf("el nieto %d sobrevivió a la cancelación", pid)
		}
	}
}

// Un hijo que escribe sin fin: se corta en el tope, se lo mata y es error,
// sin esperar al plazo.
func TestRunBoundedTopeDeSalida(t *testing.T) {
	start := time.Now()
	res, err := hijo(t, context.Background(), "inundar", Bounded{Timeout: 30 * time.Second, MaxOutput: 1 << 20})
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("esperaba ErrOutputLimit, dio %v", err)
	}
	if len(res.Stdout) != 1<<20 {
		t.Fatalf("guardó %d bytes, esperaba exactamente el tope", len(res.Stdout))
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("tardó %v: debía cortar apenas pasó el tope, no al plazo", d)
	}
}

// La salida de errores se recorta en silencio (no es error) y la estándar llega entera.
func TestRunBoundedRecortaStderr(t *testing.T) {
	res, err := hijo(t, context.Background(), "ruido", Bounded{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "hola" || len(res.Stderr) != boundedStderrMax {
		t.Fatalf("stdout %q, stderr %d bytes", res.Stdout, len(res.Stderr))
	}
}

// Un ejecutable que no existe es un error al lanzar, no un cuelgue.
func TestRunBoundedNoExiste(t *testing.T) {
	if _, err := RunBounded(context.Background(), `C:\no\existe\nada.exe`, nil, Bounded{Timeout: 5 * time.Second}); err == nil {
		t.Fatal("esperaba error")
	}
}

// El comando del sistema por ruta absoluta (netsh, el que usa killport).
func TestRunBoundedNetsh(t *testing.T) {
	res, err := RunBounded(context.Background(), SystemExe("netsh.exe"), []string{"interface", "ipv4", "show", "excludedportrange", "protocol=tcp"}, Bounded{Timeout: 20 * time.Second})
	if err != nil || res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "----") {
		t.Fatalf("netsh: %v código %d salida %q", err, res.ExitCode, res.Stdout)
	}
}

// Layout de JOBOBJECT_EXTENDED_LIMIT_INFORMATION en x64 (winnt.h): 64 + 48 + 32.
func TestJobLimitLayout(t *testing.T) {
	if s := unsafe.Sizeof(jobBasicLimit{}); s != 64 {
		t.Fatalf("JOBOBJECT_BASIC_LIMIT_INFORMATION mide %d, esperaba 64", s)
	}
	if s := unsafe.Sizeof(jobExtendedLimit{}); s != 144 {
		t.Fatalf("JOBOBJECT_EXTENDED_LIMIT_INFORMATION mide %d, esperaba 144", s)
	}
	if o := unsafe.Offsetof(jobBasicLimit{}.LimitFlags); o != 16 {
		t.Fatalf("LimitFlags en %d, esperaba 16", o)
	}
}

// X-25: un programa que no se pudo lanzar se dice con su nombre (no la ruta
// dos veces), sin "fork/exec", con el error de Windows y su número; y se
// sigue pudiendo preguntar por el error de fondo.
func TestLanzarErrorLegible(t *testing.T) {
	_, err := RunBounded(context.Background(), `C:\no\existe\nada.exe`, nil, Bounded{Timeout: time.Second, MaxOutput: 1 << 10})
	if err == nil {
		t.Fatal("lanzó algo que no existe")
	}
	if msg := err.Error(); strings.Contains(msg, "fork/exec") || strings.Count(msg, "nada.exe") != 1 || !strings.Contains(msg, "no pude lanzar nada.exe") || !strings.Contains(msg, "(error ") {
		t.Fatalf("mensaje = %q", msg)
	}
	var le *LaunchError
	if !errors.As(err, &le) || !errors.Is(err, syscall.ERROR_PATH_NOT_FOUND) && !errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("el error de fondo se perdió: %#v", err)
	}
	if got := motivoLanzar(syscall.Errno(1816)); !strings.Contains(got, "cuota insuficiente (error 1816") {
		t.Fatalf("1816 = %q", got)
	}
}

// waitOne espera un solo handle con timeout en ms (WaitForMultipleObjects de a
// uno). Solo lo usan las pruebas.
func waitOne(h uintptr, ms uint32) uint32 {
	arr := [1]uintptr{h}
	r, _ := call(procWaitForMultipleObjects, 1, uintptr(unsafe.Pointer(&arr[0])), 0, uintptr(ms))
	return uint32(r)
}
