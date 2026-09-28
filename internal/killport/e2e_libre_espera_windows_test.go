//go:build windows

package killport

// PRUEBAS DE PUNTA A PUNTA de --free y --wait con el killport.exe de verdad:
//
//   - --free contra la tabla de esta máquina, con la trampa clásica puesta (un
//     servidor en 127.0.0.1 que un bind común a 0.0.0.0 no ve); el oráculo es
//     atar de verdad cada puerto sugerido en las cuatro direcciones.
//   - --wait: un escuchador que aparece DESPUÉS, solo en [::1] (a 127.0.0.1 no
//     contesta nadie: la trampa de wait-on); vence; --new con uno viejo; y el
//     Ctrl+C, que se le manda con el ayudante de consola del propio killport.
//   - --wait-free: el dueño se muere y la espera se entera al instante.
//
// Todo proceso lanzado se mata en t.Cleanup; ninguno abre ventana.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
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

// puertoLibreDelSistema elige un puerto libre de verdad para una prueba.
// No se usa el que da el sistema para TCP (bind al 0): sale del rango
// dinámico, donde los clientes efímeros UDP (chrome, QUIC) y los bloques de
// WinNAT viven, y killport sin --tcp mira los dos protocolos. Se prueba al
// azar entre 20000 y 44999: nadie lo tiene en la tabla, en ningún protocolo,
// y se deja atar en TCP y UDP, v4 y v6.
func puertoLibreDelSistema(t *testing.T) int {
	t.Helper()
	var ultimo error
	for range 200 {
		p := 20000 + rand.IntN(25000)
		if ultimo = libreEnTodo(p); ultimo == nil {
			return p
		}
	}
	t.Fatalf("no encontré un puerto libre en TCP y en UDP (el último: %v)", ultimo)
	return 0
}

// libreEnTodo: nadie lo tiene en la tabla (en ningún protocolo) y se deja
// atar en TCP y UDP, v4 y v6 (un bloque de WinNAT no figura en la tabla).
func libreEnTodo(p int) error {
	socks, err := win.AllSockets()
	if err != nil {
		return err
	}
	for _, s := range socks {
		if int(s.Local.Port()) == p {
			return fmt.Errorf("%d: lo tiene el PID %d", p, s.PID)
		}
	}
	if err := atable(p); err != nil {
		return err
	}
	for _, red := range []struct{ net, dir string }{{"udp4", "0.0.0.0"}, {"udp6", "[::]"}} {
		c, err := net.ListenPacket(red.net, red.dir+":"+strconv.Itoa(p))
		if err != nil {
			return err
		}
		c.Close()
	}
	return nil
}

// atable dice si el puerto se puede atar en las cuatro direcciones (TCP).
func atable(p int) error {
	for _, dir := range []string{"127.0.0.1", "0.0.0.0", "[::]", "[::1]"} {
		red := "tcp4"
		if strings.HasPrefix(dir, "[") {
			red = "tcp6"
		}
		l, err := net.Listen(red, dir+":"+strconv.Itoa(p))
		if err != nil {
			return err
		}
		l.Close()
	}
	return nil
}

// killportAparte corre killport en segundo plano, sin consola, y devuelve un
// canal que entrega salida y código al terminar.
type corrida struct {
	cmd  *exec.Cmd
	out  bytes.Buffer
	fin  chan struct{}
	code int
	t0   time.Time
	dur  time.Duration
}

func killportAparte(t *testing.T, dir string, flags uint32, args ...string) *corrida {
	t.Helper()
	c := &corrida{fin: make(chan struct{})}
	c.cmd = exec.Command(filepath.Join(dir, "killport.exe"), args...)
	c.cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
	c.cmd.Stdout, c.cmd.Stderr = &c.out, &c.out
	c.t0 = time.Now()
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		err := c.cmd.Wait()
		c.dur = time.Since(c.t0)
		if e, ok := err.(*exec.ExitError); ok {
			c.code = e.ExitCode()
		}
		close(c.fin)
	}()
	t.Cleanup(func() {
		c.cmd.Process.Kill()
		<-c.fin
	})
	return c
}

func (c *corrida) esperar(t *testing.T, plazo time.Duration) {
	t.Helper()
	select {
	case <-c.fin:
	case <-time.After(plazo):
		t.Fatalf("killport no terminó en %v:\n%s", plazo, c.out.String())
	}
}

// --free, con la trampa: un servidor en 127.0.0.1:P. Un sondeo común a
// 0.0.0.0:P diría "libre"; killport no puede sugerirlo.
func TestE2ELibre(t *testing.T) {
	dir := binariosE2E(t)
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	P := l.Addr().(*net.TCPAddr).Port
	out, code := killportE2E(t, dir, nil, "--free", strconv.Itoa(P), "--count", "3")
	if code != 0 {
		t.Fatalf("código %d\n%s", code, out)
	}
	lineas := strings.Fields(out)
	if len(lineas) != 3 {
		t.Fatalf("sin consola tienen que salir SOLO los tres números:\n%q", out)
	}
	for _, s := range lineas {
		p, err := strconv.Atoi(s)
		if err != nil || p == P || p < P {
			t.Fatalf("sugirió %q (el tomado es el %d)", s, P)
		}
		if err := atable(p); err != nil {
			t.Fatalf("sugirió el %d y no se deja atar: %v", p, err)
		}
	}
	// El JSON cuenta por qué salteó el P: lo escucha esta prueba.
	out, code = killportE2E(t, dir, nil, "--free", strconv.Itoa(P), "--json")
	var doc jsonLibre
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != 0 {
		t.Fatalf("--json: %v (código %d)\n%s", err, code, out)
	}
	if len(doc.Salteados) == 0 || doc.Salteados[0].Inicio != uint16(P) || doc.Salteados[0].Motivo != "tomado" || doc.Salteados[0].PID != uint32(os.Getpid()) {
		t.Fatalf("salteados: %+v", doc.Salteados)
	}
}

// Los umbrales de tiempo de estas pruebas separan lo correcto de lo que se
// quiere descartar y van lejos de los dos. Lo descartado es esperar el plazo
// entero, así que la cota es la mitad del plazo: cubre lanzar el .exe,
// escribir y salir en una máquina cargada, y sigue lejos del plazo. Lo fino
// (enterarse a lo sumo 200 ms después, o al instante cuando muere el dueño)
// lo prueba reaper con reloj falso: TestWaitPortsEscucha y
// TestWaitPortsLibreDespiertaConLaSalida.
const plazoE2E = 20 * time.Second

// --wait: el servidor aparece después, SOLO en [::1]. Conectarse a
// 127.0.0.1 no llega (la trampa de wait-on); mirar la tabla, sí.
func TestE2EEsperaEscucha(t *testing.T) {
	dir := binariosE2E(t)
	Q := puertoLibreDelSistema(t)
	c := killportAparte(t, dir, sinVentana, strconv.Itoa(Q), "--wait", plazoE2E.String())
	time.Sleep(600 * time.Millisecond)
	select {
	case <-c.fin:
		t.Fatalf("terminó antes de que escuche nadie (código %d):\n%s", c.code, c.out.String())
	default:
	}
	_, pid := escuchar(t, dir, dir, "::1", Q, "srv")
	escucha := time.Now()
	c.esperar(t, 10*time.Second)
	latencia := time.Since(escucha)
	out := c.out.String()
	if c.code != 0 || !strings.Contains(out, strconv.Itoa(Q)+" escucha") || !strings.Contains(out, "escuchador.exe (PID "+strconv.Itoa(pid)) || !strings.Contains(out, "en [::1]") {
		t.Fatalf("código %d:\n%s", c.code, out)
	}
	if latencia >= plazoE2E/2 {
		t.Fatalf("tardó %v en enterarse (el plazo era %v)", latencia, plazoE2E)
	}
	if quienContesta("127.0.0.1:"+strconv.Itoa(Q)) != "" {
		t.Fatal("el oráculo: a 127.0.0.1 no tendría que contestar nadie")
	}
	t.Logf("se enteró %v después de que el escuchador anunció su puerto:\n%s", latencia, out)
}

// --wait vence; y con --new, un servidor que ya estaba no cuenta.
func TestE2EEsperaVenceYNuevo(t *testing.T) {
	dir := binariosE2E(t)
	Q := puertoLibreDelSistema(t)
	out, code := killportE2E(t, dir, nil, strconv.Itoa(Q), "--wait", "1s")
	if code != 3 || !strings.Contains(out, "Venció el plazo") {
		t.Fatalf("código %d:\n%s", code, out)
	}
	// Que venza A TIEMPO se mide con el reloj de killport (el "ms" del JSON),
	// no desde afuera: el reloj de la prueba suma lanzar el .exe y esperar que
	// salga, que en una máquina cargada son segundos. Ni antes del plazo (con
	// la tolerancia de medir el arranque de la espera un instante después de
	// fijarlo) ni mucho después.
	out, code = killportE2E(t, dir, nil, strconv.Itoa(Q), "--wait", "1s", "--json")
	var vencida jsonEspera
	if err := json.Unmarshal([]byte(out), &vencida); err != nil || code != 3 || vencida.Resultado != "vencio" || vencida.PlazoMS != 1000 ||
		vencida.MS < 900 || vencida.MS > 3000 {
		t.Fatalf("--wait 1s --json: código %d, %v\n%s", code, err, out)
	}
	R, _ := escuchar(t, dir, dir, "127.0.0.1", 0, "viejo")
	out, code = killportE2E(t, dir, nil, strconv.Itoa(R), "--wait", "1s", "--new")
	if code != 3 || !strings.Contains(out, "nada nuevo escucha el "+strconv.Itoa(R)) {
		t.Fatalf("--new con uno viejo: código %d:\n%s", code, out)
	}
	out, code = killportE2E(t, dir, nil, strconv.Itoa(R), "--wait", "1s", "--json")
	var doc jsonEspera
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != 0 || doc.Resultado != "cumplido" || !doc.Puertos[0].YaEstaba {
		t.Fatalf("sin --new, ya escuchaba: código %d, %v\n%s", code, err, out)
	}
}

// --wait-free: la salida del dueño despierta la espera al instante (Wait
// sobre su handle), no al próximo sondeo.
func TestE2EEsperaLibre(t *testing.T) {
	dir := binariosE2E(t)
	cmd := exec.Command(filepath.Join(dir, "escuchador.exe"), "-dir", "0.0.0.0", "-puerto", "0", "-etiqueta", "x")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: sinVentana}
	pipe, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	var buf [64]byte
	n, _ := pipe.Read(buf[:])
	campos := strings.Fields(string(buf[:n]))
	if len(campos) < 2 {
		t.Fatalf("el escuchador dijo %q", buf[:n])
	}
	Q := campos[1]
	c := killportAparte(t, dir, sinVentana, Q, "--wait-free", plazoE2E.String())
	time.Sleep(800 * time.Millisecond)
	muerte := time.Now()
	cmd.Process.Kill()
	c.esperar(t, 10*time.Second)
	latencia := time.Since(muerte)
	out := c.out.String()
	if c.code != 0 || !strings.Contains(out, Q+" lo ocupa escuchador.exe") || !strings.Contains(out, Q+" libre") {
		t.Fatalf("código %d:\n%s", c.code, out)
	}
	if latencia >= plazoE2E/2 {
		t.Fatalf("tardó %v en enterarse de que se liberó (el plazo era %v)", latencia, plazoE2E)
	}
	t.Logf("se enteró %v después de la muerte del dueño:\n%s", latencia, out)
}

// --json sale con el mismo código que el texto: el 445 (SMB, del kernel) no se
// toca y sale 3 en los dos; un puerto libre, 0 en los dos. Y el JSON trae la
// sugerencia del próximo puerto libre.
func TestE2EJSONMismoCodigo(t *testing.T) {
	dir := binariosE2E(t)
	if !escuchaTCP(445) {
		t.Skip("el 445 no escucha en esta máquina (compartir archivos apagado)")
	}
	libre := strconv.Itoa(puertoLibreDelSistema(t))
	for _, c := range []struct {
		args []string
		code int
	}{{[]string{"445"}, 3}, {[]string{"445", "--json"}, 3}, {[]string{libre}, 0}, {[]string{libre, "--json"}, 0}} {
		out, code := killportE2E(t, dir, nil, c.args...)
		if code != c.code {
			t.Errorf("killport %s: código %d, esperaba %d\n%s", strings.Join(c.args, " "), code, c.code, out)
		}
	}
	out, _ := killportE2E(t, dir, nil, "445", "--json")
	var doc struct {
		Puertos []struct {
			Estado      string `json:"estado"`
			Alternativa int    `json:"alternativa"`
		} `json:"puertos"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Puertos) != 1 || doc.Puertos[0].Estado != "no_se_toca" || doc.Puertos[0].Alternativa <= 445 {
		t.Fatalf("445 --json: %v\n%s", err, out)
	}
}

// escuchaTCP dice si alguien escucha ese puerto TCP (en la dirección que sea).
func escuchaTCP(p int) bool {
	for _, dir := range []string{"127.0.0.1", "[::1]"} {
		if c, err := net.DialTimeout("tcp", dir+":"+strconv.Itoa(p), time.Second); err == nil {
			c.Close()
			return true
		}
	}
	return false
}

// Ctrl+C a una espera: sale 130. El Ctrl+C se lo manda el ayudante de consola
// del propio killport (sin consola propia, se engancha a la del que espera).
func TestE2EEsperaCancelada(t *testing.T) {
	dir := binariosE2E(t)
	Q := puertoLibreDelSistema(t)
	const nuevaConsola = 0x00000010 // CREATE_NEW_CONSOLE (oculta: HideWindow)
	// TRAMPA: si go test corre bajo Git Bash, este proceso tiene el Ctrl+C
	// IGNORADO (SetConsoleCtrlHandler(NULL, TRUE)) y los hijos lo heredan:
	// el Ctrl+C llegaría y nadie lo atendería. Se reactiva antes de lanzar al
	// que espera, para que lo herede activo, como el de una terminal común.
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")
	proc.Call(0, 0)
	c := killportAparte(t, dir, nuevaConsola, strconv.Itoa(Q), "--wait", "30s")
	time.Sleep(800 * time.Millisecond)
	pid := strconv.Itoa(c.cmd.Process.Pid)
	ayu := exec.Command(filepath.Join(dir, "killport.exe"), "--ctrl-c-helper", pid, "--allow", pid+":0")
	ayu.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: detachedProcess}
	dijo, err := ayu.Output()
	if err != nil || strings.TrimSpace(string(dijo)) != "SENT" {
		t.Fatalf("el ayudante dijo %q (%v)", dijo, err)
	}
	c.esperar(t, 5*time.Second)
	if c.code != 130 || !strings.Contains(c.out.String(), "Cancelaste") {
		t.Fatalf("código %d:\n%s", c.code, c.out.String())
	}
}
