//go:build windows

package win

// Pruebas contra el sistema real de lo que agregó la integración de killport:
// la ruta de cualquier proceso sin abrirlo (contra un oráculo independiente),
// WaitAny (primera salida, plazo, cancelación, y el punto ciego pasados los
// 63 handles), la identidad en QueryDetail, el proceso lanzado por
// ShellExecute esperado con WaitAny, el archivo de resultado que no se deja
// desviar, y los caminos de error de los servicios. Todo fixture se mata en
// t.Cleanup.

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// TestImagePathContraOraculo: la ruta que da la clase 88 (sin abrir el
// proceso) tiene que coincidir con la de QueryFullProcessImageNameW (abriendo
// el proceso, otra API y otro formato de origen) en TODOS los procesos que se
// dejan abrir. Y tiene que haber ruta para casi todos, incluidos los que no se
// dejan abrir (que es para lo que existe).
func TestImagePathContraOraculo(t *testing.T) {
	procs, err := AllProcesses()
	if err != nil {
		t.Fatal(err)
	}
	var compared, withPath, gone int
	for _, p := range procs {
		if p.PID == 0 {
			continue
		}
		got, err := ImagePath(p.PID)
		if errors.Is(err, ErrGone) {
			gone++ // murió entre la foto y la consulta
			continue
		}
		if err != nil {
			t.Fatalf("ImagePath(%d %s): %v", p.PID, p.Image, err)
		}
		if got != "" {
			withPath++
			if !strings.EqualFold(filepath.Base(got), p.Image) && p.PID > 4 {
				t.Errorf("PID %d: la ruta %q no termina en su imagen %q", p.PID, got, p.Image)
			}
		}
		h, err := openProcess(processQueryLimitedInfo, p.PID)
		if err != nil {
			continue
		}
		oracle := queryFullImageName(h)
		closeHandle(h)
		if oracle == "" {
			continue
		}
		compared++
		if !strings.EqualFold(got, oracle) {
			t.Errorf("PID %d %s: clase 88 dio %q, QueryFullProcessImageName dio %q", p.PID, p.Image, got, oracle)
		}
	}
	t.Logf("%d procesos · %d con ruta · %d comparados contra el oráculo · %d murieron en el medio", len(procs), withPath, compared, gone)
	if compared < 10 || withPath < len(procs)*9/10 {
		t.Fatalf("muy pocos datos: %d comparados, %d de %d con ruta", compared, withPath, len(procs))
	}
}

func TestImagePathGone(t *testing.T) {
	if _, err := ImagePath(0x7FFFFFF8); !errors.Is(err, ErrGone) {
		t.Fatalf("un PID inexistente debería dar ErrGone, dio %v", err)
	}
}

func TestQueryDetailIdentidad(t *testing.T) {
	pid := spawnPing(t)
	h, err := OpenForKill(pid, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	created := h.Created
	h.Close()
	d, err := QueryDetail(pid, created)
	if err != nil {
		t.Fatalf("con la identidad correcta: %v", err)
	}
	if !strings.Contains(strings.ToLower(d.CommandLine), "ping") {
		t.Errorf("línea de comandos = %q", d.CommandLine)
	}
	if _, err := QueryDetail(pid, created.Add(-time.Second)); err != ErrRecycled {
		t.Fatalf("con otra hora de creación esperaba ErrRecycled, dio %v", err)
	}
}

func TestProtectedProcesoComun(t *testing.T) {
	pid := spawnPing(t)
	h, err := OpenForKill(pid, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.Protected() {
		t.Fatal("un ping no es un proceso protegido")
	}
}

// TestWaitAnyPrimeraSalida: de tres, sale el del medio; WaitAny devuelve su
// índice y vuelve apenas sale (no espera al plazo).
func TestWaitAnyPrimeraSalida(t *testing.T) {
	var hs []*Handle
	for i := 0; i < 3; i++ {
		h, err := OpenForKill(spawnPing(t), time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		hs = append(hs, h)
	}
	go func() {
		time.Sleep(40 * time.Millisecond)
		hs[1].Terminate(1)
	}()
	start := time.Now()
	i, res, err := WaitAny(context.Background(), hs, time.Now().Add(10*time.Second))
	if err != nil || res != WaitExited || i != 1 {
		t.Fatalf("WaitAny = %d, %v, %v; esperaba 1, WaitExited", i, res, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("tardó %v en ver la salida", d)
	}
}

func TestWaitAnyPlazoYCancelacion(t *testing.T) {
	h, err := OpenForKill(spawnPing(t), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()

	start := time.Now()
	if _, res, _ := WaitAny(context.Background(), []*Handle{h}, time.Now().Add(60*time.Millisecond)); res != WaitDeadline {
		t.Fatalf("esperaba WaitDeadline, dio %v", res)
	}
	if d := time.Since(start); d < 55*time.Millisecond || d > 2*time.Second {
		t.Fatalf("el plazo de 60 ms duró %v", d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var canceledAt time.Time
	go func() {
		time.Sleep(40 * time.Millisecond)
		canceledAt = time.Now()
		cancel()
	}()
	if _, res, _ := WaitAny(ctx, []*Handle{h}, time.Time{}); res != WaitCanceled {
		t.Fatalf("esperaba WaitCanceled, dio %v", res)
	}
	if lat := time.Since(canceledAt); lat > 50*time.Millisecond {
		t.Fatalf("la cancelación tardó %v en despertar la espera", lat)
	}

	// Sin handles: espera pura hasta el plazo.
	start = time.Now()
	if _, res, _ := WaitAny(context.Background(), nil, time.Now().Add(30*time.Millisecond)); res != WaitDeadline || time.Since(start) < 25*time.Millisecond {
		t.Fatalf("la espera pura dio %v en %v", res, time.Since(start))
	}
}

// TestWaitAnyPasados63: con 70 procesos, el que sale es el ÚLTIMO (el de la
// segunda tanda de 63 handles).
// Una espera por tandas secuenciales no lo vería hasta que salieran los 63
// primeros; WaitAny lo tiene que ver enseguida.
func TestWaitAnyPasados63(t *testing.T) {
	if testing.Short() {
		t.Skip("lanza 70 procesos")
	}
	const n = 70
	hs := make([]*Handle, n)
	for i := range hs {
		h, err := OpenForKill(spawnPing(t), time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		hs[i] = h
	}
	go func() {
		time.Sleep(40 * time.Millisecond)
		hs[n-1].Terminate(1)
	}()
	start := time.Now()
	i, res, err := WaitAny(context.Background(), hs, time.Now().Add(10*time.Second))
	if err != nil || res != WaitExited || i != n-1 {
		t.Fatalf("WaitAny = %d, %v, %v; esperaba %d", i, res, err, n-1)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("la salida en la segunda tanda de handles tardó %v en verse", d)
	}
}

// TestShellExecuteProcess: lanzar con "open" (el camino "runas" es el mismo
// salvo el diálogo, que no se automatiza), esperar con WaitAny y leer el
// código de salida exacto.
func TestShellExecuteProcess(t *testing.T) {
	h, err := ShellExecuteProcess("open", filepath.Join(systemDirectory, "cmd.exe"), []string{"/c", "exit", "7"}, true)
	if err != nil || h == nil {
		t.Fatalf("ShellExecuteProcess: %v", err)
	}
	defer h.Close()
	if h.PID == 0 || h.Created.IsZero() {
		t.Fatalf("handle sin identidad: %+v", h)
	}
	if _, res, err := WaitAny(context.Background(), []*Handle{h}, time.Now().Add(10*time.Second)); res != WaitExited || err != nil {
		t.Fatalf("WaitAny = %v, %v", res, err)
	}
	if code, ok := h.ExitCode(); !ok || code != 7 {
		t.Fatalf("código = %d (%v), esperaba 7", code, ok)
	}
}

// TestCreateNewExact: crea y escribe; no pisa un archivo existente; y si una
// carpeta del camino se cambió por un desvío (una unión de directorio, que se
// crea sin privilegios), no deja el archivo en el destino desviado.
func TestCreateNewExact(t *testing.T) {
	base := t.TempDir()
	real, err := FinalPath(base)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(real, "resultado.json")
	f, err := CreateNewExact(target)
	if err != nil {
		t.Fatalf("camino feliz: %v", err)
	}
	f.WriteString(`{"ok":true}`)
	f.Close()
	if b, _ := os.ReadFile(target); string(b) != `{"ok":true}` {
		t.Fatalf("contenido = %q", b)
	}
	if _, err := CreateNewExact(target); err == nil {
		t.Fatal("CREATE_NEW no debería abrir un archivo existente")
	}

	// Desvío: la carpeta "pedida" pasa a ser una unión hacia "otra".
	pedida := filepath.Join(real, "pedida")
	otra := filepath.Join(real, "otra")
	os.Mkdir(pedida, 0o755)
	os.Mkdir(otra, 0o755)
	want := filepath.Join(pedida, "r.json") // la ruta canónica que habría pasado el padre
	os.Remove(pedida)
	if out, err := exec.Command(filepath.Join(systemDirectory, "cmd.exe"), "/c", "mklink", "/J", pedida, otra).CombinedOutput(); err != nil {
		t.Skipf("no pude crear la unión de prueba: %v %s", err, out)
	}
	if _, err := CreateNewExact(want); err == nil {
		t.Fatal("con una carpeta desviada tenía que negarse")
	}
	if _, err := os.Stat(filepath.Join(otra, "r.json")); !os.IsNotExist(err) {
		t.Fatalf("quedó un archivo en el destino desviado: %v", err)
	}
}

// TestCallBuferEnPilaNueva: regresión de la directiva uintptrescapes de call.
// Una goroutine recién creada tiene la pila mínima; la primera llamada que
// pasa un búfer de tamaño fijo por call hace crecer (y copiar) esa pila. Sin
// la directiva, el sistema escribía en la pila vieja y el resultado volvía
// vacío. 64 goroutines nuevas, cada una con su primera llamada.
func TestCallBuferEnPilaNueva(t *testing.T) {
	dir := t.TempDir()
	want, err := FinalPath(dir)
	if err != nil || want == "" {
		t.Fatalf("FinalPath = %q, %v", want, err)
	}
	type verdict struct {
		got string
		err error
	}
	results := make(chan verdict, 64)
	for i := 0; i < 64; i++ {
		go func() {
			got, err := FinalPath(dir)
			results <- verdict{got, err}
		}()
	}
	for i := 0; i < 64; i++ {
		if v := <-results; v.err != nil || v.got != want {
			t.Fatalf("en una goroutine nueva FinalPath dio %q (%v), esperaba %q", v.got, v.err, want)
		}
	}
}

func TestFinalPathNombreCorto(t *testing.T) {
	long := filepath.Join(t.TempDir(), "carpeta con nombre largo")
	os.Mkdir(long, 0o755)
	buf := make([]uint16, 1024)
	r, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetShortPathNameW").Call(
		uintptr(unsafe.Pointer(UTF16Ptr(long))), uintptr(unsafe.Pointer(&buf[0])), 1024)
	short := syscall.UTF16ToString(buf[:r])
	if r == 0 || strings.EqualFold(short, long) {
		t.Skip("el volumen no genera nombres cortos 8.3")
	}
	got, err := FinalPath(short)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := FinalPath(long)
	if !strings.EqualFold(got, want) || strings.Contains(got, "~") {
		t.Fatalf("FinalPath(%q) = %q, esperaba %q", short, got, want)
	}
}

func TestStopServiceErrores(t *testing.T) {
	err := StopService(context.Background(), "killport-servicio-que-no-existe", time.Now().Add(time.Second))
	if !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("inexistente: %v", err)
	}
	if IsElevated() {
		return // no se detienen servicios reales en una prueba
	}
	// Sin admin, pedir detener un servicio real se niega ANTES de tocarlo.
	err = StopService(context.Background(), "Spooler", time.Now().Add(time.Second))
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("sin admin esperaba acceso denegado, dio %v", err)
	}
}

// TestParseDependents: bytes sintéticos de ENUM_SERVICE_STATUSW (48 bytes por
// fila, punteros que apuntan DENTRO del mismo búfer).
func TestParseDependents(t *testing.T) {
	names := []string{"Spooler", "SSDPSRV"}
	b := alignedBuffer(2*esEntrySize + 64)
	base := uintptr(unsafe.Pointer(&b[0]))
	off := 2 * esEntrySize
	for i, n := range names {
		binary.LittleEndian.PutUint64(b[i*esEntrySize:], uint64(base)+uint64(off))
		for _, u := range syscall.StringToUTF16(n) {
			binary.LittleEndian.PutUint16(b[off:], u)
			off += 2
		}
	}
	got, err := parseDependents(b, base, 2)
	if err != nil || len(got) != 2 || got[0] != "Spooler" || got[1] != "SSDPSRV" {
		t.Fatalf("parseDependents = %v, %v", got, err)
	}
	if _, err := parseDependents(b[:esEntrySize], base, 2); !errors.Is(err, ErrMalformed) {
		t.Fatalf("truncado: esperaba ErrMalformed, dio %v", err)
	}
	var de error = &DependentsError{Names: got}
	if !errors.Is(de, ErrServiceDependents) || !strings.Contains(de.Error(), "SSDPSRV") {
		t.Fatalf("DependentsError = %v", de)
	}
}
