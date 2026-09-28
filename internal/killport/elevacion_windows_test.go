//go:build windows

package killport

// El proceso elevado y el flujo de las acciones de configuración, contra un
// sistema simulado (redFalsa, de config_test.go): ninguna de estas pruebas
// corre un netsh que cambie algo, detiene WinNAT ni pide elevación.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

// netshDe es el netsh entero del sistema simulado: contesta las consultas
// (las tablas, en el formato y el idioma del de verdad) a partir del estado, y
// le pasa los cambios a redFalsa. consultas cuenta las lecturas; es atómico
// porque killport las corre a la vez (las ocho de red.go, cada una en su
// goroutine).
func netshDe(r *redFalsa, consultas *atomic.Int32) netshFn {
	return func(ctx context.Context, args ...string) (string, error) {
		if len(args) != 5 || args[2] != "show" {
			return r.netsh(ctx, args...)
		}
		if consultas != nil {
			consultas.Add(1)
		}
		c := combo{ports.TCP, ports.IPv4}
		if args[1] == "ipv6" {
			c.fam = ports.IPv6
		}
		if args[4] == "protocol=udp" {
			c.proto = ports.UDP
		}
		var b strings.Builder
		switch args[3] {
		case "excludedportrange":
			fmt.Fprintf(&b, "\r\nProtocolo %s Intervalos de exclusión de puertos\r\n\r\nPuerto de inicio    Puerto final      \r\n----------          --------      \r\n", c.prot())
			for _, x := range r.estado.Excl {
				if x.Proto == c.proto && x.Family == c.fam {
					marca := " "
					if x.Administered {
						marca = "*"
					}
					fmt.Fprintf(&b, "%10d  %10d     %s\r\n", x.Lo, x.Hi, marca)
				}
			}
			b.WriteString("\r\n* - Exclusiones de puertos administrados.\r\n\r\n")
		case "dynamicport":
			d, ok := r.estado.Dyn.Find(c.proto, c.fam)
			if !ok {
				return "", &errorNetsh{Codigo: 1, Dijo: "No se encuentra el elemento."}
			}
			fmt.Fprintf(&b, "\r\nIntervalo de puerto dinámico de protocolo %s\r\n---------------------------------\r\nPuerto de inicio    : %d\r\nNúmero de puertos   : %d\r\n\r\n", c.prot(), d.Start, d.Count)
		default:
			return "", &errorNetsh{Codigo: 1, Dijo: "No se encontró el comando."}
		}
		return b.String(), nil
	}
}

// sistemaDeRed arma un sistema cuya red es la simulada.
func sistemaDeRed(r *redFalsa, admin bool) *sistema {
	return &sistema{
		admin:  func() bool { return admin },
		red:    nuevaRed(netshDe(r, nil)),
		config: r,
		winnat: func() (bool, bool, error) { return r.estado.WinnatExiste, r.estado.WinnatCorriendo, nil },
		tablas: func(context.Context) ([]ports.Socket, error) {
			var socks []ports.Socket
			for c, set := range r.estado.Ocupados {
				for p := range set.All() {
					dir := "0.0.0.0"
					if c.fam == ports.IPv6 {
						dir = "::"
					}
					estado := ports.Listen
					if c.proto == ports.UDP {
						estado = ports.StateNone
					}
					socks = append(socks, sock(c.proto, dir, p, estado, 1000, ""))
				}
			}
			return socks, nil
		},
		sondeo: func(proto ports.Proto, fam ports.Family, p uint16) ports.BindResult {
			// Como el Windows de verdad: un bloque de WinNAT niega el bind, una
			// exclusión administrada no.
			if x, ok := r.estado.Excl.Find(proto, fam, p); ok && !x.Administered {
				return ports.BindForbidden
			}
			return ports.BindFree
		},
	}
}

// redConBloque es el caso clásico: el 3000 cae en un bloque de WinNAT.
func redConBloque() *redFalsa {
	r := &redFalsa{bloques: []ports.ExcludedRange{excl(tcp4, 2980, 3079, false), excl(tcp6, 2980, 3079, false), excl(udp4, 50100, 50199, false)}}
	r.estado = estadoRed{Excl: slices.Clone(r.bloques), Dyn: dinamicos(1025, 3976), Ocupados: map[combo]ports.Set{tcp4: ports.Of(8080)},
		WinnatExiste: true, WinnatCorriendo: true}
	r.estado.Excl = append(r.estado.Excl, excl(udp4, 50000, 50059, true), excl(udp6, 50000, 50059, true))
	return r
}

// capturar corre f con una terminal que escribe en un archivo (sin consola:
// no puede preguntar) y devuelve lo que salió.
func capturar(t *testing.T, f func(term *tui.Term) int) (string, int) {
	t.Helper()
	archivo, err := os.CreateTemp(t.TempDir(), "salida-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	viejo := os.Stdout
	os.Stdout = archivo
	term := tui.Open()
	code := f(term)
	term.Close()
	os.Stdout = viejo
	archivo.Close()
	b, err := os.ReadFile(archivo.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b), code
}

// El estado que lee killport del sistema simulado es el del sistema simulado:
// lo que escribe netsh, leído con los parsers de verdad.
func TestConfigLeerEstado(t *testing.T) {
	r := redConBloque()
	var consultas atomic.Int32
	sys := sistemaDeRed(r, false)
	sys.red = nuevaRed(netshDe(r, &consultas))
	e, err := leerEstadoRed(context.Background(), sys)
	if err != nil {
		t.Fatal(err)
	}
	if n := consultas.Load(); n != 8 {
		t.Errorf("netsh corrió %d consultas", n)
	}
	orden := func(x ports.Exclusions) []string {
		var out []string
		for _, e := range x {
			out = append(out, fmt.Sprint(e.Proto, e.Family, e.Range, e.Administered))
		}
		slices.Sort(out)
		return out
	}
	if !reflect.DeepEqual(orden(e.Excl), orden(r.estado.Excl)) {
		t.Errorf("exclusiones leídas %v\n  las del sistema %v", orden(e.Excl), orden(r.estado.Excl))
	}
	if !reflect.DeepEqual(e.Dyn, r.estado.Dyn) || !e.WinnatExiste || !e.WinnatCorriendo || !e.Ocupados[tcp4].Equal(ports.Of(8080)) {
		t.Errorf("estado = %+v", e)
	}
	// Cada lectura es nueva: no usa lo memoizado.
	r.estado.Excl = append(r.estado.Excl, excl(tcp4, 4000, 4000, true))
	if e, _ := leerEstadoRed(context.Background(), sys); len(e.Excl) != len(r.estado.Excl) || consultas.Load() != 16 {
		t.Errorf("la segunda lectura no vio el cambio (%d exclusiones, %d consultas)", len(e.Excl), consultas.Load())
	}
}

// Lo que NO ejecuta: mostrar el plan, no tener consola, --json, un impedimento.
func TestConfigNoCambiaSinConfirmar(t *testing.T) {
	casos := []struct {
		nombre  string
		puertos ports.Set
		o       opciones
		codigo  int
		dice    []string
	}{
		{"--list muestra el plan", ports.Of(3000), opciones{reserve: true, list: true}, cli.ExitOK, []string{
			"reservar puertos para vos", "hoy · TCP IPv4: tapado por el bloque 2980–3079 de WinNAT",
			"1. detener WinNAT", "net stop winnat",
			"netsh interface ipv4 add excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent",
			"4. volver a arrancar WinNAT", "corta por un instante la red de WSL, Docker y Hyper-V",
			"se deshace con: killport 3000 --unreserve", "solo mostré el plan",
		}},
		{"sin consola no se hace nada, ni con --yes", ports.Of(3000), opciones{reserve: true, yes: true}, cli.ExitFailure, []string{
			"No cambié nada", "hace falta una consola para confirmarlo (--yes no alcanza)",
		}},
		{"un impedimento", ports.Of(8080), opciones{reserve: true, yes: true}, cli.ExitFailure, []string{
			"No se puede", "8080 TCP IPv4 está en uso", "no se hace la mitad de un pedido",
		}},
		{"nada que hacer", ports.NewSet(ports.R(50000, 50059)), opciones{reserve: true, udp: true}, cli.ExitOK, []string{
			"50000–50059 ya está reservado en UDP IPv4", "No hay nada que hacer",
		}},
		{"el rango dinámico", ports.Set{}, opciones{fixDynamic: true, list: true}, cli.ExitOK, []string{
			"devolver el rango dinámico al de fábrica", "hoy · rango dinámico TCP IPv4: 1025–5000",
			"netsh interface ipv6 set dynamicport protocol=udp startport=49152 numberofports=16384 store=persistent",
			"se deshace con: netsh interface ipv4 set dynamicport protocol=tcp startport=1025 numberofports=3976 store=persistent",
		}},
		{"quitar una reserva ajena pide nombrarla entera", ports.Of(50010), opciones{unreserve: true, udp: true, list: true}, cli.ExitOK, []string{
			"50010 es parte de la reserva 50000–50059, que no se parte", "No hay nada que hacer",
		}},
	}
	for _, c := range casos {
		for _, admin := range []bool{false, true} {
			r := redConBloque()
			antes := slices.Clone(r.estado.Excl)
			salida, codigo := capturar(t, func(term *tui.Term) int { return correrConfig(term, sistemaDeRed(r, admin), c.puertos, c.o) })
			if codigo != c.codigo {
				t.Errorf("%s (admin %v): código %d, esperaba %d\n%s", c.nombre, admin, codigo, c.codigo, salida)
			}
			for _, dice := range c.dice {
				if !strings.Contains(salida, dice) {
					t.Errorf("%s (admin %v): la salida no dice %q\n%s", c.nombre, admin, dice, salida)
				}
			}
			if len(r.pedidos) != 0 || !reflect.DeepEqual(r.estado.Excl, antes) || !r.estado.WinnatCorriendo {
				t.Errorf("%s (admin %v): se ejecutó %q", c.nombre, admin, r.pedidos)
			}
			if strings.Contains(salida, "\x1b[") {
				t.Errorf("%s: sin consola no puede haber escapes de terminal", c.nombre)
			}
		}
	}

	// --json: el plan como datos, sin ejecutar. Sale con el código del texto
	// (X-39): pedido de hacer algo que sin consola no se confirma (ni con
	// --yes) → 3, como "No cambié nada"; con --list, 0.
	r := redConBloque()
	salida, codigo := capturar(t, func(term *tui.Term) int {
		return correrConfig(term, sistemaDeRed(r, false), ports.Of(3000), opciones{reserve: true, fixDynamic: true, json: true, yes: true})
	})
	var doc jsonConfig
	if err := json.Unmarshal([]byte(salida), &doc); err != nil || codigo != cli.ExitFailure {
		t.Fatalf("--json -y: %v (código %d, esperaba 3 como el texto)\n%s", err, codigo, salida)
	}
	if _, c := capturar(t, func(term *tui.Term) int {
		return correrConfig(term, sistemaDeRed(redConBloque(), false), ports.Of(3000), opciones{reserve: true, json: true, list: true})
	}); c != cli.ExitOK {
		t.Fatalf("--json --list: código %d", c)
	}
	if !reflect.DeepEqual(doc.Reservar, []uint16{3000}) || !doc.Dinamico || !doc.SoloPlan || !doc.PideAdmin || len(doc.Pasos) != 8 ||
		doc.Pasos[0].Clase != "detener-winnat" || doc.Pasos[7].Clase != "arrancar-winnat" || doc.Pasos[1].Resultado != "" ||
		doc.Pasos[3].Clase != "rango-dinamico" || len(doc.Avisos) != 2 || len(doc.Impedimentos) != 0 || len(doc.Huella) != 32 || len(r.pedidos) != 0 {
		t.Errorf("JSON = %s (pedidos %q)", salida, r.pedidos)
	}
}

// Confirmado y como administrador: se ejecuta, se verifica releyendo el
// sistema y se cuenta.
func TestConfigHacer(t *testing.T) {
	r := redConBloque()
	sys := sistemaDeRed(r, true)
	pedido := pedidoConfig{Reservar: ports.Of(3000)}
	estado, err := leerEstadoRed(context.Background(), sys)
	if err != nil {
		t.Fatal(err)
	}
	plan := planificarConfig(estado, pedido)
	salida, codigo := capturar(t, func(term *tui.Term) int { return hacerConfig(term, context.Background(), sys, pedido, plan) })
	if codigo != cli.ExitOK {
		t.Fatalf("código %d\n%s", codigo, salida)
	}
	if want := []string{"net stop winnat",
		"netsh interface ipv4 add excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent",
		"netsh interface ipv6 add excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent",
		"net start winnat"}; !reflect.DeepEqual(r.pedidos, want) {
		t.Errorf("se ejecutó %q", r.pedidos)
	}
	for _, dice := range []string{"Detener WinNAT", "Reservar 3000 TCP IPv4", "Volver a arrancar WinNAT",
		"3000 TCP IPv4 reservado", "3000 TCP IPv6 se deja atar", "WinNAT corriendo", "se deshace con: killport 3000 --unreserve", "Pasos hechos  4 de 4", "Verificado    5 de 5"} {
		if !strings.Contains(salida, dice) {
			t.Errorf("la salida no dice %q\n%s", dice, salida)
		}
	}

	// Un paso falla: WinNAT vuelve, lo que no se hizo se dice, y el código lo
	// refleja. La verificación no le cree al ejecutor: mira el sistema.
	r = redConBloque()
	r.falla = map[string]error{"netsh interface ipv6 add excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent": &errorNetsh{Codigo: 1, Dijo: "Acceso denegado."}}
	sys = sistemaDeRed(r, true)
	salida, codigo = capturar(t, func(term *tui.Term) int { return hacerConfig(term, context.Background(), sys, pedido, plan) })
	if codigo != cli.ExitPartial || !r.estado.WinnatCorriendo || r.pedidos[len(r.pedidos)-1] != "net start winnat" {
		t.Errorf("código %d, WinNAT corriendo %v, pedidos %q\n%s", codigo, r.estado.WinnatCorriendo, r.pedidos, salida)
	}
	for _, dice := range []string{"No pude reservar 3000 TCP IPv6", "netsh: Acceso denegado.", "No quedó: 3000 TCP IPv6 reservado", "3000 TCP IPv4 reservado", "Pasos hechos  3 de 4"} {
		if !strings.Contains(salida, dice) {
			t.Errorf("la salida no dice %q\n%s", dice, salida)
		}
	}
	// No salió entero: no se ofrece deshacer el pedido completo, sino lo que sí
	// quedó hecho, paso por paso (la exclusión de IPv4 es persistente).
	if strings.Contains(salida, "se deshace con: killport") ||
		!strings.Contains(salida, "lo que sí quedó hecho se deshace con: netsh interface ipv4 delete excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent") {
		t.Errorf("a medias, el deshacer tiene que ser el de lo hecho:\n%s", salida)
	}

	// WinNAT no vuelve a arrancar: la reserva quedó, pero la red de WSL,
	// Docker y Hyper-V no: el código no es 0 y se dice cómo arrancarlo.
	r = redConBloque()
	r.falla = map[string]error{"net start winnat": errors.New("plazo vencido esperando RUNNING")}
	sys = sistemaDeRed(r, true)
	salida, codigo = capturar(t, func(term *tui.Term) int { return hacerConfig(term, context.Background(), sys, pedido, plan) })
	if codigo != cli.ExitPartial || r.estado.WinnatCorriendo || !strings.Contains(salida, "No quedó: WinNAT corriendo") ||
		!strings.Contains(salida, "net start winnat") || strings.Contains(salida, "Verificado    5 de 5") {
		t.Errorf("WinNAT no volvió: código %d\n%s", codigo, salida)
	}
	doc := configJSON(pedido, plan, []resultadoPaso{{Paso: plan.Pasos[0], Fin: pasoHecho}, {Paso: plan.Pasos[1], Fin: pasoHecho}, {Paso: plan.Pasos[2], Fin: pasoFallo}}, nil, false, false)
	if len(doc.DeshacerHecho) != 1 || !strings.Contains(doc.DeshacerHecho[0], "ipv4 delete excludedportrange") {
		t.Errorf("JSON deshacer_hecho = %q", doc.DeshacerHecho)
	}

	// El ejecutor miente (dice que sí y no hace nada): la verificación lo caza.
	r = redConBloque()
	sys = sistemaDeRed(r, true)
	sys.config = mudo{}
	salida, codigo = capturar(t, func(term *tui.Term) int { return hacerConfig(term, context.Background(), sys, pedido, plan) })
	if codigo != cli.ExitFailure || !strings.Contains(salida, "No quedó: 3000 TCP IPv4 reservado") || !strings.Contains(salida, "Windows le sigue negando el bind a 3000") {
		t.Errorf("con un ejecutor que miente: código %d\n%s", codigo, salida)
	}
}

// mudo es un configurador que dice que sí a todo y no hace nada.
type mudo struct{}

func (mudo) detenerWinnat(context.Context, time.Time) error   { return nil }
func (mudo) arrancarWinnat(context.Context, time.Time) error  { return nil }
func (mudo) netsh(context.Context, ...string) (string, error) { return "", nil }

// rutaResultado arma la ruta canónica del archivo de resultado de una prueba:
// en la carpeta de temporales (una de las que el hijo acepta) y con un nombre
// de los que acepta.
func rutaResultado(t *testing.T) string {
	t.Helper()
	dir, err := win.FinalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, nombreResultado(1, 1))
}

// El hijo elevado deja su informe SOLO en sus carpetas de trabajo y con el
// nombre de siempre: la ruta llega por argumentos, y un proceso con token de
// administrador no crea archivos donde le digan.
func TestResultadoSoloDondeCorresponde(t *testing.T) {
	doc := jsonReporte{Libres: 1, Total: 1}
	buena := rutaResultado(t)
	escribirResultado(buena, doc)
	if back, err := leerResultado(buena); err != nil || back.Libres != 1 {
		t.Fatalf("en la carpeta de temporales no quedó el informe: %v", err)
	}
	// No pisa lo que ya existe (ni siquiera su propio informe).
	escribirResultado(buena, jsonReporte{Libres: 7})
	if back, _ := leerResultado(buena); back.Libres != 1 {
		t.Errorf("pisó un archivo que ya existía: %+v", back)
	}

	temporales := filepath.Dir(buena)
	aca, err := os.Getwd() // la carpeta del paquete: ni de temporales ni de trabajo
	if err != nil {
		t.Fatal(err)
	}
	if aca, err = win.FinalPath(aca); err != nil {
		t.Fatal(err)
	}
	if rutaResultadoValida(filepath.Join(aca, nombreResultado(2, 2)), carpetasDeResultado()) {
		t.Skip("la carpeta del paquete está dentro de una carpeta de trabajo: no sirve de carpeta ajena")
	}
	for nombre, ruta := range map[string]string{
		"otra carpeta":                 filepath.Join(aca, nombreResultado(2, 2)),
		"otro nombre":                  filepath.Join(temporales, "informe.json"),
		"otra extensión":               filepath.Join(temporales, "resultado-2-2.bat"),
		"un nombre que no elige nadie": filepath.Join(temporales, "resultado-2-2.json.bat"),
		"saliendo de la carpeta":       temporales + `\..\` + nombreResultado(2, 2),
		"un flujo alternativo":         filepath.Join(temporales, nombreResultado(2, 2)) + ":oculto",
		"con barras al derecho":        strings.ReplaceAll(filepath.Join(temporales, nombreResultado(2, 2)), `\`, "/"),
		"relativa":                     nombreResultado(2, 2),
		"vacía":                        "",
		// Las rutas de red y de dispositivo se prueban en TestRutaResultado,
		// que no toca el sistema: acá habría que salir a buscar un servidor.
	} {
		escribirResultado(ruta, doc)
		if ruta == "" {
			continue
		}
		if _, err := os.Lstat(strings.TrimSuffix(ruta, ":oculto")); err == nil {
			os.Remove(ruta)
			t.Errorf("%s: el hijo escribió en %q", nombre, ruta)
		}
	}
	if quedaron, _ := filepath.Glob(filepath.Join(temporales, "*")); len(quedaron) != 1 {
		t.Errorf("en la carpeta quedó %q", quedaron)
	}
}

// El padre lee del informe hasta un tope: el archivo está en una carpeta donde
// escribe cualquier programa del usuario.
func TestResultadoAcotado(t *testing.T) {
	ruta := rutaResultado(t)
	if err := os.WriteFile(ruta, []byte(`{"libres":1,"total":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if doc, err := leerResultado(ruta); err != nil || doc.Libres != 1 {
		t.Fatalf("un informe común: %+v (%v)", doc, err)
	}
	// Justo en el tope pasa (y es JSON válido: un texto largo); un byte más, no.
	relleno := func(n int) []byte {
		cabeza, cola := `{"libres":1,"x":"`, `"}`
		return []byte(cabeza + strings.Repeat("a", n-len(cabeza)-len(cola)) + cola)
	}
	if err := os.WriteFile(ruta, relleno(maxResultado), 0o600); err != nil {
		t.Fatal(err)
	}
	if doc, err := leerResultado(ruta); err != nil || doc.Libres != 1 {
		t.Errorf("un informe de %d bytes: %v", maxResultado, err)
	}
	if err := os.WriteFile(ruta, relleno(maxResultado+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := leerResultado(ruta); err == nil || !strings.Contains(err.Error(), "demasiado grande") {
		t.Errorf("un informe de %d bytes: %v", maxResultado+1, err)
	}
	if _, err := leerResultadoConfig(ruta); err == nil {
		t.Error("el informe de configuración se lee sin tope")
	}
	// Lo que no es JSON, o no está: error, no un informe vacío.
	os.WriteFile(ruta, []byte("esto no es un informe"), 0o600)
	if _, err := leerResultado(ruta); err == nil {
		t.Error("un archivo que no es JSON pasó por informe")
	}
	os.Remove(ruta)
	if _, err := leerResultado(ruta); err == nil {
		t.Error("un archivo que no existe pasó por informe")
	}
}

// El hijo elevado de una acción de configuración: relee, replanifica y
// ejecuta SOLO si el plan es el que el usuario confirmó.
func TestConfigElevado(t *testing.T) {
	pedido := pedidoConfig{Reservar: ports.Of(3000)}
	huellaDe := func(r *redFalsa) string { return planificarConfig(r.estado, pedido).huella() }
	trabajoDe := func(huella string) string {
		texto, err := codificarJob(trabajo{Clase: trabajoConfig, Pedido: pedido, Huella: huella})
		if err != nil {
			t.Fatal(err)
		}
		return texto
	}
	correr := func(r *redFalsa, admin bool, texto string) (jsonConfig, int) {
		ruta := rutaResultado(t)
		code := correrElevado(sistemaDeRed(r, admin), opciones{elevatedJob: texto, resultPath: ruta})
		doc, err := leerResultadoConfig(ruta)
		if err != nil {
			t.Fatalf("sin resultado: %v", err)
		}
		return doc, code
	}

	// 1. El plan es el confirmado: se ejecuta entero.
	r := redConBloque()
	doc, code := correr(r, true, trabajoDe(huellaDe(r)))
	if code != cli.ExitOK || doc.Error != "" || doc.SoloPlan || len(doc.Pasos) != 4 || len(r.pedidos) != 4 {
		t.Fatalf("código %d, doc %+v, pedidos %q", code, doc, r.pedidos)
	}
	for i, p := range doc.Pasos {
		if p.Resultado != "hecho" {
			t.Errorf("paso %d = %+v", i, p)
		}
	}
	if x, ok := r.estado.Excl.Find(ports.TCP, ports.IPv4, 3000); !ok || !x.Administered || !r.estado.WinnatCorriendo {
		t.Errorf("no quedó reservado: %+v", r.estado)
	}

	// 2. La red cambió desde que el usuario confirmó (WinNAT soltó el bloque:
	// ahora el plan no necesita detenerlo): la huella no coincide y no se hace
	// NADA. Ni siquiera la parte que sería igual.
	r = redConBloque()
	confirmada := huellaDe(r)
	r.estado.Excl = slices.DeleteFunc(r.estado.Excl, func(x ports.ExcludedRange) bool { return x.Proto == ports.TCP })
	doc, code = correr(r, true, trabajoDe(confirmada))
	if code != cli.ExitFailure || !strings.Contains(doc.Error, "la red cambió") || len(r.pedidos) != 0 || !doc.SoloPlan {
		t.Errorf("con la red cambiada: código %d, doc %+v, pedidos %q", code, doc, r.pedidos)
	}

	// 2b. La huella es la de OTRO pedido (el usuario confirmó reservar el 3001
	// y el trabajo dice 3000): nada. Es lo que impide cambiarle el pedido a un
	// plan confirmado.
	r = redConBloque()
	del3001 := planificarConfig(r.estado, pedidoConfig{Reservar: ports.Of(3001)}).huella()
	if doc, code = correr(r, true, trabajoDe(del3001)); code != cli.ExitFailure || len(r.pedidos) != 0 || !strings.Contains(doc.Error, "la red cambió") {
		t.Errorf("con la huella de otro pedido: código %d, doc %+v, pedidos %q", code, doc, r.pedidos)
	}

	// 3. Una huella inventada, aunque tenga la forma: nada.
	r = redConBloque()
	if doc, code = correr(r, true, trabajoDe(strings.Repeat("0", 32))); code != cli.ExitFailure || len(r.pedidos) != 0 || doc.Error == "" {
		t.Errorf("con una huella inventada: código %d, pedidos %q", code, r.pedidos)
	}

	// 4. No corre como administrador: nada (no intenta "a ver si pasa").
	r = redConBloque()
	if doc, code = correr(r, false, trabajoDe(huellaDe(r))); code != cli.ExitFailure || len(r.pedidos) != 0 || !strings.Contains(doc.Error, "administrador") {
		t.Errorf("sin ser administrador: código %d, doc %+v, pedidos %q", code, doc, r.pedidos)
	}

	// 5. Un plan con impedimento (el puerto se ocupó mientras tanto): nada.
	r = redConBloque()
	confirmada = huellaDe(r)
	r.estado.Ocupados[tcp6] = ports.Of(3000)
	if doc, code = correr(r, true, trabajoDe(confirmada)); code != cli.ExitFailure || len(r.pedidos) != 0 || doc.Error == "" {
		t.Errorf("con el puerto ocupado: código %d, doc %+v, pedidos %q", code, doc, r.pedidos)
	}

	// 6. Un trabajo que no cumple la gramática: ni se lee el sistema.
	r = redConBloque()
	var consultas atomic.Int32
	sys := sistemaDeRed(r, true)
	sys.red = nuevaRed(netshDe(r, &consultas))
	for _, texto := range []string{"", "v1;do=config", trabajoDe(huellaDe(r)) + ";paso=net-stop-http", "v2;do=config;res=3000;proto=00;huella=" + strings.Repeat("g", 32)} {
		if code := correrElevado(sys, opciones{elevatedJob: texto, resultPath: rutaResultado(t)}); code != cli.ExitUsage || consultas.Load() != 0 || len(r.pedidos) != 0 {
			t.Errorf("trabajo %q: código %d, consultas %d, pedidos %q", texto, code, consultas.Load(), r.pedidos)
		}
	}
}

// El padre acomoda lo que dice el hijo sobre los pasos que el usuario
// confirmó, y si el hijo informa otra cosa no le cree.
func TestConfigResultadoDelHijo(t *testing.T) {
	r := redConBloque()
	pedido := pedidoConfig{Reservar: ports.Of(3000)}
	plan := planificarConfig(r.estado, pedido)
	res := ejecutarConfig(context.Background(), r, plan, relojFijo(), nil)
	doc := configJSON(pedido, plan, res, nil, false, false)
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var back jsonConfig
	if err := json.Unmarshal(b, &back); err != nil || len(back.Pasos) != 4 || back.Pasos[2].Resultado != "hecho" || back.Pasos[2].MS <= 0 ||
		back.Pasos[1].Comando != plan.Pasos[1].comando() || back.Huella != plan.huella() {
		t.Errorf("ida y vuelta = %+v (%v)", back, err)
	}
	for _, clave := range []string{`"pasos"`, `"ya_estaba"`, `"avisos"`, `"impedimentos"`, `"deshacer"`, `"pide_admin"`, `"solo_plan"`, `"huella"`, `"reservar":[3000]`} {
		if !strings.Contains(string(b), clave) {
			t.Errorf("el JSON no tiene %s: %s", clave, b)
		}
	}
}

// Lo que el padre toma del informe del hijo: cómo terminó cada paso DEL PLAN
// que el usuario confirmó. Qué pasos son no sale del informe.
func TestConfigInformeDelHijo(t *testing.T) {
	r := redConBloque()
	pedido := pedidoConfig{Reservar: ports.Of(3000)}
	plan := planificarConfig(r.estado, pedido)
	if len(plan.Pasos) != 4 {
		t.Fatalf("plan = %v", pasosTexto(plan))
	}
	informe := func(f func(*jsonConfig)) jsonConfig {
		doc := configJSON(pedido, plan, ejecutarConfig(context.Background(), redConBloque(), plan, relojFijo(), nil), nil, false, false)
		if f != nil {
			f(&doc)
		}
		return doc
	}
	fin := func(res []resultadoPaso) string {
		out := make([]string, len(res))
		for i, r := range res {
			out[i] = r.Fin.clave()
		}
		return strings.Join(out, " ")
	}

	// El informe de una corrida buena.
	res, falla := pasosInformados(plan, informe(nil))
	if fin(res) != "hecho hecho hecho hecho" || falla != "" {
		t.Fatalf("informe bueno: %q (%q)", fin(res), falla)
	}
	for i, x := range res {
		if x.Paso != plan.Pasos[i] || x.Tardo <= 0 {
			t.Errorf("el paso %d = %+v", i, x)
		}
	}

	casos := []struct {
		nombre string
		cambia func(*jsonConfig)
		want   string
	}{
		{"un paso falló", func(d *jsonConfig) { d.Pasos[2].Resultado, d.Pasos[2].Error = "error", "netsh: Acceso denegado." }, "hecho hecho error hecho"},
		{"un paso que no se intentó", func(d *jsonConfig) { d.Pasos[3].Resultado = "salteado" }, "hecho hecho hecho salteado"},
		{"un resultado que no existe", func(d *jsonConfig) { d.Pasos[0].Resultado = "formateado" }, "salteado hecho hecho hecho"},
		// El hijo informa pasos que no son los del plan: de esos no se le cree nada.
		{"otra clase en un lugar", func(d *jsonConfig) { d.Pasos[1].Clase = "rango-dinamico" }, "hecho salteado hecho hecho"},
		{"las clases corridas de lugar", func(d *jsonConfig) { d.Pasos[0], d.Pasos[3] = d.Pasos[3], d.Pasos[0] }, "salteado hecho hecho salteado"},
		{"un paso de más", func(d *jsonConfig) { d.Pasos = append(d.Pasos, d.Pasos[1]) }, "salteado salteado salteado salteado"},
		{"un paso de menos", func(d *jsonConfig) { d.Pasos = d.Pasos[:3] }, "salteado salteado salteado salteado"},
		{"sin pasos", func(d *jsonConfig) { d.Pasos = nil }, "salteado salteado salteado salteado"},
	}
	for _, c := range casos {
		res, _ := pasosInformados(plan, informe(c.cambia))
		if fin(res) != c.want {
			t.Errorf("%s: %q, esperaba %q", c.nombre, fin(res), c.want)
		}
		// Pase lo que pase, los pasos son los del plan, en su orden.
		for i, x := range res {
			if x.Paso != plan.Pasos[i] {
				t.Errorf("%s: el paso %d es %+v y el del plan es %+v", c.nombre, i, x.Paso, plan.Pasos[i])
			}
		}
	}

	// Los textos del informe se limpian, y un tiempo disparatado no se muestra.
	res, falla = pasosInformados(plan, informe(func(d *jsonConfig) {
		d.Error = "la red cambió\x1b[2J\r\nformat c:"
		d.Pasos[1].Resultado, d.Pasos[1].Error, d.Pasos[1].MS = "error", "linea uno\nlinea dos\x07", 1<<40
		d.Pasos[2].MS = -5
	}))
	if strings.ContainsAny(falla, "\x1b\r\n") || !strings.Contains(falla, "la red cambió") {
		t.Errorf("falla = %q", falla)
	}
	if e := res[1].Err; e == nil || strings.ContainsAny(e.Error(), "\n\x07") || e.Error() != "linea uno linea dos" {
		t.Errorf("error del paso = %q", e)
	}
	if res[1].Tardo != 0 || res[2].Tardo != 0 {
		t.Errorf("tiempos = %v y %v", res[1].Tardo, res[2].Tardo)
	}
}

// soloIdentidades, la última puerta del hijo elevado: del plan que él mismo
// armó pasa SOLO lo que el trabajo nombra, y de los externos solo las reglas.
func TestSoloIdentidades(t *testing.T) {
	id := func(pid uint32) reaper.ID { return reaper.ID{PID: pid, Created: t0} }
	regla3000, _ := ports.ParseProxyRule(ports.V4ToV4, "0.0.0.0/3000", "172.24.1.5/3000")
	regla8080, _ := ports.ParseProxyRule(ports.V4ToV4, "0.0.0.0/8080", "172.24.1.5/80")
	otroDestino, _ := ports.ParseProxyRule(ports.V4ToV4, "0.0.0.0/3000", "10.0.0.66/3000")
	plan := reaper.Plan{
		Targets: []reaper.Target{
			{Root: id(100), Name: "node.exe", Kind: reaper.KillProcess, Ports: ports.Of(5173), Victims: []reaper.ID{id(100)}},
			{Root: id(200), Name: "viejo.exe", Kind: reaper.KillPhantom, Ports: ports.Of(5174), Victims: []reaper.ID{id(200)}},
			{Root: id(300), Name: "mysqld.exe", Kind: reaper.StopThirdSvc, Service: "MySQL80", Ports: ports.Of(3306), Victims: []reaper.ID{id(300)}},
			{Kind: reaper.StopExternal, External: externoDeRegla(regla3000), Ports: ports.Of(3000)},
			{Kind: reaper.StopExternal, External: externoDeRegla(regla8080), Ports: ports.Of(8080)},
			{Kind: reaper.StopExternal, External: reaper.External{Kind: reaper.ExtContainer, Key: "abc", Name: "db-1", Ref: &contenedor{ID: "abc", Nombre: "db-1"}}, Ports: ports.Of(5432)},
			{Kind: reaper.StopExternal, External: reaper.External{Kind: reaper.ExtLinux, Key: "Ubuntu/7@1", Name: "node", Ref: &procesoLinux{Distro: "Ubuntu", PID: 7}}, Ports: ports.Of(4000)},
		},
		Refusals: []reaper.Refusal{{PID: 4, Name: "System", Ports: ports.Of(445), Reason: "es compartir archivos"}},
	}
	queda := func(job trabajo) string {
		var out []string
		podado := soloIdentidades(plan, job)
		for _, tg := range podado.Targets {
			if tg.Kind == reaper.StopExternal {
				out = append(out, tg.External.Key)
			} else {
				out = append(out, tg.Name)
			}
		}
		if len(podado.Refusals) != 1 {
			t.Errorf("las negativas no se tocan: %+v", podado.Refusals)
		}
		return strings.Join(out, " ")
	}
	casos := []struct {
		nombre string
		job    trabajo
		want   string
	}{
		{"nada", trabajo{}, ""},
		{"un proceso", trabajo{Kill: []reaper.ID{id(100)}}, "node.exe"},
		{"un heredero", trabajo{Kill: []reaper.ID{id(200)}}, "viejo.exe"},
		{"el mismo PID, otra identidad", trabajo{Kill: []reaper.ID{{PID: 100, Created: t0.Add(time.Second)}}}, ""},
		{"el proceso de un servicio no se cierra como proceso", trabajo{Kill: []reaper.ID{id(300)}}, ""},
		{"un servicio", trabajo{Servicios: []svcObjetivo{{Nombre: "mysql80", Owner: id(300)}}}, "mysqld.exe"},
		{"un servicio que corre otro proceso", trabajo{Servicios: []svcObjetivo{{Nombre: "MySQL80", Owner: id(301)}}}, ""},
		{"otro servicio", trabajo{Servicios: []svcObjetivo{{Nombre: "Spooler", Owner: id(300)}}}, ""},
		{"una regla", trabajo{Reglas: []ports.ProxyRule{regla3000}}, "portproxy/v4tov4/0.0.0.0/3000"},
		{"la regla, con otro destino", trabajo{Reglas: []ports.ProxyRule{otroDestino}}, ""},
		// Un contenedor o un proceso Linux no se detienen mejor por ser
		// administrador: el hijo no los toca ni aunque estén en el plan.
		{"todo lo que el trabajo puede nombrar", trabajo{Kill: []reaper.ID{id(100), id(200)}, Servicios: []svcObjetivo{{Nombre: "MySQL80", Owner: id(300)}},
			Reglas: []ports.ProxyRule{regla3000, regla8080}},
			"node.exe viejo.exe mysqld.exe portproxy/v4tov4/0.0.0.0/3000 portproxy/v4tov4/0.0.0.0/8080"},
	}
	for _, c := range casos {
		if got := queda(c.job); got != c.want {
			t.Errorf("%s: pasó %q, esperaba %q", c.nombre, got, c.want)
		}
	}
	if len(plan.Targets) != 7 {
		t.Errorf("soloIdentidades cambió el plan que recibió: %d objetivos", len(plan.Targets))
	}
}

// --- el hijo elevado y http.sys -------------------------------------------------

// Un proceso detrás de http.sys que el padre no pudo cerrar: el hijo elevado
// lo encuentra mirando ÉL las colas, y lo cierra solo si el trabajo lo nombra.
func TestElevadoDetrasDeHTTP(t *testing.T) {
	ajeno := reaper.Proc{ID: reaper.ID{PID: 7000, Created: t0}, Name: "servidor.exe", Parent: pidServices, Path: `C:\apps\servidor.exe`}
	otro := reaper.Proc{ID: reaper.ID{PID: 7001, Created: t0}, Name: "otro.exe", Parent: 3000, Path: `C:\apps\otro.exe`}
	servicio := reaper.Proc{ID: reaper.ID{PID: 7002, Created: t0}, Name: "ReportingServicesService.exe", Parent: pidServices,
		Path: `C:\Program Files\SSRS\ReportingServicesService.exe`}
	host := svchostGenuino(1480)
	forest := reaper.NewForest(conWindows(ajeno, otro, servicio, host))
	svc := map[uint32][]reaper.Service{7002: {{Name: "SQLServerReportingServices"}}, 1480: {{Name: "WAS"}}}
	socks := []ports.Socket{sock(ports.TCP, "0.0.0.0", 80, ports.Listen, 4, "System"), sock(ports.TCP, "0.0.0.0", 445, ports.Listen, 4, "System")}
	pedidos := ports.Of(80, 445)
	url := func(camino string) []ports.QueueURL {
		return []ports.QueueURL{{Raw: "HTTP://+:80/" + camino, Host: "+", Port: 80, Path: "/" + camino}}
	}
	colas := []ports.RequestQueue{
		{Processes: []ports.QueueProcess{{PID: 7000, Image: ajeno.Path}}, URLs: url("A/")},
		{Processes: []ports.QueueProcess{{PID: 7001, Image: otro.Path}}, URLs: url("B/")},
		{Processes: []ports.QueueProcess{{PID: 7002, Image: servicio.Path}}, URLs: url("REPORTS/")},
		{Controller: ports.QueueProcess{PID: 1480}, URLs: url("IIS/")},
	}
	planear := func(job trabajo, visto detras) reaper.Plan {
		job.Puertos = pedidos
		return planElevado(job, holding(socks, ports.Filter{}, pedidos), forest, svc, cfgC2, visto)
	}
	raices := func(pl reaper.Plan) string {
		var out []string
		for _, tg := range pl.Targets {
			out = append(out, u32(tg.Root.PID)+":"+tipoObjetivo(tg)+":"+tg.ViaName)
		}
		return strings.Join(out, " ")
	}

	if got := raices(planear(trabajo{Kill: []reaper.ID{ajeno.ID}}, detras{colas: colas})); got != "7000:proceso:System" {
		t.Errorf("objetivos = %q", got)
	}
	// El servicio de terceros se detiene como servicio, también detrás de http.sys.
	job := trabajo{Servicios: []svcObjetivo{{Nombre: "SQLServerReportingServices", Owner: servicio.ID}}}
	if got := raices(planear(job, detras{colas: colas})); got != "7002:servicio:System" {
		t.Errorf("objetivos = %q", got)
	}
	for nombre, c := range map[string]struct {
		job   trabajo
		visto detras
	}{
		"sin mirar las colas":                     {trabajo{Kill: []reaper.ID{ajeno.ID}}, detras{}},
		"las colas no lo nombran":                 {trabajo{Kill: []reaper.ID{ajeno.ID}}, detras{colas: colas[1:]}},
		"es otro proceso con ese PID (reciclado)": {trabajo{Kill: []reaper.ID{{PID: 7000, Created: t0.Add(time.Second)}}}, detras{colas: colas}},
		"el svchost, aunque el trabajo lo nombre": {trabajo{Kill: []reaper.ID{host.ID}}, detras{colas: colas}},
		"el kernel, aunque el trabajo lo nombre":  {trabajo{Kill: []reaper.ID{{PID: 4}, sistemaID}}, detras{colas: colas}},
		"un servicio que el trabajo no nombra":    {trabajo{Servicios: []svcObjetivo{{Nombre: "Spooler", Owner: servicio.ID}}}, detras{colas: colas}},
		"sin nada en el trabajo":                  {trabajo{}, detras{colas: colas}},
	} {
		if pl := planear(c.job, c.visto); len(pl.Targets) != 0 {
			t.Errorf("%s: objetivos = %q", nombre, raices(pl))
		}
	}
	// El 445 (compartir archivos) no pasa por ninguna puerta.
	for _, tg := range planear(trabajo{Kill: []reaper.ID{ajeno.ID, otro.ID}}, detras{colas: colas}).Targets {
		if tg.Ports.Contains(445) {
			t.Errorf("objetivo sobre el 445: %+v", tg)
		}
	}
}

// Lo que vuelve del hijo elevado sobre una regla de portproxy: manda lo que se
// ve en el registro, no lo que dice el informe.
func TestFusionarExternos(t *testing.T) {
	regla, _ := ports.ParseProxyRule(ports.V4ToV4, "0.0.0.0/3000", "172.24.1.5/3000")
	x := externoDeRegla(regla)
	cont := reaper.External{Kind: reaper.ExtContainer, Key: "abc", Name: "db-1", Ref: &contenedor{ID: "abc", Nombre: "db-1"}}
	orig := func() []reaper.ExternalResult {
		return []reaper.ExternalResult{
			{External: x, Outcome: reaper.OutcomeDenied, Err: errPideAdmin},
			{External: cont, Outcome: reaper.OutcomeClosed, Method: reaper.MethodEngine, Elapsed: time.Second},
			{External: cont, Outcome: reaper.OutcomeDenied},
		}
	}
	borrada := func(r *registroFalso) { r.claves[claveV4V4] = r.claves[claveV4V4][:1] }
	dijoQueSi := jsonReporte{Externos: []jsonExterno{
		{Tipo: "regla", Clave: x.Key, Destino: "cerrado", MS: 120},
		{Tipo: "contenedor", Clave: "abc", Destino: "cerrado"},
	}}
	dijoQueNo := jsonReporte{Externos: []jsonExterno{{Clave: x.Key, Destino: "error", Error: "la regla cambió\nmientras tanto\x1b[2J"}}}
	casos := []struct {
		nombre string
		armar  func(*registroFalso)
		hijo   jsonReporte
		fin    reaper.Outcome
		metodo reaper.Method
		tardo  time.Duration
		dice   string
	}{
		{"dijo que la borró, y no está", borrada, dijoQueSi, reaper.OutcomeClosed, reaper.MethodNetsh, 120 * time.Millisecond, ""},
		{"dijo que la borró, y SIGUE", nil, dijoQueSi, reaper.OutcomeError, reaper.MethodNone, 0, "la regla sigue en el registro"},
		{"dijo que no pudo", nil, dijoQueNo, reaper.OutcomeError, reaper.MethodNone, 0, "la regla cambió mientras tanto"},
		{"dijo que no pudo, y no está", borrada, dijoQueNo, reaper.OutcomeClosed, reaper.MethodNetsh, 0, ""},
		{"no dejó informe, y no está", borrada, jsonReporte{}, reaper.OutcomeClosed, reaper.MethodNetsh, 0, ""},
		{"no dejó informe, y sigue", nil, jsonReporte{}, reaper.OutcomeDenied, reaper.MethodNone, 0, "administrador"},
		{"no se puede leer el registro", func(r *registroFalso) { r.err = errors.New("registro roto") }, dijoQueSi,
			reaper.OutcomeError, reaper.MethodNone, 0, "registro roto"},
	}
	for _, c := range casos {
		reg := registroDePrueba()
		if c.armar != nil {
			c.armar(reg)
		}
		got := fusionarExternos(&sistema{proxy: reg.cliente(false)}, orig(), c.hijo)
		if got[0].Outcome != c.fin || got[0].Method != c.metodo || got[0].Elapsed != c.tardo {
			t.Errorf("%s: %v / %v / %v (%v)", c.nombre, got[0].Outcome, got[0].Method, got[0].Elapsed, got[0].Err)
		}
		if (c.dice == "") != (got[0].Err == nil) || (got[0].Err != nil && !strings.Contains(got[0].Err.Error(), c.dice)) {
			t.Errorf("%s: error = %v, esperaba %q", c.nombre, got[0].Err, c.dice)
		}
		if got[0].Err != nil && strings.ContainsAny(got[0].Err.Error(), "\x1b\n") {
			t.Errorf("%s: se coló un carácter de control del informe: %q", c.nombre, got[0].Err)
		}
		// Lo que no estaba denegado no se toca, y un contenedor denegado
		// tampoco: no viajó, así que el informe no tiene nada que decir de él.
		if got[1].Outcome != reaper.OutcomeClosed || got[1].Method != reaper.MethodEngine || got[1].Elapsed != time.Second || got[2].Outcome != reaper.OutcomeDenied {
			t.Errorf("%s: tocó lo que no era suyo: %+v / %+v", c.nombre, got[1], got[2])
		}
		if len(reg.pedidos) != 0 {
			t.Errorf("%s: el padre corrió netsh: %q", c.nombre, reg.pedidos)
		}
	}
}

// Lo que vuelve del hijo elevado sobre los procesos: que una víctima murió se
// comprueba en una instantánea nueva, lo haya informado el hijo o no.
func TestFusionarVictimas(t *testing.T) {
	viva := reaper.Proc{ID: reaper.ID{PID: 7000, Created: t0}, Name: "servidor.exe", Parent: 3000, Path: `C:\apps\servidor.exe`}
	reciclada := reaper.Proc{ID: reaper.ID{PID: 7001, Created: t0.Add(time.Minute)}, Name: "otro.exe", Parent: 3000, Path: `C:\apps\otro.exe`}
	despues := reaper.NewForest(conWindows(viva, reciclada))
	id := func(pid uint32) reaper.ID { return reaper.ID{PID: pid, Created: t0} }
	orig := func() []reaper.VictimResult {
		return []reaper.VictimResult{
			{ID: id(7000), Name: "servidor.exe", Outcome: reaper.OutcomeDenied, Err: reaper.ErrAccessDenied},
			{ID: id(7001), Name: "viejo.exe", Outcome: reaper.OutcomeDenied, Err: reaper.ErrAccessDenied}, // el PID ahora es de otro
			{ID: id(7002), Name: "mysqld.exe", Outcome: reaper.OutcomeDenied, Err: reaper.ErrAccessDenied},
			{ID: id(900), Name: "node.exe", Outcome: reaper.OutcomeClosed, Method: reaper.MethodCtrlC, Elapsed: time.Second},
		}
	}
	dijo := func(destinos ...string) jsonReporte {
		var doc jsonReporte
		for i, d := range destinos {
			if d != "" {
				doc.Victimas = append(doc.Victimas, jsonVictima{PID: uint32(7000 + i), Destino: d, Metodo: "Ctrl+C"})
			}
		}
		doc.Victimas = append(doc.Victimas, jsonVictima{PID: 900, Destino: "error"})
		return doc
	}
	type fin struct {
		o reaper.Outcome
		m reaper.Method
	}
	casos := []struct {
		nombre  string
		hijo    jsonReporte
		despues *reaper.Forest
		want    [3]fin
		dice    string // el error de la primera, si tiene
	}{
		{"dijo la verdad", dijo("protegido", "cerrado", "servicio_detenido"), despues,
			[3]fin{{reaper.OutcomeProtected, 0}, {reaper.OutcomeClosed, reaper.MethodCtrlC}, {reaper.OutcomeServiceStopped, reaper.MethodService}}, ""},
		{"dijo que la cerró, y sigue viva", dijo("terminado", "terminado", "terminado"), despues,
			[3]fin{{reaper.OutcomeError, 0}, {reaper.OutcomeTerminated, reaper.MethodTerminate}, {reaper.OutcomeTerminated, reaper.MethodTerminate}}, "sigue vivo"},
		{"dijo que no pudo, y ya no están", dijo("denegado", "denegado", "error"), despues,
			[3]fin{{reaper.OutcomeDenied, 0}, {reaper.OutcomeGone, 0}, {reaper.OutcomeGone, 0}}, ""},
		{"no dejó informe", jsonReporte{}, despues,
			[3]fin{{reaper.OutcomeDenied, 0}, {reaper.OutcomeGone, 0}, {reaper.OutcomeGone, 0}}, ""},
		{"dijo cualquier cosa", dijo("rm -rf", "", "format"), despues,
			[3]fin{{reaper.OutcomeError, 0}, {reaper.OutcomeGone, 0}, {reaper.OutcomeGone, 0}}, ""},
		// Sin instantánea queda lo que dijo el hijo (los puertos se verifican igual).
		{"sin instantánea", dijo("terminado", "", "denegado"), nil,
			[3]fin{{reaper.OutcomeTerminated, reaper.MethodTerminate}, {reaper.OutcomeDenied, 0}, {reaper.OutcomeDenied, 0}}, ""},
		{"sin instantánea ni informe", jsonReporte{}, nil,
			[3]fin{{reaper.OutcomeDenied, 0}, {reaper.OutcomeDenied, 0}, {reaper.OutcomeDenied, 0}}, ""},
	}
	for _, c := range casos {
		got := fusionarVictimas(orig(), c.hijo, c.despues)
		for i, w := range c.want {
			if got[i].Outcome != w.o || got[i].Method != w.m {
				t.Errorf("%s: la víctima %d quedó %v / %v, esperaba %v / %v", c.nombre, got[i].ID.PID, got[i].Outcome, got[i].Method, w.o, w.m)
			}
			if got[i].Outcome.Done() && got[i].Err != nil {
				t.Errorf("%s: la víctima %d se fue y conserva el error %v", c.nombre, got[i].ID.PID, got[i].Err)
			}
		}
		if c.dice != "" && (got[0].Err == nil || !strings.Contains(got[0].Err.Error(), c.dice)) {
			t.Errorf("%s: error = %v", c.nombre, got[0].Err)
		}
		// La que el padre cerró por su cuenta no se toca, diga lo que diga el informe.
		if got[3].Outcome != reaper.OutcomeClosed || got[3].Method != reaper.MethodCtrlC || got[3].Elapsed != time.Second {
			t.Errorf("%s: tocó una víctima que no estaba denegada: %+v", c.nombre, got[3])
		}
	}
}

// --manual en el hijo elevado: que el trabajo nombre un servicio no alcanza.
// Se le cambia el inicio solo al que ESTA corrida detuvo.
func TestManualElevado(t *testing.T) {
	plan, _, mysqld := planConServicio(t)
	detuvo := reaper.Report{Victims: []reaper.VictimResult{{ID: mysqld.ID, Name: "mysqld.exe", Outcome: reaper.OutcomeServiceStopped, Method: reaper.MethodService}}}
	noPudo := reaper.Report{Victims: []reaper.VictimResult{{ID: mysqld.ID, Name: "mysqld.exe", Outcome: reaper.OutcomeError}}}
	nuevo := func() (*scmFalso, *sistema) {
		scm := &scmFalso{
			cfg: map[string]win.ServiceConfig{
				"MySQL80":   terceros(win.StartAuto, false, 0),
				"Antivirus": {Type: win.ServiceWin32OwnProcess, Start: win.StartAuto, BinaryPath: `"C:\Program Files\AV\avsvc.exe"`},
			},
			estado: map[string]uint32{"MySQL80": detenido, "Antivirus": detenido},
		}
		return scm, &sistema{svc: scm.cliente(), admin: func() bool { return true }}
	}
	claves := func(rs []resultadoManual) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Servicio+"="+r.Fin.clave())
		}
		return strings.Join(out, " ")
	}

	// El caso de siempre: el hijo lo detuvo, y lo deja en manual.
	scm, sys := nuevo()
	rs := manualElevado(sys, trabajo{Manual: []string{"mysql80"}}, plan, detuvo, cfgC2)
	if claves(rs) != "MySQL80=manual" || !reflect.DeepEqual(scm.cambios, []string{"MySQL80"}) {
		t.Errorf("lo detuvo: %s, cambios %v", claves(rs), scm.cambios)
	}

	for nombre, c := range map[string]struct {
		job  trabajo
		plan reaper.Plan
		rep  reaper.Report
		want string
	}{
		// El ataque: un trabajo que nombra un servicio de terceros que está
		// detenido y no tiene nada que ver con los puertos pedidos.
		"un servicio que no es del plan":      {trabajo{Manual: []string{"Antivirus"}}, plan, detuvo, "Antivirus=no_lo_detuvo"},
		"el del plan, que no se pudo detener": {trabajo{Manual: []string{"MySQL80"}}, plan, noPudo, "MySQL80=no_lo_detuvo"},
		"el del plan, sin escalada":           {trabajo{Manual: []string{"MySQL80"}}, plan, reaper.Report{}, "MySQL80=no_lo_detuvo"},
		// El padre ya lo había detenido: el hijo no lo ve en ningún puerto.
		"un plan sin objetivos": {trabajo{Manual: []string{"MySQL80"}}, reaper.Plan{}, reaper.Report{}, "MySQL80=no_lo_detuvo"},
		"sin --manual":          {trabajo{}, plan, detuvo, ""},
	} {
		scm, sys := nuevo()
		if rs := manualElevado(sys, c.job, c.plan, c.rep, cfgC2); claves(rs) != c.want || len(scm.cambios) != 0 || scm.lecturas != 0 {
			t.Errorf("%s: %s (cambios %v, lecturas %d)", nombre, claves(rs), scm.cambios, scm.lecturas)
		}
	}

	// Y aunque lo haya detenido, siguen valiendo las reglas de siempre: uno que
	// volvió a arrancar mientras tanto no se toca.
	scm, sys = nuevo()
	scm.estado["MySQL80"] = corriendo
	if rs := manualElevado(sys, trabajo{Manual: []string{"MySQL80"}}, plan, detuvo, cfgC2); claves(rs) != "MySQL80=sigue_corriendo" || len(scm.cambios) != 0 {
		t.Errorf("volvió a arrancar: %s (cambios %v)", claves(rs), scm.cambios)
	}
}

// Un servicio pasa al plan del hijo elevado si coinciden el nombre Y el
// proceso que lo corre.
func TestElevadoServicioConIdentidad(t *testing.T) {
	plan, forest, mysqld := planConServicio(t)
	socks := []ports.Socket{sock(ports.TCP, "0.0.0.0", 3306, ports.Listen, 5120, "mysqld.exe")}
	svc := map[uint32][]reaper.Service{5120: {{Name: "MySQL80", Display: "MySQL80"}}}
	_ = plan
	planear := func(s svcObjetivo) reaper.Plan {
		job := trabajo{Puertos: ports.Of(3306), Servicios: []svcObjetivo{s}}
		return planElevado(job, socks, forest, svc, cfgC2, detras{})
	}
	if pl := planear(svcObjetivo{Nombre: "mysql80", Owner: mysqld.ID}); len(pl.Targets) != 1 || pl.Targets[0].Kind != reaper.StopThirdSvc {
		t.Errorf("el servicio que el usuario confirmó: %+v", pl.Targets)
	}
	for nombre, s := range map[string]svcObjetivo{
		"se reinició: lo corre otro proceso": {Nombre: "MySQL80", Owner: reaper.ID{PID: 5120, Created: t0.Add(time.Second)}},
		"otro PID":                           {Nombre: "MySQL80", Owner: reaper.ID{PID: 5121, Created: t0}},
		"sin identidad":                      {Nombre: "MySQL80"},
		"otro servicio, el mismo proceso":    {Nombre: "Spooler", Owner: mysqld.ID},
	} {
		if pl := planear(s); len(pl.Targets) != 0 {
			t.Errorf("%s: objetivos = %+v", nombre, pl.Targets)
		}
	}
}
