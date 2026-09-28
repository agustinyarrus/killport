//go:build windows

package win

// Pruebas contra procesos REALES: el handle con identidad, la detección de PID
// reciclado, la espera sin polling (cancelación instantánea y más de 64
// procesos), el cierre de ventanas y el sondeo de bind. Todo fixture que se
// lanza se mata en t.Cleanup (regla: nada residente).
//
// Fixture barato y largo: `ping -n 999 127.0.0.1`. No necesita consola ni
// stdin (a diferencia de `timeout`), existe en cualquier Windows y no hace
// nada mientras espera. Se lo termina por handle en cada prueba.

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// spawnPing lanza un ping largo SIN ventana y devuelve su PID. Se mata al
// terminar la prueba.
func spawnPing(t *testing.T) uint32 {
	t.Helper()
	cmd := exec.Command("ping", "-n", "999", "127.0.0.1")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	if err := cmd.Start(); err != nil {
		t.Fatalf("no pude lanzar el fixture ping: %v", err)
	}
	pid := uint32(cmd.Process.Pid)
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	return pid
}

func TestHandleIdentidad(t *testing.T) {
	pid := spawnPing(t)
	// Abrir sin verificar identidad para conocer la hora de creación real.
	h, err := OpenForKill(pid, time.Time{})
	if err != nil {
		t.Fatalf("OpenForKill: %v", err)
	}
	defer h.Close()
	if h.PID != pid || h.Created.IsZero() {
		t.Fatalf("handle: pid=%d created=%v", h.PID, h.Created)
	}
	if !h.CanTerminate {
		t.Error("un proceso propio debería poder terminarse")
	}
	if !h.Alive() {
		t.Error("el fixture debería estar vivo")
	}

	// Reabrir verificando la identidad correcta: tiene que coincidir.
	h2, err := OpenForKill(pid, h.Created)
	if err != nil {
		t.Fatalf("con la identidad correcta debería abrir: %v", err)
	}
	h2.Close()
}

func TestHandlePIDReciclado(t *testing.T) {
	pid := spawnPing(t)
	h, err := OpenForKill(pid, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	real := h.Created
	h.Close()

	// Simular reciclado: mismo PID, pero esperábamos otra hora de creación
	// (una hora antes). Tiene que dar ErrRecycled, distinguible.
	_, err = OpenForKill(pid, real.Add(-time.Hour))
	if err != ErrRecycled {
		t.Fatalf("con una hora de creación distinta esperaba ErrRecycled, dio %v", err)
	}
}

func TestHandleGone(t *testing.T) {
	// Un PID que casi seguro no existe (impar, muy alto: los PID de Windows
	// son múltiplos de 4). ErrGone, no un error genérico.
	if _, err := OpenForKill(0x7FFFFFFE, time.Time{}); err != ErrGone {
		t.Fatalf("un PID inexistente debería dar ErrGone, dio %v", err)
	}
}

func TestCloseWindowsSinVentanas(t *testing.T) {
	// El fixture ping no tiene ventanas: CloseWindows devuelve 0, sin error.
	pid := spawnPing(t)
	n, err := CloseWindows(pid)
	if err != nil || n != 0 {
		t.Fatalf("un proceso sin ventanas: n=%d err=%v (esperaba 0, nil)", n, err)
	}
}

func TestCloseWindowsCierraVentana(t *testing.T) {
	if testing.Short() {
		t.Skip("abre una ventana")
	}
	// Una ventana WinForms real que pertenece al proceso de powershell que la
	// muestra (a diferencia del notepad del Store en Win11, cuya ventana vive en
	// OTRO PID). WM_CLOSE la cierra prolijo y el proceso termina. Verifica el
	// peldaño de cierre para procesos con ventana. El script se escribe en el
	// directorio temporal de la prueba (se borra solo): el test es autónomo.
	const winform = `Add-Type -AssemblyName System.Windows.Forms
$f = New-Object System.Windows.Forms.Form
$f.Text = 'killport-fixture'
[System.Windows.Forms.Application]::Run($f)
`
	fixture := filepath.Join(t.TempDir(), "winform.ps1")
	if err := os.WriteFile(fixture, []byte(winform), 0o644); err != nil {
		t.Skipf("no pude escribir el fixture: %v", err)
	}
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", fixture)
	if err := cmd.Start(); err != nil {
		t.Skipf("no pude lanzar la ventana de prueba: %v", err)
	}
	pid := uint32(cmd.Process.Pid)
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-done
	})

	// notepad tarda en crear la ventana: reintentar CloseWindows hasta que
	// haya al menos una a la que mandarle WM_CLOSE.
	var posted int
	for i := 0; i < 100; i++ {
		n, err := CloseWindows(pid)
		if err != nil {
			t.Fatalf("CloseWindows: %v", err)
		}
		if n > 0 {
			posted = n
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if posted == 0 {
		t.Skip("la ventana de prueba no expuso una ventana de nivel superior")
	}
	// Tras el WM_CLOSE, el proceso de la ventana tiene que terminar.
	select {
	case <-done:
		// cerró
	case <-time.After(5 * time.Second):
		t.Errorf("la ventana no cerró tras WM_CLOSE (%d ventana(s))", posted)
	}
}

func TestProbeBindEnUsoYLibre(t *testing.T) {
	// Se ata un socket CRUDO sin opciones (bind exclusivo, como un servidor
	// normal) y se lo sostiene: sondear ese puerto tiene que dar "en uso".
	// Después de cerrarlo, "libre". El oráculo es nuestro propio socket.
	//
	// (Un net.Listener de Go NO sirve para este test: Go le pone SO_REUSEADDR,
	// así que un segundo bind plano no choca. Por eso se usa syscall a pelo.)
	s, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(s, &syscall.SockaddrInet4{Port: 0}); err != nil {
		syscall.Closesocket(s)
		t.Fatal(err)
	}
	lsa, _ := syscall.Getsockname(s)
	port := uint16(lsa.(*syscall.SockaddrInet4).Port)

	st, err := ProbeBind(port, true, false)
	if err != nil {
		t.Fatalf("sondeo en uso: %v", err)
	}
	if st != BindInUse {
		t.Errorf("con un socket exclusivo abierto, el puerto %d debería dar BindInUse, dio %v", port, st)
	}
	syscall.Closesocket(s)

	// Tras cerrar, el puerto queda libre (puede tardar un instante en soltarse).
	var free bool
	for i := 0; i < 20; i++ {
		if st, _ := ProbeBind(port, true, false); st == BindFree {
			free = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !free {
		t.Errorf("tras cerrar el socket, el puerto %d debería quedar libre", port)
	}
}

// El bind PROHIBIDO (WSAEACCES) se prueba en TestProbeBindContraLasExclusiones
// (oracle_windows_test.go), contra la tabla real de rangos excluidos. La
// prueba que había acá partía de una premisa equivocada —que una exclusión
// administrada, la fila con "*", niega el bind— y por eso se salteaba siempre:
// la que lo niega es la reserva de WinNAT.
