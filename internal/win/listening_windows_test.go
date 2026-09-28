//go:build windows

package win

// Pruebas REALES de la espera: la tabla liviana de lo que escucha (la que se
// relee al esperar un puerto) contra la tabla completa, y el handle que solo
// sirve para esperar. Todo fixture se lanza sin ventana y se mata en
// t.Cleanup.

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// sockKeyT identifica una fila para comparar dos lecturas.
type sockKeyT struct {
	proto   Proto
	local   string
	pid     uint32
	created int64
}

func claveDe(s Socket) sockKeyT {
	return sockKeyT{s.Proto, s.Local.String(), s.PID, s.Created.UnixNano()}
}

// La tabla liviana tiene EXACTAMENTE las filas que escuchan de la completa
// (TCP en LISTEN y todo UDP), con los mismos datos, hora de creación incluida.
// Dos lecturas no son la misma foto: se reintenta hasta que coincidan dos
// seguidas (en una máquina quieta, a la primera).
func TestListeningSocketsContraLaTablaCompleta(t *testing.T) {
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	ul, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	propioTCP := uint16(tl.Addr().(*net.TCPAddr).Port)
	propioUDP := uint16(ul.LocalAddr().(*net.UDPAddr).Port)

	var ultimo string
	for intento := 0; intento < 20; intento++ {
		todas, err := AllSockets()
		if err != nil {
			t.Fatal(err)
		}
		livianas, err := ListeningSockets()
		if err != nil {
			t.Fatal(err)
		}
		want := map[sockKeyT]bool{}
		for _, s := range todas {
			if s.Proto == UDP || s.State == TCPListen {
				want[claveDe(s)] = true
			}
		}
		got := map[sockKeyT]bool{}
		vioTCP, vioUDP := false, false
		for _, s := range livianas {
			if s.Proto == TCP && s.State != TCPListen {
				t.Fatalf("la tabla liviana trae una conexión: %+v", s)
			}
			if s.PID == uint32(os.Getpid()) && s.Local.Port() == propioTCP && s.Proto == TCP {
				vioTCP = !s.Created.IsZero()
			}
			if s.PID == uint32(os.Getpid()) && s.Local.Port() == propioUDP && s.Proto == UDP {
				vioUDP = !s.Created.IsZero()
			}
			got[claveDe(s)] = true
		}
		if !vioTCP || !vioUDP {
			t.Fatalf("la tabla liviana no trae (con su hora) el TCP %d o el UDP %d de esta prueba", propioTCP, propioUDP)
		}
		if len(got) == len(want) && func() bool {
			for k := range want {
				if !got[k] {
					return false
				}
			}
			return true
		}() {
			t.Logf("coinciden al intento %d: %d filas que escuchan de %d en total", intento+1, len(got), len(todas))
			return
		}
		ultimo = "distintas"
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("la tabla liviana y la completa nunca coincidieron (%s)", ultimo)
}

// Cuánto cuesta cada lectura en ESTA máquina: es el número que justifica
// releer la tabla liviana cada 25–200 ms mientras se espera un puerto.
//
// El costo es la MEDIANA de 5 tandas de 8 lecturas (el de cada tanda, su
// promedio). Tandas y no lecturas sueltas: el reloj monotónico de Go en
// Windows avanza de a ticks del sistema (entre 0,5 y 15,6 ms), así que una
// lectura suelta de medio milisegundo mide 0 o un tick entero. La mediana y
// no el promedio de todo: en una máquina cargada, una tanda desalojada por el
// planificador infla el promedio sin que leer cueste más.
func TestListeningSocketsCosto(t *testing.T) {
	medir := func(f func() ([]Socket, error)) (time.Duration, int) {
		const tandas, porTanda = 5, 8
		var filas int
		costos := make([]time.Duration, 0, tandas)
		for range tandas {
			t0 := time.Now()
			for range porTanda {
				s, err := f()
				if err != nil {
					t.Fatal(err)
				}
				filas = len(s)
			}
			costos = append(costos, time.Since(t0)/porTanda)
		}
		slices.Sort(costos)
		return costos[tandas/2], filas
	}
	liv, nl := medir(ListeningSockets)
	com, nc := medir(AllSockets)
	// Lo que lee killport para planificar: la tabla completa y, de cada socket
	// que escucha, el módulo dueño (una llamada al sistema por socket).
	conDueño, _ := medir(func() ([]Socket, error) {
		s, err := AllSockets()
		for _, x := range s {
			if x.Proto == UDP || x.State == TCPListen {
				x.Owner()
			}
		}
		return s, err
	})
	t.Logf("mediana por lectura · tabla liviana: %v (%d filas) · completa: %v (%d filas) · completa con dueños: %v", liv, nl, com, nc, conDueño)
	if liv > 50*time.Millisecond {
		t.Errorf("la tabla liviana tarda %v por lectura: releerla cada 200 ms ya no sería barato", liv)
	}
}

// OpenForWait: abre un proceso propio solo para esperarlo, verifica su
// identidad, no permite terminarlo, y la espera sin sondeo lo ve salir.
func TestOpenForWait(t *testing.T) {
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
	if created.IsZero() {
		t.Fatal("el ping no aparece en la foto de procesos")
	}

	if _, err := OpenForWait(pid, created.Add(time.Second)); err != ErrRecycled {
		t.Fatalf("con otra hora de creación: %v (esperaba ErrRecycled)", err)
	}
	h, err := OpenForWait(pid, created)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.CanTerminate || h.Terminate(1) != ErrAccessDenied {
		t.Fatal("un handle para esperar no puede terminar a nadie")
	}
	if _, res, _ := WaitAny(context.Background(), []*Handle{h}, time.Now().Add(100*time.Millisecond)); res != WaitDeadline {
		t.Fatalf("el ping sigue vivo y la espera dio %v", res)
	}
	cmd.Process.Kill()
	t0 := time.Now()
	i, res, err := WaitAny(context.Background(), []*Handle{h}, time.Now().Add(5*time.Second))
	if err != nil || res != WaitExited || i != 0 {
		t.Fatalf("la salida no despertó la espera: %d %v %v", i, res, err)
	}
	t.Logf("la espera despertó %v después de matarlo", time.Since(t0))
}
