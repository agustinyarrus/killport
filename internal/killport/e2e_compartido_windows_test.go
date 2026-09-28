//go:build windows

package killport

// PRUEBAS DE PUNTA A PUNTA con el killport.exe de verdad y el
// fixture escuchador (testdata/e2e/escuchador): un puerto que escuchan dos
// procesos en direcciones distintas (función 10), un servidor con su carpeta
// en un repositorio de prueba (función 5) y uno que queda huérfano (función
// 2). El oráculo de "quién contesta" es una conexión de verdad. Todo proceso
// lanzado se mata en t.Cleanup; ninguno abre ventana.

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/win"
)

// escuchar lanza un escuchador en dir:puerto y devuelve su puerto y su PID.
func escuchar(t *testing.T, bin, carpeta, dir string, puerto int, etiqueta string) (int, int) {
	t.Helper()
	cmd := exec.Command(filepath.Join(bin, "escuchador.exe"), "-dir", dir, "-puerto", strconv.Itoa(puerto), "-etiqueta", etiqueta)
	cmd.Dir = carpeta
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: sinVentana}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	var buf [64]byte
	n, _ := out.Read(buf[:])
	m := regexp.MustCompile(`PUERTO (\d+) PID (\d+)`).FindStringSubmatch(string(buf[:n]))
	if m == nil {
		t.Fatalf("el escuchador dijo %q", buf[:n])
	}
	p, _ := strconv.Atoi(m[1])
	pid, _ := strconv.Atoi(m[2])
	return p, pid
}

// quienContesta se conecta de verdad y devuelve la etiqueta de quien atendió
// ("" si nadie: conexión rechazada).
func quienContesta(addr string) string {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return ""
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 32)
	n, _ := c.Read(b)
	return string(b[:n])
}

func vive(pid int) bool {
	h, err := syscall.OpenProcess(0x1000|0x00100000, false, uint32(pid)) // QUERY_LIMITED | SYNCHRONIZE
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	ev, _ := syscall.WaitForSingleObject(h, 0)
	return ev == syscall.WAIT_TIMEOUT
}

// reporteT3 es lo que se lee del JSON del reporte en estas pruebas.
type reporteT3 struct {
	Objetivos []struct {
		PID         int      `json:"pid"`
		Direcciones []string `json:"direcciones"`
		Alcance     string   `json:"alcance"`
		Huerfano    string   `json:"huerfano"`
		Proyecto    *struct {
			Raiz, Nombre, Rama, Marcador, Repo string
		} `json:"proyecto"`
	} `json:"objetivos"`
	AlMargen []struct {
		PID int `json:"pid"`
	} `json:"al_margen"`
	Puertos []struct {
		Estado   string `json:"estado"`
		Motivo   string `json:"motivo"`
		AlMargen *struct {
			PID int `json:"pid"`
		} `json:"al_margen"`
	} `json:"puertos"`
}

func leerReporte(t *testing.T, out string) reporteT3 {
	t.Helper()
	var r reporteT3
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("JSON inválido: %v\n%s", err, out)
	}
	return r
}

// Función 10 de punta a punta: A en [::1]:P y B en 0.0.0.0:P, los dos sin
// error. localhost:P apunta a A (el que contesta localhost, medido con una
// conexión de verdad) y 127.0.0.1:P a B; cerrar localhost:P deja a B vivo y
// localhost pasa a llegarle a él.
func TestE2ELocalhostApuntaAUno(t *testing.T) {
	bin := binariosE2E(t)
	aqui := t.TempDir()
	p, pidA := escuchar(t, bin, aqui, "::1", 0, "A")
	_, pidB := escuchar(t, bin, aqui, "0.0.0.0", p, "B")
	P := strconv.Itoa(p)

	// El oráculo: quién contesta de verdad.
	if a, b := quienContesta("[::1]:"+P), quienContesta("127.0.0.1:"+P); a != "A" || b != "B" {
		t.Fatalf("oráculo: [::1] → %q, 127.0.0.1 → %q", a, b)
	}

	// El listado marca el puerto y dice a quién llega cada destino.
	out, code := killportE2E(t, bin, nil, "--json")
	var lst struct {
		Escuchando []struct {
			Puerto      int      `json:"puerto"`
			PID         int      `json:"pid"`
			Compartido  bool     `json:"compartido"`
			Alcance     string   `json:"alcance"`
			Direcciones []string `json:"direcciones"`
		} `json:"escuchando"`
		Compartidos []struct {
			Puerto    int `json:"puerto"`
			Contestan []struct {
				Destinos []string `json:"destinos"`
				PIDs     []int    `json:"pids"`
			} `json:"contestan"`
		} `json:"compartidos"`
	}
	if err := json.Unmarshal([]byte(out), &lst); err != nil || code != 0 {
		t.Fatalf("listado: %v (código %d)\n%s", err, code, out)
	}
	marcadas := 0
	for _, f := range lst.Escuchando {
		switch {
		case f.Puerto == p && f.PID == pidA && f.Compartido && f.Alcance == "esta_pc" && strings.Join(f.Direcciones, ",") == "::1":
			marcadas++
		case f.Puerto == p && f.PID == pidB && f.Compartido && f.Alcance == "red" && strings.Join(f.Direcciones, ",") == "0.0.0.0":
			marcadas++
		}
	}
	lh := -1
	for _, c := range lst.Compartidos {
		for _, x := range c.Contestan {
			if c.Puerto == p && len(x.Destinos) > 0 && x.Destinos[0] == "localhost" && len(x.PIDs) == 1 {
				lh = x.PIDs[0]
			}
		}
	}
	if marcadas != 2 || lh != pidA {
		t.Fatalf("listado: %d filas marcadas, localhost → %d (A es %d)\n%s", marcadas, lh, pidA, out)
	}

	// Cada dirección apunta a uno.
	for _, c := range []struct {
		pedido        string
		quien, margen int
	}{{"localhost:" + P, pidA, pidB}, {"127.0.0.1:" + P, pidB, pidA}, {"[::1]:" + P, pidA, pidB}, {"0.0.0.0:" + P, pidB, pidA}} {
		out, code = killportE2E(t, bin, nil, c.pedido, "--json")
		r := leerReporte(t, out)
		if code != 0 || len(r.Objetivos) != 1 || r.Objetivos[0].PID != c.quien || len(r.AlMargen) != 1 || r.AlMargen[0].PID != c.margen {
			t.Fatalf("%s: objetivos %+v, al margen %+v (código %d)\n%s", c.pedido, r.Objetivos, r.AlMargen, code, out)
		}
	}

	// Un nombre no se resuelve: apunta a todo el puerto, y el JSON lo dice.
	out, code = killportE2E(t, bin, nil, "api.local:"+P, "--json")
	var sr struct {
		SinResolver []string `json:"sin_resolver"`
		Objetivos   []struct {
			PID int `json:"pid"`
		} `json:"objetivos"`
	}
	if err := json.Unmarshal([]byte(out), &sr); err != nil || code != 0 || strings.Join(sr.SinResolver, ",") != "api.local" || len(sr.Objetivos) != 2 {
		t.Fatalf("api.local:%s --json: código %d\n%s", P, code, out)
	}

	// De verdad: cerrar localhost:P. Se cierra a A, y localhost NO queda
	// libre: ahora le llega a B. Ni "libre" ni "1 de 1" ni exit 0 (X-14).
	out, code = killportE2E(t, bin, nil, "localhost:"+P, "--tcp", "-y")
	if code != 3 || strings.Contains(out, "localhost:"+P+" libre") || !strings.Contains(out, "Puertos libres  0 de 1") ||
		!strings.Contains(out, "localhost:"+P+" ya no llega a escuchador.exe (PID "+strconv.Itoa(pidA)+")") ||
		!strings.Contains(out, "ahora localhost (por 127.0.0.1) llega a escuchador.exe (PID "+strconv.Itoa(pidB)+")") {
		t.Fatalf("killport localhost:%s -y: código %d\n%s", P, code, out)
	}
	for i := 0; i < 50 && vive(pidA); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if vive(pidA) || !vive(pidB) {
		t.Fatalf("A vivo %v, B vivo %v: tenía que cerrar A y NO a B", vive(pidA), vive(pidB))
	}
	if a, b := quienContesta("[::1]:"+P), quienContesta("127.0.0.1:"+P); a != "" || b != "B" {
		t.Fatalf("después: [::1] → %q, 127.0.0.1 → %q", a, b)
	}
	t.Logf("salida real de killport localhost:%s -y:\n%s", P, out)
}

// repoDePrueba arma un repositorio git de prueba: web\package.json con un
// nombre y .git\HEAD con una rama.
func repoDePrueba(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "tienda")
	for p, s := range map[string]string{
		`.git\HEAD`:        "ref: refs/heads/e2e\n",
		`web\package.json`: `{"name": "e2e-kp"}`,
	} {
		full := filepath.Join(repo, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// Funciones 5 y 2 de punta a punta: el proyecto de un servidor (su carpeta,
// el nombre de package.json, la rama de .git\HEAD), --project, -n, y un
// huérfano de verdad (su padre termina) que --orphans encuentra y cierra.
func TestE2EProyectoYHuerfano(t *testing.T) {
	bin := binariosE2E(t)
	repo := repoDePrueba(t)
	web := filepath.Join(repo, "web")
	p, pid := escuchar(t, bin, web, "127.0.0.1", 0, "web")
	P := strconv.Itoa(p)

	out, code := killportE2E(t, bin, nil, P, "--tcp", "--json")
	r := leerReporte(t, out)
	if code != 0 || len(r.Objetivos) != 1 || r.Objetivos[0].Proyecto == nil {
		t.Fatalf("killport %s --json: código %d\n%s", P, code, out)
	}
	pr := r.Objetivos[0].Proyecto
	if pr.Nombre != "e2e-kp" || pr.Rama != "e2e" || pr.Marcador != "package.json" || !strings.EqualFold(pr.Raiz, web) || !strings.EqualFold(pr.Repo, repo) {
		t.Fatalf("proyecto = %+v", pr)
	}
	out, _ = killportE2E(t, bin, nil, P, "--tcp", "--list")
	if !strings.Contains(out, "proyecto: tienda › web (package.json «e2e-kp», rama e2e)") || !strings.Contains(out, "navaja ") {
		t.Fatalf("bloque sin el proyecto o sin la versión:\n%s", out)
	}

	// Un huérfano: el padre lo lanza desprendido y termina.
	marca := filepath.Join(t.TempDir(), "hijo.marca")
	padre := exec.Command(filepath.Join(bin, "escuchador.exe"), "-huerfano", "-dir", "127.0.0.1", "-etiqueta", "hijo", "-marca", marca)
	padre.Dir = repo
	padre.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: sinVentana}
	salida, err := padre.Output()
	m := regexp.MustCompile(`HIJO (\d+)`).FindStringSubmatch(string(salida))
	if err != nil || m == nil {
		t.Fatalf("el padre dijo %q (%v)", salida, err)
	}
	hijo, _ := strconv.Atoi(m[1])
	// La limpieza lo cierra por IDENTIDAD (PID y hora de creación), no por
	// PID pelado: si killport ya lo cerró, su número puede ser de otro.
	var nacido time.Time
	if procs, err := win.AllProcesses(); err == nil {
		for _, p := range procs {
			if p.PID == uint32(hijo) {
				nacido = p.Created
			}
		}
	}
	t.Cleanup(func() {
		if h, err := win.OpenForKill(uint32(hijo), nacido); err == nil && !nacido.IsZero() {
			h.Terminate(1)
			h.Close()
		}
	})
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(marca); err == nil && strings.HasPrefix(string(b), "PUERTO ") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	out, code = killportE2E(t, bin, nil, "--orphans", "--project", repo, "--json")
	r = leerReporte(t, out)
	if code != 0 || len(r.Objetivos) != 1 || r.Objetivos[0].PID != hijo || !strings.Contains(r.Objetivos[0].Huerfano, "ya no existe") {
		t.Fatalf("--orphans --project: código %d\n%s", code, out)
	}
	// El servidor de web tiene padre vivo (esta prueba): no es huérfano.
	out, _ = killportE2E(t, bin, nil, "--orphans", "--project", web, "--json")
	if r := leerReporte(t, out); len(r.Objetivos) != 0 {
		t.Fatalf("un proceso con padre vivo no es huérfano:\n%s", out)
	}
	// -n y --project se intersecan.
	out, code = killportE2E(t, bin, nil, "-n", "escuchador", "--project", web, "--json")
	if r := leerReporte(t, out); code != 0 || len(r.Objetivos) != 1 || r.Objetivos[0].PID != pid {
		t.Fatalf("-n escuchador --project web: código %d\n%s", code, out)
	}
	out, code = killportE2E(t, bin, nil, "-n", "no-existe", "--project", repo)
	if code != 0 || !strings.Contains(out, "No hay nada que cerrar") {
		t.Fatalf("sin nada elegido: código %d\n%s", code, out)
	}

	// De verdad: cerrar al huérfano, y a nadie más.
	out, code = killportE2E(t, bin, nil, "--orphans", "--project", repo, "-y")
	for i := 0; i < 50 && vive(hijo); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if code != 0 || vive(hijo) || !vive(pid) {
		t.Fatalf("--orphans -y: código %d, huérfano vivo %v, web vivo %v\n%s", code, vive(hijo), vive(pid), out)
	}
	t.Logf("salida real de killport --orphans --project … -y:\n%s", out)
}
