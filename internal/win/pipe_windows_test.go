//go:build windows

package win

// Pruebas del cliente de named pipe contra un servidor REAL (pipetest): HTTP
// de ida y vuelta como con un motor de contenedores, el PID del servidor, el
// pipe inexistente, la ACL que niega (el caso "no estás en docker-users"), el
// pipe ocupado y el deadline que corta a un servidor mudo.

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/win/pipetest"
)

func escuchar(t *testing.T, o pipetest.Options) (*pipetest.Listener, string) {
	t.Helper()
	path := pipetest.Name("win")
	l, err := pipetest.Listen(path, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

// HTTP completo por el pipe, con el Transport de la biblioteca estándar.
func TestDialPipeHTTP(t *testing.T) {
	l, path := escuchar(t, pipetest.Options{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.47")
		io.WriteString(w, "OK "+r.URL.Path)
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })

	var server uint32
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		c, err := DialPipe(ctx, path)
		if err == nil {
			server = c.ServerPID()
		}
		return c, err
	}}
	defer tr.CloseIdleConnections()
	cl := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	for i := 0; i < 3; i++ { // también reusa la conexión (keep-alive)
		resp, err := cl.Get("http://docker/_ping")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(b) != "OK /_ping" || resp.Header.Get("Api-Version") != "1.47" {
			t.Fatalf("respuesta = %q %v", b, resp.Header)
		}
	}
	if server != uint32(os.Getpid()) {
		t.Fatalf("PID del servidor = %d, esperaba %d (este proceso)", server, os.Getpid())
	}
}

func TestDialPipeNoExiste(t *testing.T) {
	_, err := DialPipe(context.Background(), `\\.\pipe\navaja-no-existe-`+time.Now().Format("150405.000000"))
	if !errors.Is(err, ErrPipeNotFound) {
		t.Fatalf("esperaba ErrPipeNotFound, dio %v", err)
	}
}

// Un pipe cuya ACL niega a todos: ErrAccessDenied, distinguible (en Docker
// Desktop es "no estás en el grupo docker-users").
func TestDialPipeAccesoDenegado(t *testing.T) {
	l, path := escuchar(t, pipetest.Options{SDDL: "D:(D;;GA;;;WD)"})
	go func() { // una instancia esperando, como un motor vivo
		if c, err := l.Accept(); err == nil {
			c.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := DialPipe(context.Background(), path); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("esperaba ErrAccessDenied, dio %v", err)
	}
}

// Una sola instancia y ya tomada: el segundo cliente espera en el kernel
// (WaitNamedPipe) hasta el deadline de su contexto, y falla a tiempo.
func TestDialPipeOcupado(t *testing.T) {
	l, path := escuchar(t, pipetest.Options{MaxInstances: 1})
	go func() {
		c, err := l.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(3 * time.Second)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	first, err := DialPipe(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = DialPipe(ctx, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("esperaba DeadlineExceeded, dio %v", err)
	}
	if d := time.Since(start); d < 300*time.Millisecond || d > 2*time.Second {
		t.Fatalf("esperó %v, esperaba ~400 ms", d)
	}
}

// Se escribe MIENTRAS hay una lectura pendiente: es lo que hace un cliente
// HTTP (su lector espera la respuesta desde antes de mandar el pedido) y lo
// que exige que el pipe se abra con FILE_FLAG_OVERLAPPED. Con un handle
// síncrono el kernel hace las operaciones de a una: la escritura esperaría a
// que termine la lectura, que espera una respuesta que nadie va a mandar
// porque el pedido no salió. Bloqueo mutuo.
func TestDialPipeEscribeConUnaLecturaPendiente(t *testing.T) {
	l, path := escuchar(t, pipetest.Options{})
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if line, err := bufio.NewReader(c).ReadString('\n'); err == nil { // contesta recién cuando llega el pedido
			io.WriteString(c, "eco: "+line)
		}
	}()
	c, err := DialPipe(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	type leido struct {
		s   string
		err error
	}
	lectura := make(chan leido, 1)
	go func() {
		s, err := bufio.NewReader(c).ReadString('\n')
		lectura <- leido{s, err}
	}()
	time.Sleep(150 * time.Millisecond) // la lectura ya quedó pendiente
	escritura := make(chan error, 1)
	go func() {
		_, err := io.WriteString(c, "hola\n")
		escritura <- err
	}()
	select {
	case err := <-escritura:
		if err != nil {
			t.Fatalf("escritura: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("la escritura quedó trabada detrás de la lectura pendiente: el handle no es overlapped")
	}
	select {
	case r := <-lectura:
		if r.err != nil || r.s != "eco: hola\n" {
			t.Fatalf("lectura = %q, %v", r.s, r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("la respuesta no llegó")
	}
}

// Un servidor que acepta y no contesta: el deadline de la lectura corta (la
// operación pendiente se cancela; con una lectura que no se pudiera cortar,
// un motor colgado colgaría a killport).
func TestDialPipeDeadlineCortaServidorMudo(t *testing.T) {
	l, path := escuchar(t, pipetest.Options{})
	go func() {
		if c, err := l.Accept(); err == nil {
			defer c.Close()
			bufio.NewReader(c).ReadString('\n') // lee y no contesta nunca
			time.Sleep(5 * time.Second)
		}
	}()
	c, err := DialPipe(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	io.WriteString(c, "GET /_ping HTTP/1.1\r\nHost: docker\r\n\r\n")
	start := time.Now()
	_, err = c.Read(make([]byte, 16))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("esperaba el vencimiento del deadline, dio %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("tardó %v en cortar", d)
	}
	if !strings.HasPrefix(c.RemoteAddr().String(), `\\.\pipe\`) || c.RemoteAddr().Network() != "pipe" {
		t.Fatalf("dirección = %v", c.RemoteAddr())
	}
}
