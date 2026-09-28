package killport

// http1.go: un cliente HTTP/1.1 mínimo, para hablar con el motor de
// contenedores por su named pipe. Es lo único que killport necesita de HTTP:
// una petición sin cuerpo (GET, o un POST vacío), una conexión por petición
// ("Connection: close"), y leer la respuesta con Content-Length, en trozos
// (chunked) o hasta que el motor cierre. net/http hacía lo mismo y le sumaba
// al binario 3,6 MB de TLS, certificados, HTTP/2 e IDNA para un pipe local
// (medido en la revisión: 8,55 MB contra 4,96 MB).
//
// Puro: habla sobre cualquier io.ReadWriter (el pipe, o un buffer en las
// pruebas). Los topes (de cabeceras y de cuerpo) están porque el pipe puede
// ser de cualquiera (ver win.DialPipe): un servidor que manda basura no puede
// hacer que killport lea para siempre.

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
)

// Topes de la respuesta: cabeceras de a lo sumo 64 KB y 100 renglones.
const (
	http1MaxHeader = 64 << 10
	http1MaxLines  = 100
)

// cabeceras son las de una respuesta, con el nombre en minúsculas (HTTP no
// distingue mayúsculas en los nombres).
type cabeceras map[string]string

// Get es el valor de una cabecera ("" si no vino).
func (h cabeceras) Get(nombre string) string { return h[strings.ToLower(nombre)] }

// respuestaHTTP es lo que devolvió el servidor.
type respuestaHTTP struct {
	Code   int
	Header cabeceras
	Body   []byte
}

// errHTTP es una respuesta que no se entiende.
var errHTTP = errors.New("el motor contestó algo que no es HTTP")

// pedirHTTP escribe una petición sin cuerpo y lee la respuesta. Del cuerpo lee
// a lo sumo tope bytes (lo demás se descarta: un JSON cortado falla después,
// al parsearlo, que es lo que hay que decir). host va en la cabecera Host
// (en un pipe no importa, pero HTTP/1.1 la exige).
func pedirHTTP(rw io.ReadWriter, method, host, path string, tope int64) (respuestaHTTP, error) {
	var b strings.Builder
	b.WriteString(method + " " + path + " HTTP/1.1\r\nHost: " + host + "\r\nUser-Agent: killport\r\nAccept: application/json\r\nConnection: close\r\n")
	if method == "POST" || method == "PUT" {
		b.WriteString("Content-Length: 0\r\n")
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(rw, b.String()); err != nil {
		return respuestaHTTP{}, err
	}
	return leerRespuesta(bufio.NewReader(rw), method, tope)
}

// leerRespuesta lee la línea de estado, las cabeceras y el cuerpo.
func leerRespuesta(br *bufio.Reader, method string, tope int64) (respuestaHTTP, error) {
	leidos := 0
	renglon := func() (string, error) {
		l, err := br.ReadString('\n')
		leidos += len(l)
		if err != nil {
			if err == io.EOF && l == "" {
				return "", io.ErrUnexpectedEOF
			}
			return "", err
		}
		if leidos > http1MaxHeader {
			return "", errors.New("el motor mandó cabeceras de más de 64 KB")
		}
		return strings.TrimRight(l, "\r\n"), nil
	}
	estado, err := renglon()
	if err != nil {
		return respuestaHTTP{}, err
	}
	proto, resto, _ := strings.Cut(estado, " ")
	codigo, _, _ := strings.Cut(resto, " ")
	code, err := strconv.Atoi(codigo)
	if !strings.HasPrefix(proto, "HTTP/1.") || err != nil || code < 100 || code > 999 {
		return respuestaHTTP{}, errHTTP
	}
	r := respuestaHTTP{Code: code, Header: cabeceras{}}
	for i := 0; ; i++ {
		l, err := renglon()
		if err != nil {
			return respuestaHTTP{}, err
		}
		if l == "" {
			break
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok || i >= http1MaxLines || strings.TrimSpace(k) != k || k == "" {
			return respuestaHTTP{}, errHTTP
		}
		r.Header[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	var cuerpo io.Reader
	switch te := strings.ToLower(r.Header.Get("Transfer-Encoding")); {
	case code == 204 || code == 304 || code < 200 || method == "HEAD":
		return r, nil // sin cuerpo, por definición
	case te != "" && te != "identity":
		if te != "chunked" {
			return respuestaHTTP{}, errors.New("el motor mandó un cuerpo codificado como " + te)
		}
		cuerpo = &enTrozos{r: br}
	case r.Header.Get("Content-Length") != "":
		n, err := strconv.ParseInt(r.Header.Get("Content-Length"), 10, 64)
		if err != nil || n < 0 {
			return respuestaHTTP{}, errHTTP
		}
		cuerpo = io.LimitReader(br, n)
	default:
		cuerpo = br // hasta que el motor cierre (Connection: close)
	}
	r.Body, err = io.ReadAll(io.LimitReader(cuerpo, tope))
	return r, err
}

// enTrozos lee un cuerpo "Transfer-Encoding: chunked": tamaño en hexadecimal
// (con extensiones opcionales después de ";"), el trozo, CRLF; un trozo de
// tamaño 0 termina (y lo que sigue son cabeceras finales, que se ignoran).
type enTrozos struct {
	r     *bufio.Reader
	queda int64
	fin   bool
}

func (c *enTrozos) Read(p []byte) (int, error) {
	if c.fin {
		return 0, io.EOF
	}
	if c.queda == 0 {
		l, err := c.r.ReadString('\n')
		if err != nil {
			return 0, io.ErrUnexpectedEOF
		}
		l, _, _ = strings.Cut(strings.TrimRight(l, "\r\n"), ";")
		n, err := strconv.ParseInt(strings.TrimSpace(l), 16, 64)
		if err != nil || n < 0 {
			return 0, errHTTP
		}
		if n == 0 {
			c.fin = true
			return 0, io.EOF
		}
		c.queda = n
	}
	if int64(len(p)) > c.queda {
		p = p[:c.queda]
	}
	n, err := c.r.Read(p)
	c.queda -= int64(n)
	if c.queda == 0 && err == nil {
		// El CRLF que cierra cada trozo.
		if l, err2 := c.r.ReadString('\n'); err2 != nil || strings.TrimRight(l, "\r\n") != "" {
			err = errHTTP
		}
	}
	if err == io.EOF {
		err = io.ErrUnexpectedEOF // se cortó a mitad de un trozo
	}
	return n, err
}
