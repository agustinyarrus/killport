package killport

// trabajo.go: lo que viaja al PROCESO ELEVADO, y cómo se lee del otro lado.
//
// Cuando killport necesita administrador se relanza a sí mismo por el UAC. Lo
// que el hijo elevado tiene que hacer viaja POR ARGUMENTOS —nunca por un
// archivo, que otro programa del usuario podría cambiar entre que se escribe y
// que el hijo lo lee— en un solo texto: el trabajo.
//
// Un proceso con token de administrador que obedece a sus argumentos es un
// blanco: cualquier programa del usuario puede lanzar killport elevado con el
// trabajo que quiera, y el diálogo del UAC va a decir "killport". Por eso el
// trabajo NO es una lista de órdenes. Es una SELECCIÓN dentro de lo que el
// hijo, mirando el sistema por su cuenta, decide que corresponde hacer:
//
//   - un proceso se cierra solo si el plan que arma el hijo lo tiene de
//     objetivo (ocupa uno de los puertos pedidos y ninguna regla lo protege) Y
//     su identidad está en el trabajo;
//   - un servicio se detiene solo si ese plan lo tiene de objetivo y lo corre
//     el mismo proceso que cuando el usuario confirmó;
//   - una regla de portproxy se borra solo si está en el registro, tiene uno
//     de los puertos pedidos y conecta adonde dice el trabajo;
//   - un servicio se deja en manual solo si el hijo lo detuvo en esa misma
//     corrida (y es de terceros, y quedó detenido);
//   - la red se configura solo con los pasos que arma el planificador, y solo
//     si son los mismos que el usuario confirmó (la huella);
//   - el informe se escribe solo en una carpeta de trabajo del propio hijo, y
//     con un nombre que no sale de ningún argumento (al final de este archivo).
//
// Este archivo es la primera mitad de esa defensa: la GRAMÁTICA. Es cerrada y
// estricta —una versión, un alfabeto, un juego fijo de campos, cada valor con
// su forma y su tope— y lo que no la cumple no se interpreta "lo mejor
// posible": se rechaza entero. La segunda mitad, contrastar contra el sistema,
// está en elevacion.go.
//
// Puro: sin build tag.

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
)

const (
	versionTrabajo = "v2"
	// maxTrabajo acota el texto entero: entra holgado en una línea de
	// comandos (32.767 caracteres) junto con todo lo demás.
	maxTrabajo = 8000

	maxCerrar    = 64 // procesos por trabajo
	maxServicios = 16
	maxReglas    = 16
	maxEspera    = time.Hour
)

// claseTrabajo es qué clase de trabajo es. Son dos y no se mezclan.
type claseTrabajo uint8

const (
	trabajoCierre claseTrabajo = iota + 1 // liberar puertos: cerrar, detener, borrar reglas
	trabajoConfig                         // configurar la red: reservar, quitar reservas, rango dinámico
)

// svcObjetivo es un servicio de terceros a detener (nombre + proceso dueño).
type svcObjetivo struct {
	Nombre string
	Owner  reaper.ID
}

// trabajo es lo que el proceso elevado necesita para hacer exactamente lo que
// el padre no pudo.
type trabajo struct {
	Clase claseTrabajo

	// Cierre.
	Puertos   ports.Set
	Tree      bool
	Force     bool
	TCP, UDP  bool
	AllStates bool
	Timeout   time.Duration
	Kill      []reaper.ID       // procesos a cerrar (raíces)
	Servicios []svcObjetivo     // servicios de terceros a detener
	Manual    []string          // de esos servicios, los que además quedan en inicio manual
	Reglas    []ports.ProxyRule // reglas de portproxy a borrar

	// Configuración.
	Pedido pedidoConfig
	Huella string // la del plan que el usuario vio y confirmó
}

// alfabetoTrabajo es todo lo que puede aparecer en un trabajo. Ni espacios, ni
// comillas, ni barras invertidas, ni nada que una línea de comandos pueda
// leer de otra manera.
func alfabetoTrabajo(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("_.:,;=@|*/-", c) >= 0
}

// codificarJob escribe el trabajo en UN texto sin espacios, apto como
// argumento. La forma es canónica: un mismo trabajo se escribe siempre igual.
// Falla si el trabajo no cumple su propia gramática (no se manda algo que el
// hijo va a rechazar).
func codificarJob(job trabajo) (string, error) {
	var b strings.Builder
	b.WriteString(versionTrabajo)
	campo := func(k, v string) { b.WriteString(";" + k + "=" + v) }
	switch job.Clase {
	case trabajoCierre:
		campo("do", "cierre")
		campo("ports", specDe(job.Puertos))
		campo("flags", boolBit(job.Tree)+boolBit(job.Force)+boolBit(job.TCP)+boolBit(job.UDP)+boolBit(job.AllStates))
		campo("to", strconv.FormatInt(job.Timeout.Milliseconds(), 10))
		if len(job.Kill) > 0 {
			campo("kill", idsEncoded(job.Kill))
		}
		if len(job.Servicios) > 0 {
			svcs := make([]string, len(job.Servicios))
			for i, s := range job.Servicios {
				svcs[i] = b64(s.Nombre) + "@" + idsEncoded([]reaper.ID{s.Owner})
			}
			campo("svc", strings.Join(svcs, ","))
		}
		if len(job.Manual) > 0 {
			ms := make([]string, len(job.Manual))
			for i, m := range job.Manual {
				ms[i] = b64(m)
			}
			campo("manual", strings.Join(ms, ","))
		}
		if len(job.Reglas) > 0 {
			rs := make([]string, len(job.Reglas))
			for i, r := range job.Reglas {
				rs[i] = r.Kind.String() + "|" + r.ListenAddr + "|" + strconv.Itoa(int(r.ListenPort)) + "|" + r.Target
			}
			campo("rule", strings.Join(rs, ","))
		}
	case trabajoConfig:
		campo("do", "config")
		if !job.Pedido.Reservar.IsEmpty() {
			campo("res", specDe(job.Pedido.Reservar))
		}
		if !job.Pedido.Liberar.IsEmpty() {
			campo("unres", specDe(job.Pedido.Liberar))
		}
		if job.Pedido.Dinamico {
			campo("dyn", "1")
		}
		campo("proto", boolBit(job.Pedido.TCP)+boolBit(job.Pedido.UDP))
		campo("huella", job.Huella)
	default:
		return "", fmt.Errorf("trabajo sin clase")
	}
	s := b.String()
	if _, err := decodificarJob(s); err != nil {
		return "", fmt.Errorf("el trabajo no se puede mandar: %w", err)
	}
	return s, nil
}

// campoTrabajo es un campo de la gramática.
type campoTrabajo struct {
	clave       string
	obligatorio bool
}

// camposDe son los campos que admite cada clase de trabajo, EN ORDEN. El
// orden es parte de la gramática: un trabajo se escribe de una sola manera, y
// un texto que no está escrito así no es un trabajo.
var camposDe = map[string][]campoTrabajo{
	"cierre": {{"ports", true}, {"flags", true}, {"to", true}, {"kill", false}, {"svc", false}, {"manual", false}, {"rule", false}},
	"config": {{"res", false}, {"unres", false}, {"dyn", false}, {"proto", true}, {"huella", true}},
}

// decodificarJob lee un trabajo. Estricto: lo que no cumple la gramática al
// pie de la letra es un error, y un error es no hacer nada.
func decodificarJob(s string) (trabajo, error) {
	var job trabajo
	if len(s) == 0 || len(s) > maxTrabajo {
		return job, fmt.Errorf("el trabajo mide %d caracteres (el tope es %d)", len(s), maxTrabajo)
	}
	for i := 0; i < len(s); i++ {
		if !alfabetoTrabajo(s[i]) {
			return job, fmt.Errorf("el trabajo tiene un carácter que no corresponde en la posición %d", i)
		}
	}
	partes := strings.Split(s, ";")
	if partes[0] != versionTrabajo {
		return job, fmt.Errorf("versión de trabajo desconocida (%q)", partes[0])
	}
	if len(partes) < 2 {
		return job, fmt.Errorf("el trabajo no dice de qué clase es")
	}
	clase, esClase := strings.CutPrefix(partes[1], "do=")
	admitidos, conocida := camposDe[clase]
	if !esClase || !conocida {
		return job, fmt.Errorf("clase de trabajo desconocida (%q)", partes[1])
	}
	// Los campos, en el orden de la gramática: cada uno a lo sumo una vez, y
	// ninguno antes que uno que va antes.
	campos := map[string]string{}
	sigue := 0
	for _, kv := range partes[2:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return job, fmt.Errorf("el campo %q no tiene la forma clave=valor", kv)
		}
		lugar := -1
		for i := sigue; i < len(admitidos); i++ {
			if admitidos[i].clave == k {
				lugar = i
				break
			}
		}
		if lugar < 0 {
			return job, fmt.Errorf("el campo %q no corresponde ahí (repetido, fuera de orden, o no es de un trabajo de %s)", k, clase)
		}
		campos[k], sigue = v, lugar+1
	}
	for _, c := range admitidos {
		if _, ok := campos[c.clave]; c.obligatorio && !ok {
			return job, fmt.Errorf("falta el campo %q", c.clave)
		}
	}
	if clase == "config" {
		return decodificarConfig(campos)
	}
	return decodificarCierre(campos)
}

func decodificarCierre(campos map[string]string) (trabajo, error) {
	job := trabajo{Clase: trabajoCierre}
	var err error
	if job.Puertos, err = puertosDe1(campos["ports"]); err != nil {
		return job, err
	}
	flags := campos["flags"]
	if len(flags) != 5 || strings.Trim(flags, "01") != "" {
		return job, fmt.Errorf("flags: esperaba cinco bits y vino %q", flags)
	}
	job.Tree, job.Force, job.TCP, job.UDP, job.AllStates = flags[0] == '1', flags[1] == '1', flags[2] == '1', flags[3] == '1', flags[4] == '1'
	ms, err := enteroDe(campos["to"], 0, maxEspera.Milliseconds())
	if err != nil {
		return job, fmt.Errorf("to: %w", err)
	}
	job.Timeout = time.Duration(ms) * time.Millisecond

	if v, ok := campos["kill"]; ok {
		if job.Kill, err = identidadesDe(v, maxCerrar); err != nil {
			return job, fmt.Errorf("kill: %w", err)
		}
	}
	nombres := map[string]bool{}
	if v, ok := campos["svc"]; ok {
		for _, e := range strings.Split(v, ",") {
			n, id, ok := strings.Cut(e, "@")
			if !ok {
				return job, fmt.Errorf("svc: %q no tiene la forma nombre@identidad", e)
			}
			nombre, err := servicioDeB64(n)
			if err != nil {
				return job, fmt.Errorf("svc: %w", err)
			}
			ids, err := identidadesDe(id, 1)
			if err != nil {
				return job, fmt.Errorf("svc: %w", err)
			}
			if nombres[strings.ToLower(nombre)] {
				return job, fmt.Errorf("svc: el servicio %q está dos veces", nombre)
			}
			nombres[strings.ToLower(nombre)] = true
			job.Servicios = append(job.Servicios, svcObjetivo{Nombre: nombre, Owner: ids[0]})
		}
		if len(job.Servicios) > maxServicios {
			return job, fmt.Errorf("svc: son %d servicios (el tope es %d)", len(job.Servicios), maxServicios)
		}
	}
	if v, ok := campos["manual"]; ok {
		visto := map[string]bool{}
		for _, e := range strings.Split(v, ",") {
			nombre, err := servicioDeB64(e)
			if err != nil {
				return job, fmt.Errorf("manual: %w", err)
			}
			k := strings.ToLower(nombre)
			// Dejar en manual es la segunda mitad de detener: no existe para
			// un servicio que el trabajo no manda detener.
			if !nombres[k] {
				return job, fmt.Errorf("manual: %q no es uno de los servicios del trabajo", nombre)
			}
			if visto[k] {
				return job, fmt.Errorf("manual: %q está dos veces", nombre)
			}
			visto[k] = true
			job.Manual = append(job.Manual, nombre)
		}
	}
	if v, ok := campos["rule"]; ok {
		visto := map[string]bool{}
		for _, e := range strings.Split(v, ",") {
			f := strings.Split(e, "|")
			if len(f) != 4 {
				return job, fmt.Errorf("rule: %q no tiene la forma clase|dirección|puerto|destino", e)
			}
			kind, ok := ports.ParseProxyKind(f[0])
			if !ok || f[0] != kind.String() {
				return job, fmt.Errorf("rule: clase desconocida (%q)", f[0])
			}
			// El puerto, en su forma canónica: el parser de reglas tolera ceros
			// de más (lee el registro, que guarda lo que le escribieron); el
			// trabajo no.
			if _, err := enteroDe(f[2], 1, 65535); err != nil {
				return job, fmt.Errorf("rule: puerto: %w", err)
			}
			r, err := ports.ParseProxyRule(kind, f[1]+"/"+f[2], f[3])
			if err != nil {
				return job, fmt.Errorf("rule: %w", err)
			}
			// El puerto tiene que ser uno de los pedidos: una regla de otro
			// puerto no tiene nada que hacer en este trabajo.
			if !job.Puertos.Contains(r.ListenPort) {
				return job, fmt.Errorf("rule: la regla %s no es de ninguno de los puertos del trabajo", r.Key())
			}
			if visto[strings.ToLower(r.Key())] {
				return job, fmt.Errorf("rule: la regla %s está dos veces", r.Key())
			}
			visto[strings.ToLower(r.Key())] = true
			job.Reglas = append(job.Reglas, r)
		}
		if len(job.Reglas) > maxReglas {
			return job, fmt.Errorf("rule: son %d reglas (el tope es %d)", len(job.Reglas), maxReglas)
		}
	}
	if len(job.Kill)+len(job.Servicios)+len(job.Reglas) == 0 {
		return job, fmt.Errorf("el trabajo no tiene nada que hacer")
	}
	return job, nil
}

func decodificarConfig(campos map[string]string) (trabajo, error) {
	job := trabajo{Clase: trabajoConfig}
	var err error
	if v, ok := campos["res"]; ok {
		if job.Pedido.Reservar, err = puertosDe1(v); err != nil {
			return job, fmt.Errorf("res: %w", err)
		}
	}
	if v, ok := campos["unres"]; ok {
		if job.Pedido.Liberar, err = puertosDe1(v); err != nil {
			return job, fmt.Errorf("unres: %w", err)
		}
	}
	if v, ok := campos["dyn"]; ok {
		if v != "1" {
			return job, fmt.Errorf("dyn: esperaba 1 y vino %q", v)
		}
		job.Pedido.Dinamico = true
	}
	proto := campos["proto"]
	if len(proto) != 2 || strings.Trim(proto, "01") != "" {
		return job, fmt.Errorf("proto: esperaba dos bits y vino %q", proto)
	}
	job.Pedido.TCP, job.Pedido.UDP = proto[0] == '1', proto[1] == '1'
	if err := job.Pedido.validar(); err != nil {
		return job, err
	}
	job.Huella = campos["huella"]
	if len(job.Huella) != 32 || strings.Trim(job.Huella, "0123456789abcdef") != "" {
		return job, fmt.Errorf("huella: no tiene la forma de una huella")
	}
	return job, nil
}

// puertosDe1 lee un conjunto de puertos NO VACÍO en la forma canónica
// ("3000,5000-5003"): solo dígitos, comas y guiones. El parser de la línea de
// comandos acepta mucho más (URL, "localhost:3000"); acá no tiene por qué.
func puertosDe1(v string) (ports.Set, error) {
	if v == "" || strings.Trim(v, "0123456789,-") != "" {
		return ports.Set{}, fmt.Errorf("puertos: %q no tiene la forma esperada", v)
	}
	s, err := ports.ParseSpec(v)
	if err != nil {
		return ports.Set{}, err
	}
	if s.IsEmpty() {
		return ports.Set{}, fmt.Errorf("puertos: el conjunto está vacío")
	}
	if specDe(s) != v {
		return ports.Set{}, fmt.Errorf("puertos: %q no está en su forma canónica (%s)", v, specDe(s))
	}
	return s, nil
}

// enteroDe lee un entero decimal sin signo ni ceros de más, entre lo y hi.
func enteroDe(v string, lo, hi int64) (int64, error) {
	if v == "" || len(v) > 19 || strings.Trim(v, "0123456789") != "" || (len(v) > 1 && v[0] == '0') {
		return 0, fmt.Errorf("%q no es un número", v)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s está fuera de rango", v)
	}
	return n, nil
}

// identidadesDe lee una lista de identidades "PID:nanos". Un PID de 4 o menos
// es el kernel: no es una identidad que un trabajo pueda nombrar.
func identidadesDe(v string, tope int) ([]reaper.ID, error) {
	var out []reaper.ID
	visto := map[uint32]bool{}
	for _, part := range strings.Split(v, ",") {
		pid, ns, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("%q no tiene la forma PID:hora", part)
		}
		p, err := enteroDe(pid, 5, 1<<32-1)
		if err != nil {
			return nil, fmt.Errorf("PID: %w", err)
		}
		n, err := enteroDe(ns, 0, 1<<62)
		if err != nil {
			return nil, fmt.Errorf("hora de creación: %w", err)
		}
		if visto[uint32(p)] {
			return nil, fmt.Errorf("el PID %d está dos veces", p)
		}
		visto[uint32(p)] = true
		out = append(out, reaper.ID{PID: uint32(p), Created: fromNanos(n)})
	}
	if len(out) > tope {
		return nil, fmt.Errorf("son %d identidades (el tope es %d)", len(out), tope)
	}
	return out, nil
}

// b64 codifica un nombre de servicio: puede tener espacios, signos y letras
// de cualquier alfabeto, y en el trabajo solo entra el alfabeto de base64url.
func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// servicioDeB64 decodifica un nombre de servicio y lo valida.
func servicioDeB64(v string) (string, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(v)
	if err != nil {
		return "", fmt.Errorf("%q no es un nombre codificado", v)
	}
	nombre := string(raw)
	if !nombreServicioValido(nombre) {
		return "", fmt.Errorf("%q no es un nombre de servicio", nombre)
	}
	if b64(nombre) != v {
		return "", fmt.Errorf("%q no está en su forma canónica", v)
	}
	return nombre, nil
}

// nombreServicioValido: lo que el administrador de servicios acepta como
// nombre. Hasta 256 caracteres, sin barras (son los separadores de su clave
// del registro) y sin caracteres de control.
func nombreServicioValido(s string) bool {
	if s == "" || len(s) > 256 || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if r == '/' || r == '\\' || unicode.IsControl(r) || r == utf8.RuneError {
			return false
		}
	}
	return true
}

func boolBit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// --- dónde deja su informe el proceso elevado --------------------------------
//
// La ruta del archivo de resultado también llega por argumentos, y un proceso
// con token de administrador que crea un archivo donde le digan es un regalo:
// alcanza con elegir la carpeta y el nombre (un .bat en el inicio de todos los
// usuarios, un archivo que algún servicio lee) para que el informe —que lleva
// nombres de procesos y de servicios, o sea texto que un tercero puede
// fabricar— termine siendo otra cosa. Por eso el hijo no elige ni obedece:
//
//   - el NOMBRE tiene una sola forma, resultado-<PID>-<nanos>.json: ni la
//     extensión ni el nombre salen de un argumento;
//   - la CARPETA es una de las de trabajo del propio proceso elevado, o está
//     debajo de una (ver carpetasDeResultado);
//   - la ruta es absoluta, con letra de unidad y en su forma más simple: sin
//     ".", "..", barras al derecho, flujos alternativos ni nombres que Windows
//     reescribe (los que terminan en punto o en espacio).
//
// Lo que no cumple no se "arregla": no hay informe.

// nombreResultado arma el nombre del archivo de resultado.
func nombreResultado(pid uint32, nanos int64) string {
	return "resultado-" + strconv.FormatUint(uint64(pid), 10) + "-" + strconv.FormatInt(nanos, 10) + ".json"
}

// nombreResultadoValido dice si n tiene exactamente la forma de nombreResultado.
func nombreResultadoValido(n string) bool {
	resto, ok := strings.CutPrefix(n, "resultado-")
	if !ok {
		return false
	}
	if resto, ok = strings.CutSuffix(resto, ".json"); !ok {
		return false
	}
	pid, ns, ok := strings.Cut(resto, "-")
	if !ok {
		return false
	}
	_, errPID := enteroDe(pid, 1, 1<<32-1)
	_, errNS := enteroDe(ns, 1, 1<<62)
	return errPID == nil && errNS == nil
}

// maxRutaResultado acota la ruta del archivo de resultado.
const maxRutaResultado = 1024

// rutaResultadoValida dice si path es una ruta donde el proceso elevado acepta
// dejar su informe. carpetas son las carpetas permitidas, en forma canónica;
// se comparan sin distinguir mayúsculas (NTFS no las distingue).
func rutaResultadoValida(path string, carpetas []string) bool {
	if len(path) < len(`C:\x`) || len(path) > maxRutaResultado || !utf8.ValidString(path) {
		return false
	}
	if c := path[0]; !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') || path[1] != ':' || path[2] != '\\' {
		return false
	}
	tramos := strings.Split(path[3:], `\`)
	for _, t := range tramos {
		// Un tramo que termina en punto o en espacio es de los que Windows
		// reescribe; ahí caen también "." y "..".
		if t == "" || strings.HasSuffix(t, ".") || strings.HasSuffix(t, " ") || strings.ContainsAny(t, `/:*?"<>|`) {
			return false
		}
		for _, r := range t {
			if unicode.IsControl(r) || r == utf8.RuneError {
				return false
			}
		}
	}
	nombre := tramos[len(tramos)-1]
	if len(tramos) < 2 || !nombreResultadoValido(nombre) {
		return false
	}
	dir := strings.ToLower(path[:len(path)-len(nombre)-1])
	for _, c := range carpetas {
		c = strings.ToLower(strings.TrimRight(c, `\`))
		if len(c) >= len(`C:\x`) && (dir == c || strings.HasPrefix(dir, c+`\`)) {
			return true
		}
	}
	return false
}

// idsEncoded / decodeIDs: identidades como "PID:nanos", para el ayudante de
// consola y el trabajo elevado. nanos es UnixNano de la hora de creación (0 =
// sin hora).
func idsEncoded(ids []reaper.ID) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatUint(uint64(id.PID), 10) + ":" + strconv.FormatInt(nanos(id.Created), 10)
	}
	return strings.Join(parts, ",")
}

// decodeIDs es la lectura TOLERANTE, para el ayudante de consola (que recibe
// la lista de permitidos de su propio padre): lo que no se entiende se
// saltea. El trabajo elevado usa identidadesDe, que no perdona nada.
func decodeIDs(s string) []reaper.ID {
	if s == "" {
		return nil
	}
	var out []reaper.ID
	for _, part := range strings.Split(s, ",") {
		pid, ns, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		p, err1 := strconv.ParseUint(pid, 10, 32)
		n, err2 := strconv.ParseInt(ns, 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, reaper.ID{PID: uint32(p), Created: fromNanos(n)})
	}
	return out
}

func nanos(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fromNanos(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	// En UTC: es la forma canónica de una identidad (ver reaper.ID.Canon). Así
	// una identidad que fue y volvió por argumentos compara con == contra las
	// del bosque, sin depender de la zona horaria de la máquina.
	return time.Unix(0, n).UTC()
}
