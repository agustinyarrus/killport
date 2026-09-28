package killport

// comando.go: la columna COMANDO, legible. Una línea de comandos de Windows
// cruda es mayormente ruido: la ruta del ejecutable entre comillas
// ("C:\Program Files\nodejs\node.exe"), rutas absolutas en los argumentos, y
// lo que identifica al proceso —el script, el puerto— queda al final, justo
// lo primero que se come un recorte. Acá se la reescribe para leerla:
//
//	"C:\Program Files\nodejs\node.exe" C:\dev\shop\web\server.js 3000
//	→ node server.js 3000                       (con carpeta C:\dev\shop\web)
//
//   - el programa, sin ruta, sin comillas y sin ".exe";
//   - cada argumento que es una ruta, acortada con criterio: lo que está
//     dentro de node_modules desde el nombre del paquete
//     (vite\bin\vite.js), lo que está debajo de la carpeta del proceso
//     relativo a ella (server.js), lo del perfil del usuario con ~ (~\scripts\x.js);
//   - y si igual no entra, se recorta por el medio lo que le sigue al
//     programa (tui.Cell.Keep): "node …\bin\vite.js --port 5173".
//
// Puro. Las reglas para partir la línea son las de CommandLineToArgvW (se
// prueban contra la función de Windows con miles de líneas al azar).

import (
	"strings"
	"unicode/utf8"

	"github.com/agustinyarrus/killport/internal/tui"
)

// argvDe parte una línea de comandos en sus argumentos con las reglas de
// CommandLineToArgvW:
//
//   - el primero (el programa) es especial: si empieza con comillas llega
//     hasta las siguientes, sin escapes; si no, hasta el primer blanco;
//   - en los demás, 2n barras invertidas seguidas de comillas son n barras y
//     las comillas abren o cierran; 2n+1 barras y comillas son n barras y
//     comillas literales; unas barras que no preceden comillas son literales;
//   - dentro de comillas, "" es una comilla literal (y se sale de las
//     comillas, como hace Windows desde siempre);
//   - fuera de comillas, espacios y tabuladores separan.
//
// Una línea vacía no tiene argumentos (CommandLineToArgvW devolvería la ruta
// del proceso que llama: eso no sirve para describir a otro). O(n).
func argvDe(cmd string) []string {
	var out []string
	i := 0
	// El programa.
	switch {
	case cmd == "":
		return nil
	case cmd[0] == '"':
		end := strings.IndexByte(cmd[1:], '"')
		if end < 0 {
			return []string{cmd[1:]}
		}
		out = append(out, cmd[1:1+end])
		i = 2 + end
	default:
		end := strings.IndexAny(cmd, " \t")
		if end < 0 {
			return []string{cmd}
		}
		out = append(out, cmd[:end])
		i = end
	}
	// Los argumentos.
	for {
		for i < len(cmd) && (cmd[i] == ' ' || cmd[i] == '\t') {
			i++
		}
		if i >= len(cmd) {
			return out
		}
		var b strings.Builder
		quoted := false
	arg:
		for i < len(cmd) {
			switch c := cmd[i]; {
			case c == '\\':
				n := 0
				for i < len(cmd) && cmd[i] == '\\' {
					n++
					i++
				}
				if i < len(cmd) && cmd[i] == '"' {
					b.WriteString(strings.Repeat(`\`, n/2))
					if n%2 == 1 {
						b.WriteByte('"')
						i++
					}
					continue // con n par, las comillas las procesa la vuelta siguiente
				}
				b.WriteString(strings.Repeat(`\`, n))
			case c == '"':
				i++
				if quoted && i < len(cmd) && cmd[i] == '"' {
					b.WriteByte('"')
					i++
				}
				quoted = !quoted
			case (c == ' ' || c == '\t') && !quoted:
				break arg
			default:
				b.WriteByte(c)
				i++
			}
		}
		out = append(out, b.String())
	}
}

// rutasDe es lo que sirve para acortar las rutas de una línea de comandos.
type rutasDe struct {
	cwd  string // la carpeta de trabajo del proceso ("" si no se supo)
	home string // el perfil del usuario (%USERPROFILE%)
}

// comandoLegible reescribe una línea de comandos para leerla (ver arriba).
// keep es cuántas columnas del principio tiene que respetar el recorte por el
// medio: el programa con el espacio que lo sigue, y si corre un módulo
// ("python -m http.server"), también el módulo, que es lo que lo identifica.
// El script de un intérprete, cuando es una ruta larga, lo respeta la forma
// corta (comandoCompacto), que es la que se usa si esta no entra.
func comandoLegible(cmd string, r rutasDe) (texto string, keep int) {
	argv := argvDe(strings.TrimSpace(cmd))
	if len(argv) == 0 {
		return "", 0
	}
	prog := programa(argv[0])
	parts := []string{prog}
	hasta := 0 // índice en parts de lo que se respeta
	for i, a := range argv[1:] {
		if a = acortarArg(a, r); a != "" {
			parts = append(parts, a)
			if hasta == 0 && i > 0 && argv[i] == "-m" {
				hasta = len(parts) - 1
			}
		}
	}
	texto = strings.Join(parts, " ")
	keep = tui.Width(strings.Join(parts[:hasta+1], " "))
	if len(parts) > hasta+1 {
		keep++ // y el espacio que le sigue
	}
	return texto, keep
}

// comandoCompacto es la forma corta de una línea de comandos, para una
// columna angosta: el programa y, de cada argumento que es una ruta, solo el
// último elemento ("node srv.js normal 0.0.0.0 m-L4.txt"). keep respeta el
// programa y el SCRIPT —el primer argumento que es una ruta, o el módulo de
// "-m"—, que es lo que distingue a dos procesos del mismo programa: el
// recorte por el medio se come lo demás. Puro.
func comandoCompacto(cmd string) (texto string, keep int) {
	argv := argvDe(strings.TrimSpace(cmd))
	if len(argv) == 0 {
		return "", 0
	}
	parts := []string{programa(argv[0])}
	script := 0 // índice en parts de lo que se respeta
	for i, a := range argv[1:] {
		b, ruta := baseArg(a)
		if b == "" {
			continue
		}
		parts = append(parts, b)
		if script == 0 && (ruta || (i > 0 && argv[i] == "-m")) {
			script = len(parts) - 1
		}
	}
	texto = strings.Join(parts, " ")
	keep = tui.Width(strings.Join(parts[:script+1], " "))
	if len(parts) > script+1 {
		keep++ // y el espacio que le sigue
	}
	return texto, keep
}

// baseArg es un argumento con las rutas reducidas a su último elemento
// (también el valor de un --flag=ruta), y si era una ruta. Una URL no es
// una ruta.
func baseArg(a string) (string, bool) {
	if strings.Contains(a, "://") {
		return a, false
	}
	if strings.HasPrefix(a, "-") {
		if k, v, ok := strings.Cut(a, "="); ok && esRutaAbsoluta(v) {
			return k + "=" + ultimo(v), false
		}
		return a, false
	}
	if esRutaAbsoluta(a) || strings.ContainsAny(a, `\/`) {
		return ultimo(a), true
	}
	return a, false
}

// ultimo es el último elemento de una ruta ("C:\dev\app\srv.js" → "srv.js").
func ultimo(p string) string {
	p = strings.TrimRight(p, `\/`)
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// programa es el nombre del ejecutable sin ruta ni ".exe".
func programa(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		p = p[i+1:]
	}
	if len(p) > 4 && strings.EqualFold(p[len(p)-4:], ".exe") {
		p = p[:len(p)-4]
	}
	return p
}

// acortarArg acorta un argumento que es una ruta, o que lleva una ruta como
// valor (--config=C:\dev\x.json).
func acortarArg(a string, r rutasDe) string {
	if esRutaAbsoluta(a) {
		return acortarRuta(a, r)
	}
	if strings.HasPrefix(a, "-") {
		if k, v, ok := strings.Cut(a, "="); ok && esRutaAbsoluta(v) {
			return k + "=" + acortarRuta(v, r)
		}
	}
	return a
}

// esRutaAbsoluta: "X:\…", "X:/…" o "\\servidor\…".
func esRutaAbsoluta(s string) bool {
	if strings.HasPrefix(s, `\\`) {
		return true
	}
	return len(s) >= 3 && s[1] == ':' && (s[2] == '\\' || s[2] == '/') && esLetra(s[0])
}

func esLetra(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

// acortarRuta aplica, en orden, el primer criterio que sirve:
//
//  1. dentro de node_modules: desde el nombre del paquete (lo que identifica
//     a un script de npm es el paquete, no dónde está instalado);
//  2. debajo de la carpeta del proceso: relativa a ella (siempre que esa
//     carpeta no sea la raíz de una unidad ni el perfil, donde "relativo"
//     no aclara nada);
//  3. debajo del perfil del usuario: con ~.
//
// Si no, queda como está. Las barras se normalizan a las de Windows.
func acortarRuta(p string, r rutasDe) string {
	p = strings.ReplaceAll(p, "/", `\`)
	low := strings.ToLower(p)
	if i := strings.LastIndex(low, `\node_modules\`); i >= 0 && i+len(`\node_modules\`) < len(p) {
		return p[i+len(`\node_modules\`):]
	}
	if cwd := strings.TrimRight(strings.ReplaceAll(r.cwd, "/", `\`), `\`); cwd != "" && !esRaiz(cwd) && !mismaRuta(cwd, r.home) {
		if rest, ok := debajoDe(p, cwd); ok {
			return rest
		}
	}
	if home := strings.TrimRight(strings.ReplaceAll(r.home, "/", `\`), `\`); home != "" && !esRaiz(home) {
		if rest, ok := debajoDe(p, home); ok {
			return `~\` + rest
		}
	}
	return p
}

// debajoDe dice si p está debajo de dir (sin distinguir mayúsculas, en un
// límite de carpeta) y devuelve el resto.
func debajoDe(p, dir string) (string, bool) {
	if len(p) <= len(dir)+1 || !strings.EqualFold(p[:len(dir)], dir) || p[len(dir)] != '\\' {
		return "", false
	}
	return p[len(dir)+1:], true
}

// esRaiz: "C:" o "C:\" (la raíz de una unidad).
func esRaiz(dir string) bool {
	d := strings.TrimRight(dir, `\/`)
	return len(d) == 2 && d[1] == ':'
}

func mismaRuta(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(strings.ReplaceAll(a, "/", `\`), `\`), strings.TrimRight(strings.ReplaceAll(b, "/", `\`), `\`))
}

// unaLinea deja una línea de comandos en una sola línea: los saltos y
// tabuladores (que una línea de comandos puede traer) pasan a espacios y los
// caracteres de control se van.
func unaLinea(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f || r == utf8.RuneError:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
