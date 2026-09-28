package ports

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func leerColas(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// resumen aplana una cola a texto, para comparar de un vistazo.
func resumen(qs []RequestQueue) []string {
	var out []string
	for _, q := range qs {
		var ps, us []string
		for _, p := range q.Processes {
			ps = append(ps, fmt.Sprintf("%d=%s", p.PID, p.Image))
		}
		for _, u := range q.URLs {
			us = append(us, u.String())
		}
		out = append(out, fmt.Sprintf("%q ctl=%d=%s procs=[%s] urls=[%s]", q.Name, q.Controller.PID, q.Controller.Image,
			strings.Join(ps, " "), strings.Join(us, " ")))
	}
	return out
}

const pwshReal = `C:\Program Files\WindowsApps\Microsoft.PowerShell_7.6.6.0_x64__8wekyb3d8bbwe\pwsh.exe`

// Las muestras "es" son la salida REAL de netsh en esta máquina (Windows 11
// 28120 en español) con un HttpListener propio registrado; las "en" y la "xx"
// son sintéticas, con la forma de la salida en inglés (formato nuevo y viejo)
// y en un idioma inventado.
func TestColasMuestras(t *testing.T) {
	casos := []struct {
		file string
		want []string
	}{
		{"netsh_es_requestq_vacia.txt", nil},
		{"netsh_es_requestq_una.txt", []string{
			`"la cola de solicitudes no tiene nombre." ctl=0= procs=[15152=` + pwshReal + `] urls=[http://localhost:47180/KP-PRUEBA/]`,
		}},
		{"netsh_es_requestq_dos_urls.txt", []string{
			`"la cola de solicitudes no tiene nombre." ctl=0= procs=[38840=` + pwshReal + `] urls=[http://+:80/TEMPORARY_LISTEN_ADDRESSES/KP-PRUEBA/ http://localhost:47181/KP-DOS/]`,
		}},
		// verbose=yes suma propiedades y tiempos de espera con números sueltos:
		// ninguno se lee como proceso.
		{"netsh_es_requestq_detallada.txt", []string{
			`"la cola de solicitudes no tiene nombre." ctl=0= procs=[38840=` + pwshReal + `] urls=[http://+:80/TEMPORARY_LISTEN_ADDRESSES/KP-PRUEBA/ http://localhost:47181/KP-DOS/]`,
		}},
		// Un HttpListener con URL atadas a una IP (http://127.0.0.1:47233/,
		// http://[::1]:47234/): netsh les suma la dirección como tercer tramo.
		{"netsh_es_requestq_ip.txt", []string{
			`"la cola de solicitudes no tiene nombre." ctl=0= procs=[1856=` + pwshReal + `] urls=[http://[::1]:47234/KP-V6/ http://localhost:47235/KP-NOMBRE/ http://127.0.0.1:47233/KP-V4/]`,
		}},
		{"netsh_en_requestq_iis.txt", []string{
			`"DefaultAppPool" ctl=1480=C:\Windows\System32\svchost.exe procs=[3844=C:\Windows\System32\inetsrv\w3wp.exe 5120=C:\Windows\System32\inetsrv\w3wp.exe] urls=[http://*:80/ https://*:443/ http://[::1]:8080/ADMIN/]`,
			`"Request queue is unnamed." ctl=0= procs=[4472=] urls=[http://+:5985/WSMAN/ https://+:5986/WSMAN/]`,
			`"IdlePool" ctl=1480=C:\Windows\System32\svchost.exe procs=[] urls=[http://intranet.example:8081/]`,
		}},
		{"netsh_en_requestq_viejo.txt", []string{
			`"Request queue is unnamed." ctl=0= procs=[3916= 4020=] urls=[http://+:80/REPORTSERVER/ http://+:80/REPORTS/]`,
		}},
		{"netsh_xx_requestq.txt", []string{
			`"Uio pas dfg hjk." ctl=0= procs=[7321=D:\Herramientas con espacios\servidor.exe] urls=[http://ejemplo.local:9090/API/]`,
		}},
	}
	for _, c := range casos {
		out := leerColas(t, c.file)
		// El repo guarda el texto con LF y netsh escribe CRLF: tienen que dar
		// lo mismo, vengan como vengan.
		lf := strings.ReplaceAll(out, "\r\n", "\n")
		for forma, texto := range map[string]string{"tal cual": out, "LF": lf, "CRLF": strings.ReplaceAll(lf, "\n", "\r\n")} {
			qs, err := ParseRequestQueues(texto)
			if err != nil {
				t.Errorf("%s (%s): %v", c.file, forma, err)
				continue
			}
			if got := resumen(qs); !reflect.DeepEqual(got, c.want) {
				t.Errorf("%s (%s):\n  got  %q\n  want %q", c.file, forma, got, c.want)
			}
		}
	}
}

func TestColasPorPuerto(t *testing.T) {
	qs, err := ParseRequestQueues(leerColas(t, "netsh_en_requestq_iis.txt"))
	if err != nil {
		t.Fatal(err)
	}
	quien := func(port uint16) string {
		var out []string
		for _, q := range qs {
			if len(q.On(port)) > 0 {
				out = append(out, q.Name)
			}
		}
		return strings.Join(out, ",")
	}
	for port, want := range map[uint16]string{80: "DefaultAppPool", 443: "DefaultAppPool", 8080: "DefaultAppPool",
		5985: "Request queue is unnamed.", 8081: "IdlePool", 3000: ""} {
		if got := quien(port); got != want {
			t.Errorf("puerto %d: lo tiene %q, esperaba %q", port, got, want)
		}
	}
}

// Sin la línea de guiones no hay vista: es un error, no "cero colas".
func TestColasSinVista(t *testing.T) {
	for _, out := range []string{
		"",
		"El servicio HTTP no está disponible.\n",
		"Request queue name: X\n    Process IDs:\n        ID: 12, image: C:\\a.exe\n",
	} {
		if qs, err := ParseRequestQueues(out); err == nil {
			t.Errorf("ParseRequestQueues(%q) = %v sin error", out, qs)
		}
	}
}

// Lo que NO es un proceso: los contadores y las propiedades tienen números, y
// alguno hasta la forma «etiqueta: N, etiqueta: texto».
func TestColasNoInventaProcesos(t *testing.T) {
	out := strings.Join([]string{
		"Titulo:",
		"-------",
		"",
		"Cola: una",
		"    Solicitudes activas: 0",
		"    Número máx. de solicitudes: 1000",
		"    Límite: 12, unidad: segundos", // dos valores, pero el segundo no es una ruta
		"    Controlador: 900, imagen: C:\\ctl.exe",    // sí: cuelga de la cola y trae ruta
		"    Otro controlador: 901, imagen: C:\\b.exe", // el controlador es uno: gana el primero
		"    Procesos:",
		"        Id.: 0, imagen: C:\\cero.exe",        // el PID 0 no es un proceso
		"        Id.: 77, imagen: <?>",                // sin ruta legible: queda el PID
		"        Id.: 77, imagen: C:\\dup.exe",        // repetido
		"        Id.: 99999999999, imagen: C:\\x.exe", // no entra en 32 bits
		"        88",
		"        no soy un número",
		"    Propiedades:",
		"        Tiempo de espera: 120", // un número con etiqueta debajo de una sección: no es un PID
		"    Grupos:",
		"    Grupo: FE01",
		"        1234", // un entero solo, pero no cuelga de un título de sección
		"            Direcciones:",
		"                HTTP://+:80/A/",
		"                HTTP://+:80/a/", // la misma (http.sys no distingue mayúsculas)
		"                HTTP://+:99999/B/",
		"                HTTP://:80/C/",
		"                HTTP://+:80/con espacio/",
		"                FTP://+:21/",
	}, "\n")
	qs, err := ParseRequestQueues(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`"una" ctl=900=C:\ctl.exe procs=[77= 88=] urls=[http://+:80/A/]`}
	if got := resumen(qs); !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestColasURL(t *testing.T) {
	casos := []struct {
		in           string
		host, path   string
		port         uint16
		secure, okay bool
	}{
		{"HTTP://+:80/", "+", "/", 80, false, true},
		{"https://*:8443/api/v1/", "*", "/api/v1/", 8443, true, true},
		{"HTTP://LOCALHOST/", "LOCALHOST", "/", 80, false, true},
		{"HTTPS://EXAMPLE.COM/x", "EXAMPLE.COM", "/x", 443, true, true},
		{"http://[::1]:5000/", "::1", "/", 5000, false, true},
		{"http://[fe80::1]/", "fe80::1", "/", 80, false, true},
		{"http://10.0.0.5:65535", "10.0.0.5", "/", 65535, false, true},
		// Atadas a una IP: host:puerto:ip.
		{"HTTP://127.0.0.1:47233:127.0.0.1/KP-V4/", "127.0.0.1", "/KP-V4/", 47233, false, true},
		{"HTTP://[::1]:47234:[::1]/KP-V6/", "::1", "/KP-V6/", 47234, false, true},
		{"https://LOCALHOST:8443:127.0.0.1/", "LOCALHOST", "/", 8443, true, true},
		{"http://+:80:/", "", "", 0, false, false},
		{"http://+:80:x/", "", "", 0, false, false},
		{"http://+:80:[::1/", "", "", 0, false, false},
		{"http://+:80:127.0.0.1:1/", "", "", 0, false, false},
		{"http://+:80:[fe80::1%25eth0]/", "", "", 0, false, false},
		{"http://+:0/", "", "", 0, false, false},
		{"http://+:65536/", "", "", 0, false, false},
		{"http://+:80a/", "", "", 0, false, false},
		{"http://[::1/", "", "", 0, false, false},
		{"http://[::1]x/", "", "", 0, false, false},
		{"http:///solo-camino", "", "", 0, false, false},
		{"httpx://+:80/", "", "", 0, false, false},
		{"Direcciones URL registradas:", "", "", 0, false, false},
	}
	for _, c := range casos {
		u, ok := parseQueueURL(c.in)
		if ok != c.okay {
			t.Errorf("parseQueueURL(%q) ok = %v, esperaba %v", c.in, ok, c.okay)
			continue
		}
		if ok && (u.Host != c.host || u.Path != c.path || u.Port != c.port || u.Secure != c.secure || u.Raw != c.in) {
			t.Errorf("parseQueueURL(%q) = %+v", c.in, u)
		}
	}
}

// La dirección a la que está atada una URL se guarda aparte; el host sigue
// siendo el que se registró.
func TestColasURLAtada(t *testing.T) {
	for in, want := range map[string]string{
		"HTTP://127.0.0.1:47233:127.0.0.1/KP-V4/": "127.0.0.1",
		"HTTP://[::1]:47234:[::1]/KP-V6/":         "::1",
		"HTTP://LOCALHOST:47235/KP-NOMBRE/":       "",
		"http://+:80/":                            "",
	} {
		u, ok := parseQueueURL(in)
		if !ok || u.Bound != want {
			t.Errorf("parseQueueURL(%q) = %+v, %v; esperaba atada a %q", in, u, ok, want)
		}
	}
}

// Propiedad: una vista armada con etiquetas al azar (otro idioma, cada vez)
// se lee igual que la estructura que la generó. El parser no puede depender de
// ninguna palabra.
func TestColasPropiedadSinIdioma(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 3))
	palabra := func() string {
		letras := "abcdefghijklmnñopqrstuvwxyzáéíóúü"
		rs := []rune(letras)
		n := 2 + rng.IntN(8)
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteRune(rs[rng.IntN(len(rs))])
		}
		return b.String()
	}
	etiqueta := func() string {
		n := 1 + rng.IntN(4)
		ws := make([]string, n)
		for i := range ws {
			ws[i] = palabra()
		}
		if rng.IntN(3) == 0 {
			ws[0] += "." // "Id.", "Núm."
		}
		return string(rune('A'+rng.IntN(26))) + strings.Join(ws, " ")
	}
	hosts := []string{"+", "*", "LOCALHOST", "10.1.2.3", "[::1]", "APP.EJEMPLO"}
	for iter := 0; iter < 300; iter++ {
		var b strings.Builder
		var want []string
		b.WriteString("\r\n" + etiqueta() + " (" + etiqueta() + "):\r\n" + strings.Repeat("-", 10+rng.IntN(60)) + " \r\n")
		for q := rng.IntN(4); q > 0; q-- {
			nombre := palabra() + "-" + palabra()
			lNombre := etiqueta()
			b.WriteString("\r\n" + lNombre + ": " + nombre + "\r\n")
			for k := rng.IntN(6); k > 0; k-- { // contadores
				fmt.Fprintf(&b, "    %s: %d\r\n", etiqueta(), rng.IntN(5000))
			}
			ctl := ""
			if rng.IntN(2) == 0 {
				pid := 1 + rng.IntN(60000)
				ctl = fmt.Sprintf("%d=C:\\ctl\\%d.exe", pid, pid)
				fmt.Fprintf(&b, "    %s: %d, %s: C:\\ctl\\%d.exe\r\n", etiqueta(), pid, etiqueta(), pid)
			} else {
				ctl = "0="
			}
			var ps []string
			if n := rng.IntN(4); n > 0 {
				b.WriteString("    " + etiqueta() + ":\r\n")
				visto := map[int]bool{}
				for ; n > 0; n-- {
					pid := 1 + rng.IntN(60000)
					if visto[pid] {
						continue
					}
					visto[pid] = true
					switch rng.IntN(3) {
					case 0: // formato viejo
						fmt.Fprintf(&b, "        %d\r\n", pid)
						ps = append(ps, fmt.Sprintf("%d=", pid))
					case 1: // sin imagen legible
						fmt.Fprintf(&b, "        %s: %d, %s: <?>\r\n", etiqueta(), pid, etiqueta())
						ps = append(ps, fmt.Sprintf("%d=", pid))
					default:
						img := fmt.Sprintf(`D:\%s %s\%d.exe`, palabra(), palabra(), pid)
						fmt.Fprintf(&b, "        %s: %d, %s: %s\r\n", etiqueta(), pid, etiqueta(), img)
						ps = append(ps, fmt.Sprintf("%d=%s", pid, img))
					}
				}
			}
			var us []string
			b.WriteString("    " + etiqueta() + ":\r\n    " + etiqueta() + ": FE0000" + fmt.Sprint(rng.IntN(99)) + "\r\n")
			b.WriteString("        " + lNombre + ": " + nombre + "\r\n") // la etiqueta del bloque, repetida adentro
			if n := rng.IntN(4); n > 0 {
				fmt.Fprintf(&b, "            %s: %d\r\n            %s:\r\n", etiqueta(), n, etiqueta())
				vistas := map[string]bool{}
				for ; n > 0; n-- {
					h := hosts[rng.IntN(len(hosts))]
					port := 1 + rng.IntN(65535)
					raw := fmt.Sprintf("HTTP://%s:%d/%s/", h, port, strings.ToUpper(palabraASCII(rng)))
					if vistas[raw] {
						continue
					}
					vistas[raw] = true
					b.WriteString("                " + raw + "\r\n")
					us = append(us, "http://"+strings.ToLower(h)+fmt.Sprintf(":%d", port)+raw[strings.LastIndex(raw[:len(raw)-1], "/"):])
				}
			}
			want = append(want, fmt.Sprintf("%q ctl=%s procs=[%s] urls=[%s]", nombre, ctl, strings.Join(ps, " "), strings.Join(us, " ")))
		}
		b.WriteString("\r\n")
		qs, err := ParseRequestQueues(b.String())
		if err != nil {
			t.Fatalf("iteración %d: %v\n%s", iter, err, b.String())
		}
		if got := resumen(qs); !reflect.DeepEqual(got, want) {
			t.Fatalf("iteración %d:\n  got  %q\n  want %q\n%s", iter, got, want, b.String())
		}
	}
}

func palabraASCII(rng *rand.Rand) string {
	n := 2 + rng.IntN(6)
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + rng.IntN(26))
	}
	return string(b)
}

// Una salida cortada por la mitad (el tope de salida del ejecutor, un netsh
// que murió) no hace entrar en pánico ni inventa nada: se lee lo que llegó.
func TestColasSalidaCortada(t *testing.T) {
	out := leerColas(t, "netsh_en_requestq_iis.txt")
	guiones := strings.Index(out, "\n---") + 1 // donde empieza la línea de guiones
	enteras, err := ParseRequestQueues(out)
	if err != nil || len(enteras) != 3 {
		t.Fatalf("la muestra entera: %d colas, %v", len(enteras), err)
	}
	for corte := 0; corte <= len(out); corte++ {
		qs, err := ParseRequestQueues(out[:corte])
		switch {
		case corte <= guiones && err == nil:
			t.Fatalf("cortada en %d (antes de la vista): tendría que ser un error", corte)
		case corte > guiones && err != nil:
			t.Fatalf("cortada en %d: %v", corte, err)
		case len(qs) > len(enteras):
			t.Fatalf("cortada en %d: %d colas, más que la muestra entera", corte, len(qs))
		}
		// Un PID cortado por la mitad ("38" de "3844") es el único número que
		// puede aparecer de más, y solo en el último proceso de la última cola.
		for i, q := range qs {
			for j, p := range q.Processes {
				ultimo := i == len(qs)-1 && j == len(q.Processes)-1
				if !ultimo && !hasProcess(enteras[i].Processes, p.PID) {
					t.Fatalf("cortada en %d: inventó el proceso %d", corte, p.PID)
				}
			}
		}
	}
}
