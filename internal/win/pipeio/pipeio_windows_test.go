//go:build windows

package pipeio_test

// Las invariantes de pipeio contra un pipe DE VERDAD (servidor: pipetest, que
// acepta conexiones que también son pipeio.Conn; cliente: CreateFile
// overlapped, como win.DialPipe). Toda espera tiene un tope propio: si una
// invariante se rompe, la prueba falla en segundos en vez de colgarse.

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/win/pipeio"
	"github.com/agustinyarrus/killport/internal/win/pipetest"
)

const (
	genericReadWrite   = 0x80000000 | 0x40000000
	openExisting       = 3
	fileFlagOverlapped = 0x40000000
)

// par arma una conexión: devuelve el extremo cliente y el extremo servidor.
func par(t *testing.T) (cliente, servidor *pipeio.Conn) {
	t.Helper()
	path := pipetest.Name("pipeio")
	l, err := pipetest.Listen(path, pipetest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	type aceptada struct {
		c   net.Conn
		err error
	}
	ch := make(chan aceptada, 1)
	go func() {
		c, err := l.Accept()
		ch <- aceptada{c, err}
	}()
	name, _ := syscall.UTF16PtrFromString(path)
	h, err := syscall.CreateFile(name, genericReadWrite, 0, nil, openExisting, fileFlagOverlapped, 0)
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	cliente, err = pipeio.New(h, path)
	if err != nil {
		syscall.CloseHandle(h)
		t.Fatal(err)
	}
	t.Cleanup(func() { cliente.Close() })
	select {
	case a := <-ch:
		if a.err != nil {
			t.Fatalf("Accept: %v", a.err)
		}
		servidor = a.c.(*pipeio.Conn)
		t.Cleanup(func() { servidor.Close() })
	case <-time.After(5 * time.Second):
		t.Fatal("el servidor no aceptó")
	}
	return cliente, servidor
}

// resultado es lo que devolvió una lectura o escritura hecha en otra goroutine.
type resultado struct {
	n   int
	err error
	d   time.Duration
}

// enFondo corre f en otra goroutine y entrega su resultado.
func enFondo(f func() (int, error)) <-chan resultado {
	ch := make(chan resultado, 1)
	start := time.Now()
	go func() {
		n, err := f()
		ch <- resultado{n, err, time.Since(start)}
	}()
	return ch
}

// espera el resultado, o falla si no llega (la operación quedó colgada).
func espera(t *testing.T, ch <-chan resultado, tope time.Duration, que string) resultado {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(tope):
		t.Fatalf("%s: sigue colgada después de %v", que, tope)
		return resultado{}
	}
}

// Leer y escribir A LA VEZ por la misma conexión, en los dos extremos: es lo
// que hace HTTP y lo que os.File no puede hacer sin una carrera de datos.
// Cuatro megas de ida y cuatro de vuelta, en pedazos de tamaños al azar, con
// el eco del servidor corriendo mientras el cliente todavía escribe. Se corre
// con -race.
func TestDuplexALaVez(t *testing.T) {
	cliente, servidor := par(t)
	const total = 4 << 20
	rng := rand.New(rand.NewPCG(20260927, 8))
	ida := make([]byte, total)
	for i := range ida {
		ida[i] = byte(rng.Uint32())
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // el eco: lee y devuelve lo mismo
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := servidor.Read(buf)
			if n > 0 {
				if _, werr := servidor.Write(buf[:n]); werr != nil {
					t.Errorf("eco, escritura: %v", werr)
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					t.Errorf("eco, lectura: %v", err)
				}
				return
			}
		}
	}()

	sizes := make([]int, 0, 256)
	for left := total; left > 0; {
		n := min(1+rng.IntN(96<<10), left)
		sizes = append(sizes, n)
		left -= n
	}
	escritura := enFondo(func() (int, error) {
		off := 0
		for _, n := range sizes {
			m, err := cliente.Write(ida[off : off+n])
			off += m
			if err != nil {
				return off, err
			}
		}
		return off, nil
	})
	vuelta := make([]byte, 0, total)
	lectura := enFondo(func() (int, error) {
		buf := make([]byte, 50<<10)
		for len(vuelta) < total {
			n, err := cliente.Read(buf)
			vuelta = append(vuelta, buf[:n]...)
			if err != nil {
				return len(vuelta), err
			}
		}
		return len(vuelta), nil
	})
	if r := espera(t, escritura, 30*time.Second, "la escritura"); r.err != nil || r.n != total {
		t.Fatalf("escritura = %d bytes, %v", r.n, r.err)
	}
	if r := espera(t, lectura, 30*time.Second, "la lectura"); r.err != nil || r.n != total {
		t.Fatalf("lectura = %d bytes, %v", r.n, r.err)
	}
	if sha256.Sum256(vuelta) != sha256.Sum256(ida) {
		t.Fatal("lo que volvió no es lo que se mandó")
	}
	cliente.Close() // el eco ve el fin y termina
	wg.Wait()
}

// El deadline corta una lectura pendiente, a tiempo y con el error que
// net/http sabe leer; y la conexión SIGUE SIRVIENDO: cortar una lectura no
// pierde los datos que llegan después.
func TestDeadlineCortaLaLecturaYLaConexionSigue(t *testing.T) {
	cliente, servidor := par(t)
	if err := cliente.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	r := espera(t, enFondo(func() (int, error) { return cliente.Read(make([]byte, 16)) }), 5*time.Second, "la lectura con deadline")
	if !errors.Is(r.err, os.ErrDeadlineExceeded) || r.n != 0 {
		t.Fatalf("lectura = %d, %v: esperaba el vencimiento del deadline", r.n, r.err)
	}
	var ne net.Error
	if !errors.As(r.err, &ne) || !ne.Timeout() {
		t.Fatalf("el error no dice Timeout(): %v", r.err)
	}
	if r.d < 150*time.Millisecond || r.d > 2*time.Second {
		t.Fatalf("cortó a los %v, esperaba ~200 ms", r.d)
	}
	// Con el deadline vencido, la próxima lectura ni siquiera llega al kernel:
	// vuelve enseguida en vez de quedar pendiente. "Enseguida" es menos de un
	// segundo y no de 100 ms: lo que se descarta es que se cuelgue, y en una
	// máquina cargada lanzar la goroutine y volver ya puede costar un cuanto.
	if r := espera(t, enFondo(func() (int, error) { return cliente.Read(make([]byte, 16)) }), 5*time.Second, "la lectura con el deadline vencido"); !errors.Is(r.err, os.ErrDeadlineExceeded) || r.d > time.Second {
		t.Fatalf("con el deadline vencido = %v en %v", r.err, r.d)
	}
	// Sin deadline, vuelve a leer.
	cliente.SetReadDeadline(time.Time{})
	if _, err := servidor.Write([]byte("hola")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	r = espera(t, enFondo(func() (int, error) { return cliente.Read(buf) }), 5*time.Second, "la lectura después del corte")
	if r.err != nil || string(buf[:r.n]) != "hola" {
		t.Fatalf("después del corte = %q, %v", buf[:r.n], r.err)
	}
}

// SetReadDeadline vale para la lectura que YA está pendiente: acortarlo la
// corta, alargarlo la deja seguir.
func TestDeadlineSobreLecturaPendiente(t *testing.T) {
	cliente, servidor := par(t)

	// Sin plazo; a los 100 ms alguien pone uno de 150 ms.
	ch := enFondo(func() (int, error) { return cliente.Read(make([]byte, 16)) })
	time.Sleep(100 * time.Millisecond)
	cliente.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	r := espera(t, ch, 5*time.Second, "la lectura a la que se le puso plazo")
	if !errors.Is(r.err, os.ErrDeadlineExceeded) || r.d < 200*time.Millisecond || r.d > 2*time.Second {
		t.Fatalf("= %v a los %v, esperaba el vencimiento a los ~250 ms", r.err, r.d)
	}

	// Plazo de 150 ms; a los 50 ms se alarga a 5 s, y el dato llega a los 400 ms.
	cliente.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 64)
	ch = enFondo(func() (int, error) { return cliente.Read(buf) })
	time.Sleep(50 * time.Millisecond)
	cliente.SetReadDeadline(time.Now().Add(5 * time.Second))
	time.AfterFunc(350*time.Millisecond, func() { servidor.Write([]byte("tarde pero seguro")) })
	r = espera(t, ch, 5*time.Second, "la lectura a la que se le alargó el plazo")
	if r.err != nil || string(buf[:r.n]) != "tarde pero seguro" {
		t.Fatalf("= %q, %v a los %v: alargar el plazo tenía que dejarla seguir", buf[:r.n], r.err, r.d)
	}

	// Un plazo ya vencido corta ahí mismo a la que está pendiente.
	cliente.SetReadDeadline(time.Time{})
	ch = enFondo(func() (int, error) { return cliente.Read(make([]byte, 16)) })
	time.Sleep(100 * time.Millisecond)
	cliente.SetReadDeadline(time.Now().Add(-time.Second))
	if r = espera(t, ch, 5*time.Second, "la lectura con plazo en el pasado"); !errors.Is(r.err, os.ErrDeadlineExceeded) || r.d > time.Second {
		t.Fatalf("= %v a los %v", r.err, r.d)
	}
}

// Un temporizador VIEJO no corta una operación NUEVA: la primera lectura
// termina antes de su plazo; se quita el plazo; la segunda queda pendiente
// mientras pasa la hora del plazo viejo y tiene que sobrevivir.
func TestTemporizadorViejoNoCortaLoNuevo(t *testing.T) {
	cliente, servidor := par(t)
	cliente.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	servidor.Write([]byte("uno"))
	buf := make([]byte, 16)
	if r := espera(t, enFondo(func() (int, error) { return cliente.Read(buf) }), 5*time.Second, "la primera"); r.err != nil || string(buf[:r.n]) != "uno" {
		t.Fatalf("primera = %q, %v", buf[:r.n], r.err)
	}
	cliente.SetReadDeadline(time.Time{})
	ch := enFondo(func() (int, error) { return cliente.Read(buf) })
	time.AfterFunc(600*time.Millisecond, func() { servidor.Write([]byte("dos")) })
	if r := espera(t, ch, 5*time.Second, "la segunda"); r.err != nil || string(buf[:r.n]) != "dos" {
		t.Fatalf("segunda = %q, %v a los %v: la cortó el plazo de la primera", buf[:r.n], r.err, r.d)
	}
}

// El plazo de la escritura: el otro extremo no lee, el búfer del pipe se
// llena y la escritura queda pendiente hasta que el plazo la corta. Cada
// sentido tiene SU plazo: uno de lectura ya vencido no corta la escritura, ni
// uno de escritura vencido corta la lectura.
func TestDeadlineCortaLaEscritura(t *testing.T) {
	cliente, servidor := par(t)
	cliente.SetWriteDeadline(time.Now().Add(300 * time.Millisecond))
	cliente.SetReadDeadline(time.Now().Add(-time.Second)) // vencido, y puesto DESPUÉS: no tiene que tocar el de escritura
	mucho := bytes.Repeat([]byte("x"), 4<<20)
	r := espera(t, enFondo(func() (int, error) { return cliente.Write(mucho) }), 10*time.Second, "la escritura con deadline")
	if !errors.Is(r.err, os.ErrDeadlineExceeded) || r.n >= len(mucho) {
		t.Fatalf("escritura = %d bytes, %v: esperaba el vencimiento", r.n, r.err)
	}
	if r.d < 250*time.Millisecond || r.d > 3*time.Second {
		t.Fatalf("cortó a los %v, esperaba ~300 ms", r.d)
	}
	// Lo que sí salió, llega.
	if r.n > 0 {
		got := make([]byte, r.n)
		servidor.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(servidor, got); err != nil {
			t.Fatalf("lo que la escritura dijo que salió (%d bytes) no llegó: %v", r.n, err)
		}
	}
	// Al revés: con el plazo de escritura vencido, la lectura espera el suyo.
	cliente.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	cliente.SetWriteDeadline(time.Now().Add(-time.Second))
	r = espera(t, enFondo(func() (int, error) { return cliente.Read(make([]byte, 16)) }), 5*time.Second, "la lectura con su propio plazo")
	if !errors.Is(r.err, os.ErrDeadlineExceeded) || r.d < 250*time.Millisecond || r.d > 3*time.Second {
		t.Fatalf("lectura = %v a los %v: esperaba SU vencimiento, a los ~300 ms", r.err, r.d)
	}
}

// Close destraba lo pendiente en los dos sentidos, con net.ErrClosed; espera
// a que termine antes de soltar el handle; se puede repetir; y después todo
// devuelve net.ErrClosed sin tocar el handle (que ya es de otro).
func TestCloseDestrabaLoPendiente(t *testing.T) {
	cliente, _ := par(t)
	lectura := enFondo(func() (int, error) { return cliente.Read(make([]byte, 16)) })
	escritura := enFondo(func() (int, error) { return cliente.Write(bytes.Repeat([]byte("y"), 4<<20)) })
	time.Sleep(150 * time.Millisecond) // las dos quedaron pendientes
	cierre := enFondo(func() (int, error) { return 0, cliente.Close() })
	if r := espera(t, cierre, 5*time.Second, "Close"); r.err != nil {
		t.Fatalf("Close = %v", r.err)
	}
	for nombre, ch := range map[string]<-chan resultado{"la lectura": lectura, "la escritura": escritura} {
		// Ya volvieron: Close las esperó.
		select {
		case r := <-ch:
			if !errors.Is(r.err, net.ErrClosed) {
				t.Fatalf("%s pendiente = %v, esperaba net.ErrClosed", nombre, r.err)
			}
		default:
			t.Fatalf("Close volvió con %s todavía en vuelo", nombre)
		}
	}
	if err := cliente.Close(); err != nil {
		t.Fatalf("el segundo Close = %v", err)
	}
	if _, err := cliente.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Read después de Close = %v", err)
	}
	if _, err := cliente.Write([]byte("z")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Write después de Close = %v", err)
	}
	if err := cliente.SetDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("SetDeadline después de Close = %v", err)
	}
}

// Cuando el otro extremo cierra: lo que había escrito se lee entero y después
// viene io.EOF (no un error); escribirle es un error (no un pánico ni un cuelgue).
func TestFinDelOtroExtremo(t *testing.T) {
	cliente, servidor := par(t)
	servidor.Write([]byte("último"))
	servidor.Close()
	cliente.SetDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(cliente)
	if err != nil || string(got) != "último" {
		t.Fatalf("leído = %q, %v", got, err)
	}
	if n, err := cliente.Read(make([]byte, 8)); n != 0 || err != io.EOF {
		t.Fatalf("después del fin = %d, %v", n, err)
	}
	if _, err := cliente.Write([]byte("hay alguien")); err == nil {
		t.Fatal("escribirle a un pipe sin nadie del otro lado tiene que fallar")
	}
	if n, err := cliente.Read(nil); n != 0 || err != nil {
		t.Fatalf("una lectura de cero bytes = %d, %v", n, err)
	}
}

// Un handle síncrono se rechaza al envolverlo, con un error claro: con él las
// operaciones van de a una y una lectura pendiente no se podría despertar
// (el cierre quedaría esperándola para siempre). El handle sigue siendo de
// quien llamó.
func TestHandleSincronoSeRechaza(t *testing.T) {
	path := pipetest.Name("sincrono")
	l, err := pipetest.Listen(path, pipetest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		if c, err := l.Accept(); err == nil {
			c.Close()
		}
	}()
	name, _ := syscall.UTF16PtrFromString(path)
	h, err := syscall.CreateFile(name, genericReadWrite, 0, nil, openExisting, 0 /* sin FILE_FLAG_OVERLAPPED */, 0)
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	c, err := pipeio.New(h, path)
	if !errors.Is(err, pipeio.ErrNotOverlapped) || c != nil {
		t.Fatalf("New con un handle síncrono = %v, %v: esperaba ErrNotOverlapped", c, err)
	}
	if err := syscall.CloseHandle(h); err != nil {
		t.Fatalf("el handle tenía que seguir abierto y ser de quien llamó: %v", err)
	}
	if _, err := pipeio.New(syscall.InvalidHandle, path); err == nil {
		t.Fatal("un handle inválido tiene que dar error")
	}
}

// Las direcciones son el nombre del pipe.
func TestDirecciones(t *testing.T) {
	cliente, servidor := par(t)
	for _, a := range []net.Addr{cliente.LocalAddr(), cliente.RemoteAddr(), servidor.LocalAddr(), servidor.RemoteAddr()} {
		if a.Network() != "pipe" || a.String() != cliente.RemoteAddr().String() || len(a.String()) < len(`\\.\pipe\x`) {
			t.Fatalf("dirección = %s %q", a.Network(), a.String())
		}
	}
	var _ net.Conn = cliente
}
