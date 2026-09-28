//go:build windows

package win

// Lo que no se puede dar por sabido al leer el sistema: la tabla de servicios
// que no entra en el primer búfer, la hora de salida de un proceso, y el
// detalle de uno cuya memoria no se puede leer. Contra el sistema real. Todo
// fixture se lanza sin ventana y se mata en t.Cleanup.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// C-04: con ERROR_MORE_DATA, pcbBytesNeeded es lo que falta para las entradas
// RESTANTES. El bucle de Services() lo tomaba como el total y oscilaba hasta
// rendirse. Se lo recorre de verdad arrancando con búferes menores que el
// total de esta máquina: tiene que devolver la lista entera, igual a la de un
// búfer holgado.
func TestServicesBufferChico(t *testing.T) {
	scm, e := call(procOpenSCManagerW, 0, 0, scManagerConnect|scManagerEnumerate)
	if scm == 0 {
		t.Fatal(lastError(e))
	}
	var needed, returned, resume uint32
	buf := alignedBuffer(8)
	call(procEnumServicesStatusExW, scm, scEnumProcessInfo, serviceWin32, serviceActive,
		uintptr(unsafe.Pointer(&buf[0])), 8, uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&returned)),
		uintptr(unsafe.Pointer(&resume)), 0)
	call(procCloseServiceHandle, scm)
	total := needed
	ref, err := Services()
	if err != nil || total == 0 {
		t.Fatalf("referencia: %v (total %d)", err, total)
	}
	nombres := func(ss []Service) map[string]bool {
		m := map[string]bool{}
		for _, s := range ss {
			m[s.Name] = true
		}
		return m
	}
	want := nombres(ref)
	for _, start := range []uint32{64, total / 4, total / 2, total * 3 / 4, total - 200} {
		got, err := servicesFrom(start)
		if err != nil {
			t.Fatalf("arrancando con %d bytes (total %d): %v", start, total, err)
		}
		// Un servicio puede arrancar o parar entre dos lecturas: se tolera una
		// diferencia mínima, no media lista.
		have := nombres(got)
		faltan := 0
		for n := range want {
			if !have[n] {
				faltan++
			}
		}
		if faltan > 2 || len(got) < len(ref)-2 {
			t.Fatalf("arrancando con %d bytes: %d servicios (referencia %d, faltan %d)", start, len(got), len(ref), faltan)
		}
	}
	t.Logf("total de esta máquina: %d bytes, %d servicios", total, len(ref))
}

// C-07: la hora de salida que da el kernel (GetProcessTimes), para medir
// cuánto tardó un proceso en cerrar desde que se le pidió.
func TestExitTime(t *testing.T) {
	cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "ping.exe"), "-n", "30", "127.0.0.1")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	pid := uint32(cmd.Process.Pid)
	var created time.Time
	procs, err := AllProcesses()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.PID == pid {
			created = p.Created
		}
	}
	h, err := OpenForWait(pid, created)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if at, ok := h.ExitTime(); ok {
		t.Fatalf("sigue vivo y ya tiene hora de salida %v", at)
	}
	t0 := time.Now()
	cmd.Process.Kill()
	if _, res, err := WaitAny(context.Background(), []*Handle{h}, time.Now().Add(5*time.Second)); err != nil || res != WaitExited {
		t.Fatalf("la espera: %v %v", res, err)
	}
	t1 := time.Now()
	at, ok := h.ExitTime()
	// La hora del sistema avanza de a un tick (hasta ~16 ms): se tolera eso.
	if !ok || at.Before(t0.Add(-20*time.Millisecond)) || at.After(t1.Add(20*time.Millisecond)) {
		t.Fatalf("hora de salida %v (ok %v) fuera de [%v, %v]", at, ok, t0, t1)
	}
}

// S-10: la carpeta de trabajo (que pide PROCESS_VM_READ y leer la memoria del
// proceso) se lee solo si quien llama dice que vale la pena, con la ruta y la
// marca de crítico ya leídas con el permiso mínimo.
func TestQueryDetailWithSinMemoria(t *testing.T) {
	yo := uint32(os.Getpid())
	exe, _ := os.Executable()
	wd, _ := os.Getwd()
	var vio string
	d, err := QueryDetailWith(yo, time.Time{}, func(path string, critico bool) bool { vio = path; return false })
	if err != nil || d.Cwd != "" || d.CommandLine == "" || d.Path == "" {
		t.Fatalf("sin carpeta: %+v, %v", d, err)
	}
	if !strings.EqualFold(filepath.Clean(vio), filepath.Clean(exe)) {
		t.Fatalf("la decisión recibió la ruta %q, esperaba %q", vio, exe)
	}
	d, err = QueryDetailWith(yo, time.Time{}, func(string, bool) bool { return true })
	if err != nil || !strings.EqualFold(filepath.Clean(d.Cwd), filepath.Clean(wd)) {
		t.Fatalf("con carpeta: %q (esperaba %q), %v", d.Cwd, wd, err)
	}
}
