//go:build windows

package killport

// PRUEBAS DE PUNTA A PUNTA de lo que killport ve DETRÁS de Windows, con el
// killport.exe de verdad contra el sistema de verdad, sin administrador y sin
// cambiar nada de la máquina:
//
//   - http.sys: un proceso propio le registra una URL al servidor HTTP del
//     kernel. La tabla de sockets dice "System (PID 4)"; killport tiene que
//     decir quién es, cerrarlo a él y dejar al kernel en paz.
//   - impostores: un programa inofensivo que se llama svchost.exe (o
//     lsass.exe) y corre desde una carpeta cualquiera. No se oculta, no se
//     protege, y se dice por qué.
//   - puertos sin dueño: los rangos que esta máquina tenga excluidos. Un bloque
//     de WinNAT se informa reservado, con sus salidas; una exclusión
//     administrada, libre. `--reserve` muestra su plan y no ejecuta nada.
//
// Todo proceso lanzado se mata en t.Cleanup; ninguno abre ventana.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/win"
)

// reporteE2E es lo que estas pruebas leen del documento --json de un puerto.
type reporteE2E struct {
	Puertos []struct {
		Puerto    uint16 `json:"puerto"`
		Estado    string `json:"estado"`
		Motivo    string `json:"motivo"`
		Reservado *struct {
			Inicio, Fin  uint16
			Administrada bool
		} `json:"reservado"`
		Remedios []struct{ Comando, Efecto string } `json:"remedios"`
	} `json:"puertos"`
	Objetivos []struct {
		Tipo     string   `json:"tipo"`
		PID      int      `json:"pid"`
		Proceso  string   `json:"proceso"`
		URLs     []string `json:"urls"`
		Impostor string   `json:"impostor"`
		Via      *struct {
			PID     int    `json:"pid"`
			Proceso string `json:"proceso"`
		} `json:"via"`
	} `json:"objetivos"`
	Negativas []struct {
		PID     int    `json:"pid"`
		Proceso string `json:"proceso"`
		Motivo  string `json:"motivo"`
	} `json:"negativas"`
	SoloListado bool `json:"solo_listado"`
}

// listadoE2E es lo que estas pruebas leen del documento --json del listado.
type listadoE2E struct {
	Escuchando []struct {
		Puerto        uint16 `json:"puerto"`
		PID           int    `json:"pid"`
		Proceso       string `json:"proceso"`
		DeWindows     bool   `json:"de_windows"`
		Intermediario string `json:"intermediario"`
		Via           string `json:"via"`
		Impostor      string `json:"impostor"`
		HTTP          *struct {
			PID  int      `json:"pid"`
			URLs []string `json:"urls"`
		} `json:"http"`
	} `json:"escuchando"`
}

func leerJSON[T any](t *testing.T, out string) T {
	t.Helper()
	var doc T
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("la salida no es JSON: %v\n%s", err, out)
	}
	return doc
}

// esperarLibre espera a que el puerto se pueda tomar (http.sys lo suelta un
// instante después de que se va el último que tenía una URL ahí).
func esperarLibre(port int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if libre(port) {
			return true
		}
	}
	return false
}

// registrarHTTP lanza httpfalso y espera su "PUERTO n". Si http.sys no le
// deja registrar la URL, el fixture dice "ERROR código" y sale: eso es de la
// máquina (el servicio HTTP deshabilitado, una política que no deja a un
// usuario común registrar localhost), no de killport, que todavía no corrió.
// La prueba se saltea y dice por qué; cualquier otra cosa sigue siendo falla.
func registrarHTTP(t *testing.T, dir string, args ...string) (*proceso, string) {
	t.Helper()
	srv := lanzar(t, nil, filepath.Join(dir, "httpfalso.exe"), args...)
	leer := func(l string) (string, bool) {
		t.Helper()
		if v, ok := strings.CutPrefix(l, "PUERTO "); ok {
			return strings.TrimSpace(v), true
		}
		if v, ok := strings.CutPrefix(l, "ERROR "); ok {
			t.Skipf("http.sys no dejó registrar la URL de prueba (código %s): en esta máquina no se puede probar lo que hay detrás de System", strings.TrimSpace(v))
		}
		return "", false
	}
	deadline := time.After(15 * time.Second)
	for {
		select {
		case l := <-srv.lineas:
			if v, ok := leer(l); ok {
				return srv, v
			}
		case <-srv.salio:
			for { // salió: lo que alcanzó a decir antes
				select {
				case l := <-srv.lineas:
					if v, ok := leer(l); ok {
						return srv, v
					}
				default:
					t.Fatal("httpfalso terminó sin decir su puerto")
				}
			}
		case <-deadline:
			t.Fatal("httpfalso no dijo su puerto")
		}
	}
}

// Función 3, http.sys: el puerto figura a nombre de System y es de otro.
func TestE2EHTTPSys(t *testing.T) {
	dir := binariosE2E(t)
	srv, puerto := registrarHTTP(t, dir, "-camino", "killport-e2e")
	port, _ := strconv.Atoi(puerto)
	P := strconv.Itoa(port)
	pid := srv.cmd.Process.Pid
	url := "http://localhost:" + P + "/KILLPORT-E2E/" // http.sys guarda el camino en mayúsculas

	// 1. El plan: el objetivo es el proceso que registró la URL, no el kernel.
	out, code := killportE2E(t, dir, nil, P, "--tcp", "--json")
	rep := leerJSON[reporteE2E](t, out)
	if code != 0 || len(rep.Objetivos) != 1 || len(rep.Negativas) != 0 || !rep.SoloListado {
		t.Fatalf("killport %s --json: código %d\n%s", P, code, out)
	}
	if o := rep.Objetivos[0]; o.Tipo != "proceso" || o.PID != pid || o.Proceso != "httpfalso.exe" ||
		o.Via == nil || o.Via.PID != 4 || !slices.Contains(o.URLs, url) || o.Impostor != "" {
		t.Fatalf("el objetivo = %+v (vía %+v), esperaba a httpfalso.exe (PID %d) con %s\n%s", o, o.Via, pid, url, out)
	}

	// 2. El listado general no esconde el servidor detrás de "System".
	out, _ = killportE2E(t, dir, nil, "--json")
	fila := -1
	lista := leerJSON[listadoE2E](t, out)
	for i, f := range lista.Escuchando {
		if int(f.Puerto) == port {
			fila = i
		}
	}
	if fila < 0 {
		t.Fatalf("el listado general no muestra el %s (quedó oculto entre lo de Windows)\n%s", P, out)
	}
	if f := lista.Escuchando[fila]; f.DeWindows || f.Intermediario != "http.sys" || f.Via != "System" || f.HTTP == nil || f.HTTP.PID != pid ||
		!slices.Contains(f.HTTP.URLs, url) {
		t.Fatalf("la fila del %s = %+v", P, f)
	}

	// 3. Lo que se le muestra a una persona.
	out, code = killportE2E(t, dir, nil, P, "--tcp", "--list")
	for _, dice := range []string{"httpfalso.exe", "PID " + strconv.Itoa(pid), "registró " + url,
		"Windows lo ve como System (PID 4): es http.sys, el servidor HTTP de Windows, no se toca"} {
		if !strings.Contains(out, dice) {
			t.Errorf("--list no dice %q:\n%s", dice, out)
		}
	}
	// Lo que recomiendan los foros y killport no dice jamás.
	for _, no := range []string{"net stop http", "sc config http", "start= disabled"} {
		if strings.Contains(strings.ToLower(out), no) {
			t.Errorf("--list aconseja %q:\n%s", no, out)
		}
	}
	if code != 0 || !srv.vivo() {
		t.Fatalf("--list: código %d, fixture vivo %v", code, srv.vivo())
	}

	// 4. De verdad: se cierra el proceso, y el kernel suelta el puerto.
	out, code = killportE2E(t, dir, nil, P, "--tcp", "-y", "--timeout", "1s")
	if code != 0 || !strings.Contains(out, "Puertos libres  1 de 1") {
		t.Fatalf("killport %s -y: código %d\n%s", P, code, out)
	}
	select {
	case <-srv.salio:
	case <-time.After(5 * time.Second):
		t.Fatal("el proceso que registró la URL sigue vivo")
	}
	if !esperarLibre(port) {
		t.Fatalf("el puerto %d sigue ocupado", port)
	}
	t.Logf("salida real de killport %s -y:\n%s", P, out)

	// 5. Ya sin nadie: libre, y sin inventar un dueño.
	out, code = killportE2E(t, dir, nil, P, "--tcp", "--json")
	if rep := leerJSON[reporteE2E](t, out); code != 0 || len(rep.Objetivos) != 0 || len(rep.Negativas) != 0 || len(rep.Puertos) != 1 || rep.Puertos[0].Estado != "libre" {
		t.Fatalf("después de cerrarlo: código %d\n%s", code, out)
	}
}

// Dos procesos, dos URL, el mismo puerto: los dos son objetivo, y el puerto
// queda libre recién cuando se fueron los dos.
func TestE2EHTTPSysCompartido(t *testing.T) {
	dir := binariosE2E(t)
	a, P := registrarHTTP(t, dir, "-camino", "kp-a")
	port, _ := strconv.Atoi(P)
	b, got := registrarHTTP(t, dir, "-camino", "kp-b", "-puerto", P)
	if got != P {
		t.Fatalf("el segundo fixture tomó el %s, no el %s", got, P)
	}

	out, code := killportE2E(t, dir, nil, P, "--tcp", "--json")
	rep := leerJSON[reporteE2E](t, out)
	if code != 0 || len(rep.Objetivos) != 2 {
		t.Fatalf("killport %s --json: código %d\n%s", P, code, out)
	}
	pids := []int{rep.Objetivos[0].PID, rep.Objetivos[1].PID}
	slices.Sort(pids)
	want := []int{a.cmd.Process.Pid, b.cmd.Process.Pid}
	slices.Sort(want)
	if !slices.Equal(pids, want) {
		t.Fatalf("objetivos %v, esperaba %v\n%s", pids, want, out)
	}
	for _, o := range rep.Objetivos {
		if len(o.URLs) != 1 || o.Via == nil || o.Via.PID != 4 {
			t.Errorf("objetivo %+v: cada uno con SU url y a través de System", o)
		}
	}

	out, code = killportE2E(t, dir, nil, P, "--tcp", "-y", "--force")
	if code != 0 {
		t.Fatalf("killport %s -y --force: código %d\n%s", P, code, out)
	}
	for _, p := range []*proceso{a, b} {
		select {
		case <-p.salio:
		case <-time.After(5 * time.Second):
			t.Fatal("uno de los dos sigue vivo")
		}
	}
	if !esperarLibre(port) {
		t.Fatalf("el puerto %d sigue ocupado", port)
	}
}

// Una URL atada a una dirección (http://127.0.0.1:P/, http://[::1]:P/, lo que
// escribe quien no quiere exponer su HttpListener a la red): netsh la muestra
// con un tercer tramo, HTTP://127.0.0.1:P:127.0.0.1/…, y antes no se leía. El
// puerto quedaba "sin ninguna URL a la vista" y el consejo culpaba a otros
// usuarios.
func TestE2EHTTPSysAtadaAUnaIP(t *testing.T) {
	dir := binariosE2E(t)
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		srv, P := registrarHTTP(t, dir, "-camino", "kp-ip", "-host", host)
		url := "http://" + host + ":" + P + "/KP-IP/"
		out, code := killportE2E(t, dir, nil, P, "--tcp", "--json")
		rep := leerJSON[reporteE2E](t, out)
		if code != 0 || len(rep.Objetivos) != 1 || len(rep.Negativas) != 0 {
			t.Fatalf("killport %s --json (URL atada a %s): código %d\n%s", P, host, code, out)
		}
		if o := rep.Objetivos[0]; o.PID != srv.cmd.Process.Pid || o.Via == nil || o.Via.PID != 4 || !slices.Contains(o.URLs, url) {
			t.Fatalf("el objetivo = %+v, esperaba a httpfalso.exe (PID %d) con %s\n%s", o, srv.cmd.Process.Pid, url, out)
		}
	}
}

// C2: un programa que se llama como un proceso de Windows y no lo es.
func TestE2EImpostor(t *testing.T) {
	dir := binariosE2E(t)
	for _, nombre := range []string{"svchost.exe", "lsass.exe"} {
		t.Run(nombre, func(t *testing.T) {
			exe := filepath.Join(dir, "impostores", nombre)
			imp := lanzar(t, nil, exe, "-modo", "wsl", "-winpid", "0")
			port, _ := strconv.Atoi(imp.esperarLinea(t, "PUERTO "))
			P := strconv.Itoa(port)
			pid := imp.cmd.Process.Pid

			// 1. El listado general (sin --system) NO lo oculta, y lo marca.
			out, _ := killportE2E(t, dir, nil, "--json")
			visto := false
			for _, f := range leerJSON[listadoE2E](t, out).Escuchando {
				if int(f.Puerto) != port {
					continue
				}
				visto = true
				if f.DeWindows || f.PID != pid || f.Proceso != nombre || !strings.Contains(f.Impostor, "se llama "+nombre+" pero corre desde") {
					t.Errorf("la fila = %+v", f)
				}
			}
			if !visto {
				t.Fatalf("el listado general OCULTÓ a un %s que corre desde %s\n%s", nombre, exe, out)
			}
			out, _ = killportE2E(t, dir, nil)
			if !strings.Contains(out, nombre+" (!)") || !strings.Contains(out, "posible impostor") {
				t.Errorf("el listado no lo marca:\n%s", out)
			}

			// 2. El plan: es un objetivo (un proceso cualquiera), no una negativa.
			out, code := killportE2E(t, dir, nil, P, "--tcp", "--json")
			rep := leerJSON[reporteE2E](t, out)
			if code != 0 || len(rep.Objetivos) != 1 || len(rep.Negativas) != 0 {
				t.Fatalf("killport %s --json: código %d\n%s", P, code, out)
			}
			if o := rep.Objetivos[0]; o.Tipo != "proceso" || o.PID != pid || !strings.Contains(o.Impostor, "el de Windows está en") {
				t.Fatalf("el objetivo = %+v", o)
			}
			out, _ = killportE2E(t, dir, nil, P, "--tcp", "--list")
			if !strings.Contains(out, "posible impostor: se llama "+nombre) || strings.Contains(out, "no se toca") || strings.Contains(out, "net stop") {
				t.Errorf("--list:\n%s", out)
			}
			if !imp.vivo() {
				t.Fatal("--list no toca nada")
			}

			// 3. Se cierra, como cualquier programa.
			out, code = killportE2E(t, dir, nil, P, "--tcp", "-y", "--timeout", "1s")
			if code != 0 {
				t.Fatalf("killport %s -y: código %d\n%s", P, code, out)
			}
			select {
			case <-imp.salio:
			case <-time.After(5 * time.Second):
				t.Fatalf("killport protegió a un %s que corre desde %s", nombre, exe)
			}
			if !libre(port) {
				t.Fatalf("el puerto %d sigue ocupado", port)
			}
			t.Logf("salida real de killport %s -y:\n%s", P, out)
		})
	}

	// Y los de verdad siguen como siempre: el 135 es de un svchost genuino.
	out, code := killportE2E(t, dir, nil, "135", "-y")
	if code != 3 || !strings.Contains(out, "no se toca") || strings.Contains(out, "impostor") {
		t.Errorf("killport 135 -y: código %d\n%s", code, out)
	}
	// Con --system se ve todo lo de Windows: ninguno sale marcado.
	out, _ = killportE2E(t, dir, nil, "--json", "--system")
	for _, f := range leerJSON[listadoE2E](t, out).Escuchando {
		if f.Impostor != "" {
			t.Errorf("un proceso de esta máquina sale marcado como impostor: %+v", f)
		}
	}
}

// identidadReal busca en la instantánea de verdad la identidad de un proceso.
func identidadReal(t *testing.T, pid int) reaper.ID {
	t.Helper()
	sys, err := nuevoSistema()
	if err != nil {
		t.Fatal(err)
	}
	procs, err := sys.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, ok := reaper.NewForest(procs).LivePID(uint32(pid))
	if !ok {
		t.Fatalf("el PID %d no está en la instantánea", pid)
	}
	return p.ID
}

// El PROCESO HIJO del trabajo elevado, de punta a punta: el killport.exe de
// verdad con --elevated-job, como lo lanza el padre. Acá corre sin elevar —el
// clic del UAC no se puede automatizar— y contra procesos propios, que no
// piden administrador: lo que se prueba es que el hijo lee el trabajo, mira el
// sistema por su cuenta, toca SOLO lo que corresponde y deja su informe donde
// puede.
func TestE2EHijoElevado(t *testing.T) {
	dir := binariosE2E(t)
	hijo := func(t *testing.T, texto, informe string) int {
		t.Helper()
		out, code := killportE2E(t, dir, nil, "--yes", "--elevated-job", texto, "--result", informe)
		if strings.TrimSpace(out) != "" {
			t.Errorf("el hijo elevado corre oculto y no imprime nada; imprimió:\n%s", out)
		}
		return code
	}
	trabajoTexto := func(t *testing.T, job trabajo) string {
		t.Helper()
		job.Clase, job.TCP, job.UDP, job.Timeout = trabajoCierre, false, false, time.Second
		texto, err := codificarJob(job)
		if err != nil {
			t.Fatal(err)
		}
		return texto
	}

	t.Run("cierra lo que el trabajo nombra", func(t *testing.T) {
		srv := lanzar(t, nil, filepath.Join(dir, "impostores", "svchost.exe"), "-modo", "wsl", "-winpid", "0")
		port, _ := strconv.Atoi(srv.esperarLinea(t, "PUERTO "))
		id := identidadReal(t, srv.cmd.Process.Pid)
		informe := rutaResultado(t)

		// Con la identidad de OTRO (el mismo PID, otra hora de creación): nada.
		ajeno := trabajoTexto(t, trabajo{Puertos: ports.Of(uint16(port)), Kill: []reaper.ID{{PID: id.PID, Created: id.Created.Add(time.Second)}}})
		if code := hijo(t, ajeno, informe); code != 3 || !srv.vivo() {
			t.Fatalf("con otra identidad: código %d, vivo %v", code, srv.vivo())
		}
		doc, err := leerResultado(informe)
		if err != nil || len(doc.Victimas) != 0 || doc.Libres != 0 || doc.Total != 1 {
			t.Fatalf("informe = %+v (%v)", doc, err)
		}
		// Con un puerto que no es el suyo: nada.
		informe = rutaResultado(t)
		if code := hijo(t, trabajoTexto(t, trabajo{Puertos: ports.Of(1), Kill: []reaper.ID{id}}), informe); !srv.vivo() {
			t.Fatalf("con otro puerto: código %d y lo cerró", code)
		}
		// Con lo que corresponde: se cierra, y el informe lo cuenta.
		informe = rutaResultado(t)
		if code := hijo(t, trabajoTexto(t, trabajo{Puertos: ports.Of(uint16(port)), Kill: []reaper.ID{id}}), informe); code != 0 {
			t.Fatalf("código %d", code)
		}
		select {
		case <-srv.salio:
		case <-time.After(5 * time.Second):
			t.Fatal("el proceso sigue vivo")
		}
		doc, err = leerResultado(informe)
		if err != nil || len(doc.Victimas) != 1 || doc.Victimas[0].PID != id.PID || !outcomeDeJSON(doc.Victimas[0].Destino).Done() || doc.Libres != 1 {
			t.Fatalf("informe = %+v (%v)", doc, err)
		}
		// El informe del hijo no lleva el plan: el padre tiene el suyo.
		if len(doc.Objetivos) != 0 || len(doc.Negativas) != 0 {
			t.Errorf("el informe trae un plan: %+v", doc)
		}
	})

	t.Run("detrás de http.sys", func(t *testing.T) {
		srv, puerto := registrarHTTP(t, dir, "-camino", "killport-hijo")
		port, _ := strconv.Atoi(puerto)
		id := identidadReal(t, srv.cmd.Process.Pid)
		// El trabajo nombra al kernel (que es quien figura en la tabla): nada.
		informe := rutaResultado(t)
		alKernel := "v2;do=cierre;ports=" + strconv.Itoa(port) + ";flags=00000;to=1000;kill=4:0"
		if code := hijo(t, alKernel, informe); code != cli.ExitUsage || !srv.vivo() {
			t.Fatalf("nombrando al PID 4: código %d, vivo %v", code, srv.vivo())
		}
		// El trabajo nombra al proceso: el hijo lo encuentra mirando ÉL las colas.
		informe = rutaResultado(t)
		if code := hijo(t, trabajoTexto(t, trabajo{Puertos: ports.Of(uint16(port)), Kill: []reaper.ID{id}}), informe); code != 0 {
			t.Fatalf("código %d", code)
		}
		select {
		case <-srv.salio:
		case <-time.After(5 * time.Second):
			t.Fatal("el proceso que registró la URL sigue vivo")
		}
		if !esperarLibre(port) {
			t.Fatalf("el puerto %d sigue ocupado", port)
		}
	})

	t.Run("el informe, solo donde corresponde", func(t *testing.T) {
		srv := lanzar(t, nil, filepath.Join(dir, "impostores", "lsass.exe"), "-modo", "wsl", "-winpid", "0")
		port, _ := strconv.Atoi(srv.esperarLinea(t, "PUERTO "))
		id := identidadReal(t, srv.cmd.Process.Pid)
		aca, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if aca, err = win.FinalPath(aca); err != nil {
			t.Fatal(err)
		}
		mala := filepath.Join(aca, nombreResultado(3, 3))
		if rutaResultadoValida(mala, carpetasDeResultado()) {
			t.Skip("la carpeta del paquete está dentro de una carpeta de trabajo: no sirve de carpeta ajena")
		}
		t.Cleanup(func() { os.Remove(mala) })
		// El trabajo se hace igual (no depende del informe)…
		if code := hijo(t, trabajoTexto(t, trabajo{Puertos: ports.Of(uint16(port)), Kill: []reaper.ID{id}}), mala); code != 0 {
			t.Fatalf("código %d", code)
		}
		select {
		case <-srv.salio:
		case <-time.After(5 * time.Second):
			t.Fatal("el proceso sigue vivo")
		}
		// …pero el hijo no escribió donde le dijeron.
		if _, err := os.Lstat(mala); err == nil {
			t.Fatalf("el hijo creó %s", mala)
		}
	})

	t.Run("un trabajo que no es un trabajo", func(t *testing.T) {
		srv := lanzar(t, nil, filepath.Join(dir, "impostores", "svchost.exe"), "-modo", "wsl", "-winpid", "0")
		P := srv.esperarLinea(t, "PUERTO ")
		id := identidadReal(t, srv.cmd.Process.Pid)
		bueno := "v2;do=cierre;ports=" + P + ";flags=00000;to=1000;kill=" + idsEncoded([]reaper.ID{id})
		if _, err := decodificarJob(bueno); err != nil {
			t.Fatalf("el trabajo de referencia no es válido: %v", err)
		}
		for nombre, texto := range map[string]string{
			"otra versión":             strings.Replace(bueno, "v2;", "v1;", 1),
			"un campo de más":          bueno + ";cmd=calc.exe",
			"un campo repetido":        bueno + ";kill=" + idsEncoded([]reaper.ID{id}),
			"fuera de orden":           "v2;do=cierre;flags=00000;ports=" + P + ";to=1000;kill=" + idsEncoded([]reaper.ID{id}),
			"con un espacio":           strings.Replace(bueno, ";to=", "; to=", 1),
			"sin nada que hacer":       "v2;do=cierre;ports=" + P + ";flags=00000;to=1000",
			"de las dos clases":        bueno + ";huella=" + strings.Repeat("0", 32),
			"una regla de otro puerto": "v2;do=cierre;ports=" + P + ";flags=00000;to=1000;rule=v4tov4|0.0.0.0|1|10.0.0.1/80",
		} {
			informe := rutaResultado(t)
			if code := hijo(t, texto, informe); code != cli.ExitUsage {
				t.Errorf("%s: código %d", nombre, code)
			}
			if _, err := os.Lstat(informe); err == nil {
				t.Errorf("%s: dejó un informe", nombre)
			}
		}
		if !srv.vivo() {
			t.Fatal("un trabajo inválido cerró el proceso")
		}
	})

	t.Run("configurar la red sin ser administrador", func(t *testing.T) {
		if win.IsElevated() {
			t.Skip("la sesión está elevada: esta prueba es la del hijo que NO lo está")
		}
		antes := exclusionesReales(t)
		// Un pedido bien formado, con la huella del plan que corresponde a esta
		// máquina: lo único que falta es ser administrador.
		sys, err := nuevoSistema()
		if err != nil {
			t.Fatal(err)
		}
		pedido := pedidoConfig{Reservar: ports.Of(47998)}
		estado, err := leerEstadoRed(context.Background(), sys)
		if err != nil {
			t.Skipf("no pude leer el estado de la red: %v", err)
		}
		plan := planificarConfig(estado, pedido)
		if !plan.ejecutable() {
			t.Skipf("en esta máquina reservar el 47998 no tiene plan: %v %v", plan.Hechos, plan.Impedimentos)
		}
		texto, err := codificarJob(trabajo{Clase: trabajoConfig, Pedido: pedido, Huella: plan.huella()})
		if err != nil {
			t.Fatal(err)
		}
		informe := rutaResultado(t)
		if code := hijo(t, texto, informe); code != cli.ExitFailure {
			t.Fatalf("código %d", code)
		}
		doc, err := leerResultadoConfig(informe)
		if err != nil || !strings.Contains(doc.Error, "administrador") || !doc.SoloPlan {
			t.Fatalf("informe = %+v (%v)", doc, err)
		}
		for _, p := range doc.Pasos {
			if p.Resultado == "hecho" {
				t.Errorf("hizo un paso sin ser administrador: %+v", p)
			}
		}
		despues := exclusionesReales(t)
		if x, ok := despues.Find(ports.TCP, ports.IPv4, 47998); ok && x.Administered {
			t.Fatalf("el 47998 quedó reservado: %+v (antes había %d exclusiones, ahora %d)", x, len(antes), len(despues))
		}
	})
}

// exclusionesReales lee los rangos excluidos de esta máquina (solo lectura).
func exclusionesReales(t *testing.T) ports.Exclusions {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var todas ports.Exclusions
	for _, c := range combos {
		out, err := correrNetsh(ctx, "interface", c.ipv(), "show", "excludedportrange", "protocol="+c.prot())
		if err != nil {
			t.Skipf("no pude leer los rangos excluidos de %s: %v", c, err)
		}
		x, err := ports.ParseExcluded(out, c.proto, c.fam)
		if err != nil {
			t.Fatalf("rangos excluidos de %s: %v\n%s", c, err, out)
		}
		todas = append(todas, x...)
	}
	return todas
}

// Función 4 y C3, contra los rangos que esta máquina tenga excluidos. No
// cambia nada: mira, y pide el plan de --reserve sin ejecutarlo.
func TestE2EPuertoSinDueno(t *testing.T) {
	dir := binariosE2E(t)
	antes := exclusionesReales(t)
	var bloque, bloqueUDP, administrada *ports.ExcludedRange
	var bloquesTCP []*ports.ExcludedRange
	for i, x := range antes {
		switch {
		case x.Family != ports.IPv4:
		case !x.Administered && x.Proto == ports.TCP:
			bloquesTCP = append(bloquesTCP, &antes[i])
		case !x.Administered && x.Proto == ports.UDP && bloqueUDP == nil:
			bloqueUDP = &antes[i]
		case x.Administered && administrada == nil:
			administrada = &antes[i]
		}
	}
	if bloqueUDP != nil {
		// Un bloque de WinNAT en UDP: la salida que lo reserva tiene que decir
		// --udp (--reserve solo reserva TCP), y el plan de ESE comando tiene que
		// reservar UDP. Antes se ofrecía `killport P --reserve`, que reservaba
		// el protocolo que no era.
		p := bloqueUDP.Lo + (bloqueUDP.Hi-bloqueUDP.Lo)/2
		P := strconv.Itoa(int(p))
		out, code := killportE2E(t, dir, nil, P, "--udp", "--json")
		rep := leerJSON[reporteE2E](t, out)
		if len(rep.Puertos) != 1 {
			t.Fatalf("killport %s --udp --json: código %d\n%s", P, code, out)
		}
		if pr := rep.Puertos[0]; pr.Estado == "reservado" {
			if len(pr.Remedios) == 0 || pr.Remedios[0].Comando != "killport "+P+" --reserve --udp" {
				t.Errorf("el %s UDP (bloque %s): la salida que reserva es %+v\n%s", P, bloqueUDP.Range, pr.Remedios, out)
			}
			// Con --list: solo el plan, sale 0 (sin --list, --json sale como
			// el texto sin consola: 3, "no cambié nada").
			out, code = killportE2E(t, dir, nil, P, "--reserve", "--udp", "--json", "--list")
			var plan jsonConfig
			if err := json.Unmarshal([]byte(out), &plan); err != nil || code != 0 || !plan.SoloPlan {
				t.Fatalf("killport %s --reserve --udp --json --list: código %d (%v)\n%s", P, code, err, out)
			}
			reservaUDP := false
			for _, x := range plan.Pasos {
				if x.Clase == "reservar" {
					reservaUDP = strings.Contains(x.Comando, "protocol=udp")
					if !reservaUDP {
						break
					}
				}
			}
			if len(plan.Impedimentos) == 0 && !reservaUDP {
				t.Errorf("el plan de `killport %s --reserve --udp` no reserva UDP: %+v", P, plan.Pasos)
			}
		} else {
			t.Logf("el %s UDP ya no figura reservado (%s): WinNAT cambió sus bloques mientras tanto", P, pr.Estado)
		}
	}
	// Un rango excluido también puede tener dueño: el 2869 aparece excluido
	// mientras SSDPSRV lo escucha por http.sys, y ahí killport dice, con razón,
	// que el puerto está en uso y no se toca. Lo que sigue prueba un puerto
	// reservado y SIN dueño: el primer bloque que killport vea así.
	for _, b := range bloquesTCP {
		P := strconv.Itoa(int(b.Lo + (b.Hi-b.Lo)/2))
		out, _ := killportE2E(t, dir, nil, P, "--tcp", "--json")
		if rep := leerJSON[reporteE2E](t, out); len(rep.Puertos) == 1 && rep.Puertos[0].Estado == "reservado" {
			bloque = b
			break
		}
		t.Logf("el %s (bloque %s) no está solo reservado: pruebo con el bloque siguiente", P, b.Range)
	}
	if bloque == nil && administrada == nil {
		t.Skip("esta máquina no tiene ningún rango excluido que sirva (un bloque de WinNAT sin dueño o una exclusión administrada): el diagnóstico queda cubierto con el simulador")
	}

	if bloque != nil {
		p := bloque.Lo + (bloque.Hi-bloque.Lo)/2
		P := strconv.Itoa(int(p))
		out, code := killportE2E(t, dir, nil, P, "--tcp", "--json")
		rep := leerJSON[reporteE2E](t, out)
		if len(rep.Puertos) != 1 || len(rep.Objetivos) != 0 {
			t.Fatalf("killport %s --json: código %d\n%s", P, code, out)
		}
		switch pr := rep.Puertos[0]; pr.Estado {
		case "reservado":
			if pr.Reservado == nil || pr.Reservado.Inicio != bloque.Lo || pr.Reservado.Fin != bloque.Hi || pr.Reservado.Administrada ||
				!strings.Contains(pr.Motivo, "WinNAT") || len(pr.Remedios) < 2 || pr.Remedios[0].Comando != "killport "+P+" --reserve" {
				t.Errorf("el %s (bloque %s) = %+v\n%s", P, bloque.Range, pr, out)
			}
			for _, r := range pr.Remedios {
				if strings.Contains(r.Comando, "excludedportrange") || strings.Contains(r.Efecto, "excludedportrange") {
					t.Errorf("aconseja tocar las exclusiones a mano: %+v", r)
				}
			}
		default:
			// WinNAT pudo soltar el bloque entre la lectura y la corrida.
			t.Logf("el %s ya no figura reservado (%s): WinNAT cambió sus bloques mientras tanto", P, pr.Estado)
		}
		// En texto, el código de salida dice que el puerto no se puede usar.
		out, code = killportE2E(t, dir, nil, P, "--tcp")
		if code == 3 && (!strings.Contains(out, "lo reserva Windows") || !strings.Contains(out, "killport "+P+" --reserve")) {
			t.Errorf("killport %s: código %d\n%s", P, code, out)
		}
		t.Logf("salida real de killport %s:\n%s", P, out)

		// --reserve: el plan, sin ejecutar nada.
		for _, modo := range []string{"--list", "--json", "--yes"} {
			out, code = killportE2E(t, dir, nil, P, "--reserve", modo)
			switch modo {
			case "--json":
				var plan jsonConfig
				// Sale como el texto sin consola (X-39): con algo que hacer, 3.
				if err := json.Unmarshal([]byte(out), &plan); err != nil || (code != 0) != (len(plan.Pasos) > 0 || len(plan.Impedimentos) > 0) || !plan.SoloPlan || len(plan.Huella) != 32 {
					t.Fatalf("killport %s --reserve --json: código %d (%v)\n%s", P, code, err, out)
				}
				if len(plan.Impedimentos) == 0 && (len(plan.Pasos) < 3 || plan.Pasos[0].Clase != "detener-winnat" || plan.Pasos[len(plan.Pasos)-1].Clase != "arrancar-winnat") {
					t.Errorf("el plan de reservar un puerto tapado por WinNAT = %+v", plan.Pasos)
				}
			case "--yes":
				// Sin consola no se hace nada, ni con --yes.
				if code != 3 || !strings.Contains(out, "No cambié nada") {
					t.Errorf("killport %s --reserve --yes (sin consola): código %d\n%s", P, code, out)
				}
			default:
				if code != 0 || !strings.Contains(out, "solo mostré el plan") {
					t.Errorf("killport %s --reserve --list: código %d\n%s", P, code, out)
				}
				t.Logf("salida real de killport %s --reserve --list:\n%s", P, out)
			}
		}
	}

	if administrada != nil {
		// Una exclusión administrada NO impide usar el puerto: se sondea primero.
		p, proto := administrada.Lo, "--tcp"
		if administrada.Proto == ports.UDP {
			proto = "--udp"
		}
		P := strconv.Itoa(int(p))
		out, code := killportE2E(t, dir, nil, P, proto, "--json")
		rep := leerJSON[reporteE2E](t, out)
		if len(rep.Puertos) != 1 {
			t.Fatalf("killport %s %s --json: código %d\n%s", P, proto, code, out)
		}
		if pr := rep.Puertos[0]; pr.Estado == "libre" {
			if pr.Reservado == nil || !pr.Reservado.Administrada || !strings.Contains(pr.Motivo, "exclusión administrada") {
				t.Errorf("el %s (exclusión administrada %s) = %+v\n%s", P, administrada.Range, pr, out)
			}
		} else if strings.Contains(strings.ToLower(pr.Motivo+" "+out), "delete excludedportrange") {
			t.Errorf("aconseja borrar una exclusión administrada:\n%s", out)
		}
		out, _ = killportE2E(t, dir, nil, P, proto)
		t.Logf("salida real de killport %s %s:\n%s", P, proto, out)
	}

	// Nada cambió: las exclusiones administradas son las mismas que antes (los
	// bloques de WinNAT los mueve WinNAT cuando quiere: no se comparan).
	admin := func(x ports.Exclusions) []string {
		var out []string
		for _, e := range x {
			if e.Administered {
				out = append(out, e.Proto.String()+" "+e.Family.String()+" "+e.Range.String())
			}
		}
		slices.Sort(out)
		return out
	}
	if a, d := admin(antes), admin(exclusionesReales(t)); !slices.Equal(a, d) {
		t.Fatalf("las exclusiones administradas cambiaron:\n  antes   %q\n  después %q", a, d)
	}
}
