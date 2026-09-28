package killport

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// conexionFalsa es un io.ReadWriter: lo escrito queda en pedido; lo leído
// sale de respuesta.
type conexionFalsa struct {
	pedido    bytes.Buffer
	respuesta *strings.Reader
}

func (c *conexionFalsa) Write(p []byte) (int, error) { return c.pedido.Write(p) }
func (c *conexionFalsa) Read(p []byte) (int, error)  { return c.respuesta.Read(p) }

func conHTTP(resp string) *conexionFalsa {
	return &conexionFalsa{respuesta: strings.NewReader(resp)}
}

// Q-01: el cliente HTTP/1.1 mínimo que reemplaza a net/http para hablar con
// el motor. Lo que escribe, y cómo lee cada forma de cuerpo.
func TestHTTP1Peticion(t *testing.T) {
	c := conHTTP("HTTP/1.1 204 No Content\r\nApi-Version: 1.47\r\n\r\n")
	r, err := pedirHTTP(c, "POST", "docker", "/containers/abc/stop?t=3", 1<<10)
	if err != nil || r.Code != 204 || r.Header.Get("api-version") != "1.47" || len(r.Body) != 0 {
		t.Fatalf("204: %+v, %v", r, err)
	}
	pedido := c.pedido.String()
	for _, want := range []string{"POST /containers/abc/stop?t=3 HTTP/1.1\r\n", "Host: docker\r\n", "Connection: close\r\n", "Content-Length: 0\r\n"} {
		if !strings.Contains(pedido, want) {
			t.Fatalf("la petición no dice %q:\n%s", want, pedido)
		}
	}
	if !strings.HasSuffix(pedido, "\r\n\r\n") {
		t.Fatalf("la petición no termina en una línea vacía: %q", pedido)
	}
	c = conHTTP("HTTP/1.1 200 OK\r\n\r\n")
	pedirHTTP(c, "GET", "docker", "/_ping", 1<<10)
	if strings.Contains(c.pedido.String(), "Content-Length") {
		t.Fatalf("un GET no lleva cuerpo: %q", c.pedido.String())
	}
}

func TestHTTP1Cuerpos(t *testing.T) {
	casos := []struct {
		nombre, resp, cuerpo string
		tope                 int64
		err                  bool
	}{
		{"Content-Length", "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhola!y lo que sigue no", "hola!", 1 << 10, false},
		{"chunked", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n4\r\n[{\"a\r\n6;ext=1\r\n\":1}]\n\r\n0\r\nX-Trailer: z\r\n\r\n", "[{\"a\":1}]\n", 1 << 10, false},
		{"hasta que cierra", "HTTP/1.1 200 OK\r\n\r\nOK", "OK", 1 << 10, false},
		{"tope del cuerpo", "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n0123456789", "0123", 4, false},
		{"304 sin cuerpo", "HTTP/1.1 304 Not Modified\r\nContent-Length: 5\r\n\r\n", "", 1 << 10, false},
		{"línea de estado rota", "HOLA 200 OK\r\n\r\n", "", 1 << 10, true},
		{"código rota", "HTTP/1.1 abc OK\r\n\r\n", "", 1 << 10, true},
		{"cabecera rota", "HTTP/1.1 200 OK\r\nsin dos puntos\r\n\r\n", "", 1 << 10, true},
		{"Content-Length rota", "HTTP/1.1 200 OK\r\nContent-Length: -3\r\n\r\n", "", 1 << 10, true},
		{"trozo roto", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\n", "", 1 << 10, true},
		{"trozo cortado", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n10\r\nabc", "", 1 << 10, true},
		{"codificación desconocida", "HTTP/1.1 200 OK\r\nTransfer-Encoding: gzip\r\n\r\nxx", "", 1 << 10, true},
		{"se cortó en las cabeceras", "HTTP/1.1 200 OK\r\nApi-Version: 1", "", 1 << 10, true},
		{"nada", "", "", 1 << 10, true},
	}
	for _, c := range casos {
		r, err := pedirHTTP(conHTTP(c.resp), "GET", "docker", "/x", c.tope)
		switch {
		case c.err && err == nil:
			t.Errorf("%s: se aceptó (%+v)", c.nombre, r)
		case !c.err && (err != nil || string(r.Body) != c.cuerpo):
			t.Errorf("%s: cuerpo %q, error %v; esperaba %q", c.nombre, r.Body, err, c.cuerpo)
		}
	}
}

// Un servidor que manda cabeceras sin fin no hace leer a killport para
// siempre (el pipe puede ser de cualquiera).
func TestHTTP1CabecerasSinFin(t *testing.T) {
	resp := "HTTP/1.1 200 OK\r\n" + strings.Repeat("X-Relleno: "+strings.Repeat("a", 1000)+"\r\n", 80)
	if _, err := pedirHTTP(conHTTP(resp), "GET", "docker", "/x", 1<<10); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("cabeceras de más de 64 KB: %v", err)
	}
	resp = "HTTP/1.1 200 OK\r\n" + strings.Repeat("X-A: b\r\n", 200) + "\r\n"
	if _, err := pedirHTTP(conHTTP(resp), "GET", "docker", "/x", 1<<10); err == nil {
		t.Fatal("más de 100 renglones de cabeceras")
	}
}
