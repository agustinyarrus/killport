package killport

// El planificador y el ejecutor de las acciones de configuración de red,
// contra estados armados a mano y contra un sistema simulado. Ninguna de
// estas pruebas corre netsh ni toca un servicio.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
)

var (
	tcp4 = combo{ports.TCP, ports.IPv4}
	tcp6 = combo{ports.TCP, ports.IPv6}
	udp4 = combo{ports.UDP, ports.IPv4}
	udp6 = combo{ports.UDP, ports.IPv6}
)

func excl(c combo, lo, hi uint16, administrada bool) ports.ExcludedRange {
	return ports.ExcludedRange{Proto: c.proto, Family: c.fam, Range: ports.R(lo, hi), Administered: administrada}
}

// exclHTTP es la fila de un puerto que aparta http.sys mientras SSDP lo
// atiende, con lo que el adaptador sabe de ella.
func exclHTTP(c combo, p uint16) ports.ExcludedRange {
	x := excl(c, p, p, false)
	x.HTTP = ports.HTTPHold{Held: true, URL: "http://*:2869/", Account: "servicio local", What: "SSDP (UPnP), el descubrimiento de dispositivos de la red"}
	return x
}

func dinamicos(inicio uint16, cantidad int) ports.DynamicRanges {
	var out ports.DynamicRanges
	for _, c := range combos {
		out = append(out, ports.DynamicRange{Proto: c.proto, Family: c.fam, Start: inicio, Count: cantidad})
	}
	return out
}

func deFabrica() ports.DynamicRanges {
	return dinamicos(ports.DynamicDefaultStart, ports.DynamicDefaultCount)
}

// pasosTexto aplana un plan a texto, un paso por renglón.
func pasosTexto(pl planConfig) []string {
	var out []string
	for _, p := range pl.Pasos {
		out = append(out, p.comando())
	}
	return out
}

func TestConfigReservar(t *testing.T) {
	casos := []struct {
		nombre string
		estado estadoRed
		pedido pedidoConfig
		pasos  []string
		hechos int
		avisa  string // algo que tiene que decir un aviso
		impide string // algo que tiene que decir un impedimento
		vuelta string // cómo se deshace
	}{
		{
			nombre: "el puerto no lo tapa nadie: dos exclusiones y WinNAT no se toca",
			estado: estadoRed{Excl: ports.Exclusions{excl(tcp4, 55150, 55249, false)}, WinnatExiste: true, WinnatCorriendo: true},
			pedido: pedidoConfig{Reservar: ports.Of(3000)},
			pasos: []string{
				"netsh interface ipv4 add excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent",
				"netsh interface ipv6 add excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent",
			},
			vuelta: "killport 3000 --unreserve",
		},
		{
			nombre: "lo tapa un bloque de WinNAT: se lo detiene, se reserva y se lo vuelve a arrancar",
			estado: estadoRed{Excl: ports.Exclusions{excl(tcp4, 2980, 3079, false)}, WinnatExiste: true, WinnatCorriendo: true},
			pedido: pedidoConfig{Reservar: ports.Of(3000)},
			pasos: []string{
				"net stop winnat",
				"netsh interface ipv4 add excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent",
				"netsh interface ipv6 add excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent",
				"net start winnat",
			},
			avisa:  "corta por un instante la red de WSL, Docker y Hyper-V",
			vuelta: "killport 3000 --unreserve",
		},
		{
			nombre: "un rango, con --udp: solo UDP",
			estado: estadoRed{WinnatCorriendo: true},
			pedido: pedidoConfig{Reservar: ports.NewSet(ports.R(5000, 5003)), UDP: true},
			pasos: []string{
				"netsh interface ipv4 add excludedportrange protocol=udp startport=5000 numberofports=4 store=persistent",
				"netsh interface ipv6 add excludedportrange protocol=udp startport=5000 numberofports=4 store=persistent",
			},
			vuelta: "killport 5000-5003 --unreserve --udp",
		},
		{
			nombre: "ya estaba reservado: nada que hacer",
			estado: estadoRed{Excl: ports.Exclusions{excl(tcp4, 3000, 3000, true), excl(tcp6, 2990, 3010, true)}, WinnatCorriendo: true},
			pedido: pedidoConfig{Reservar: ports.Of(3000)},
			hechos: 2,
		},
		{
			nombre: "reservado en una familia y en la otra no: solo la que falta",
			estado: estadoRed{Excl: ports.Exclusions{excl(tcp4, 3000, 3000, true)}, WinnatCorriendo: true},
			pedido: pedidoConfig{Reservar: ports.Of(3000)},
			pasos:  []string{"netsh interface ipv6 add excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent"},
			hechos: 1,
			// Deshacer es quitar lo que se agregó, no lo que ya estaba.
			vuelta: "netsh interface ipv6 delete excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent",
		},
		{
			nombre: "parte del rango ya estaba: se reserva lo que falta, a los dos lados",
			estado: estadoRed{Excl: ports.Exclusions{excl(tcp4, 3004, 3005, true), excl(tcp6, 3004, 3005, true)}, WinnatCorriendo: true},
			pedido: pedidoConfig{Reservar: ports.NewSet(ports.R(3000, 3010))},
			pasos: []string{
				"netsh interface ipv4 add excludedportrange protocol=tcp startport=3000 numberofports=4 store=persistent",
				"netsh interface ipv4 add excludedportrange protocol=tcp startport=3006 numberofports=5 store=persistent",
				"netsh interface ipv6 add excludedportrange protocol=tcp startport=3000 numberofports=4 store=persistent",
				"netsh interface ipv6 add excludedportrange protocol=tcp startport=3006 numberofports=5 store=persistent",
			},
			hechos: 2,
			vuelta: "netsh interface ipv4 delete excludedportrange protocol=tcp startport=3000 numberofports=4 store=persistent",
		},
		{
			nombre: "el puerto está en uso: no se hace nada, ni en la familia que sí se podía",
			estado: estadoRed{Ocupados: map[combo]ports.Set{tcp4: ports.Of(3000, 8080)}, WinnatCorriendo: true},
			pedido: pedidoConfig{Reservar: ports.Of(3000)},
			impide: "3000 TCP IPv4 está en uso",
		},
		{
			nombre: "un bloque que no es de WinNAT (está detenido): no se toca",
			estado: estadoRed{Excl: ports.Exclusions{excl(tcp4, 2980, 3079, false)}, WinnatExiste: true},
			pedido: pedidoConfig{Reservar: ports.Of(3000)},
			impide: "no es de WinNAT (que está detenido)",
		},
		{
			// El motivo tiene que ser el de verdad: acá no hay un WinNAT
			// detenido que se pueda arrancar, no hay WinNAT.
			nombre: "un bloque en una máquina sin WinNAT: no se toca, y se dice por qué",
			estado: estadoRed{Excl: ports.Exclusions{excl(tcp4, 2980, 3079, false)}},
			pedido: pedidoConfig{Reservar: ports.Of(3000)},
			impide: "no es de WinNAT (esta máquina no lo tiene)",
		},
		{
			// El 2869 que aparta http.sys mientras SSDP lo atiende: detener
			// WinNAT no lo libera. Antes el plan lo detenía igual.
			nombre: "lo aparta http.sys: WinNAT no se toca y se dice qué es",
			estado: estadoRed{Excl: ports.Exclusions{exclHTTP(tcp4, 2869), exclHTTP(tcp6, 2869), excl(tcp4, 55150, 55249, false)}, WinnatExiste: true, WinnatCorriendo: true},
			pedido: pedidoConfig{Reservar: ports.Of(2869)},
			impide: "2869 TCP IPv4 cae en la exclusión 2869, que Windows pone mientras http.sys atiende ese puerto (SSDP (UPnP), el descubrimiento de dispositivos de la red): no es de WinNAT",
		},
		{
			nombre: "lo reservó un programa: WinNAT no se toca",
			estado: estadoRed{Excl: ports.Exclusions{excl(tcp4, 47111, 47117, false), excl(tcp6, 47111, 47117, false)}, WinnatExiste: true, WinnatCorriendo: true},
			pedido: pedidoConfig{Reservar: ports.Of(47113)},
			impide: "47113 TCP IPv4 cae en la exclusión 47111–47117, que no es de WinNAT (sus bloques son de 100 puertos) ni administrada",
		},
		{
			// No se hace la mitad: el 3000 lo tapa WinNAT, pero el 47113 no se
			// puede, así que nada (ni detener WinNAT).
			nombre: "uno de WinNAT y uno de un programa: ninguno",
			estado: estadoRed{Excl: ports.Exclusions{excl(tcp4, 2980, 3079, false), excl(tcp4, 47111, 47117, false)}, WinnatExiste: true, WinnatCorriendo: true},
			pedido: pedidoConfig{Reservar: ports.Of(3000, 47113)},
			impide: "47113 TCP IPv4 cae en la exclusión 47111–47117",
		},
		{
			nombre: "un UDP ocupado no impide reservar TCP",
			estado: estadoRed{Ocupados: map[combo]ports.Set{udp4: ports.Of(3000)}, WinnatCorriendo: true},
			pedido: pedidoConfig{Reservar: ports.Of(3000)},
			pasos: []string{
				"netsh interface ipv4 add excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent",
				"netsh interface ipv6 add excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent",
			},
			vuelta: "killport 3000 --unreserve",
		},
	}
	for _, c := range casos {
		pl := planificarConfig(c.estado, c.pedido)
		if got := pasosTexto(pl); !reflect.DeepEqual(got, c.pasos) {
			t.Errorf("%s:\n  pasos %q\n  quería %q", c.nombre, got, c.pasos)
		}
		if len(pl.Hechos) != c.hechos {
			t.Errorf("%s: hechos = %q, esperaba %d", c.nombre, pl.Hechos, c.hechos)
		}
		if c.avisa != "" && !strings.Contains(strings.Join(pl.Avisos, "\n"), c.avisa) {
			t.Errorf("%s: los avisos %q no dicen %q", c.nombre, pl.Avisos, c.avisa)
		}
		if c.avisa == "" && pl.tocaWinnat() {
			t.Errorf("%s: toca WinNAT sin avisar", c.nombre)
		}
		if (c.impide != "") != (len(pl.Impedimentos) > 0) || !strings.Contains(strings.Join(pl.Impedimentos, "\n"), c.impide) {
			t.Errorf("%s: impedimentos = %q, esperaba %q", c.nombre, pl.Impedimentos, c.impide)
		}
		if pl.ejecutable() != (len(c.pasos) > 0) {
			t.Errorf("%s: ejecutable = %v", c.nombre, pl.ejecutable())
		}
		switch {
		case c.vuelta == "" && len(pl.Deshacer) > 0:
			t.Errorf("%s: no hizo nada y dice cómo deshacerlo: %q", c.nombre, pl.Deshacer)
		case c.vuelta != "" && (len(pl.Deshacer) == 0 || pl.Deshacer[0] != c.vuelta):
			t.Errorf("%s: deshacer = %q, esperaba %q", c.nombre, pl.Deshacer, c.vuelta)
		}
	}
}

func TestConfigQuitar(t *testing.T) {
	estado := estadoRed{Excl: ports.Exclusions{
		excl(tcp4, 3000, 3000, true), excl(tcp6, 3000, 3000, true),
		excl(tcp4, 5000, 5010, true),
		excl(udp4, 50000, 50059, true), // la de otro programa
		excl(tcp4, 55150, 55249, false),
	}, WinnatCorriendo: true}

	casos := []struct {
		nombre string
		pedido pedidoConfig
		pasos  []string
		hechos int
		avisa  string
	}{
		{"la reserva exacta, en las dos familias", pedidoConfig{Liberar: ports.Of(3000)}, []string{
			"netsh interface ipv4 delete excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent",
			"netsh interface ipv6 delete excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent",
		}, 0, ""},
		{"una reserva más grande que lo nombrado no se parte", pedidoConfig{Liberar: ports.Of(5005)}, nil, 1,
			"5005 es parte de la reserva 5000–5010"},
		{"nombrada entera, se quita", pedidoConfig{Liberar: ports.NewSet(ports.R(5000, 5010))}, []string{
			"netsh interface ipv4 delete excludedportrange protocol=tcp startport=5000 numberofports=11 store=persistent",
		}, 1, ""},
		{"lo que no está reservado", pedidoConfig{Liberar: ports.Of(8080)}, nil, 2, ""},
		// Sin --udp no se toca una exclusión de UDP, aunque el puerto coincida.
		{"la exclusión de UDP de otro programa no se toca sin --udp", pedidoConfig{Liberar: ports.NewSet(ports.R(50000, 50059))}, nil, 2, ""},
		{"con --udp y nombrándola entera, sí", pedidoConfig{Liberar: ports.NewSet(ports.R(50000, 50059)), UDP: true}, []string{
			"netsh interface ipv4 delete excludedportrange protocol=udp startport=50000 numberofports=60 store=persistent",
		}, 1, ""},
		// Un bloque de WinNAT no es una reserva: no se quita jamás.
		{"un bloque de WinNAT no se quita", pedidoConfig{Liberar: ports.NewSet(ports.R(55150, 55249))}, nil, 2, ""},
	}
	for _, c := range casos {
		pl := planificarConfig(estado, c.pedido)
		if got := pasosTexto(pl); !reflect.DeepEqual(got, c.pasos) {
			t.Errorf("%s:\n  pasos %q\n  quería %q", c.nombre, got, c.pasos)
		}
		if len(pl.Hechos) != c.hechos {
			t.Errorf("%s: hechos = %q, esperaba %d", c.nombre, pl.Hechos, c.hechos)
		}
		if c.avisa != "" && !strings.Contains(strings.Join(pl.Avisos, "\n"), c.avisa) {
			t.Errorf("%s: los avisos %q no dicen %q", c.nombre, pl.Avisos, c.avisa)
		}
		if pl.tocaWinnat() {
			t.Errorf("%s: quitar una reserva no toca WinNAT", c.nombre)
		}
		// Windows no guarda quién puso una exclusión: siempre que se va a quitar
		// una, se avisa que puede ser de otro programa.
		if avisa := strings.Contains(strings.Join(pl.Avisos, " "), "puede ser de otro programa"); avisa != (len(pl.Pasos) > 0) {
			t.Errorf("%s: con %d pasos, los avisos son %q", c.nombre, len(pl.Pasos), pl.Avisos)
		}
		// Quitar se deshace volviendo a agregar exactamente lo que se quitó.
		for i, p := range pl.Pasos {
			if want := strings.Replace(p.comando(), " delete ", " add ", 1); pl.Deshacer[i] != want {
				t.Errorf("%s: deshacer[%d] = %q, esperaba %q", c.nombre, i, pl.Deshacer[i], want)
			}
		}
	}
}

func TestConfigDinamico(t *testing.T) {
	viejo := dinamicos(1025, 3976) // el de Windows XP: pisa los puertos de desarrollo
	mixto := ports.DynamicRanges{
		{Proto: ports.TCP, Family: ports.IPv4, Start: 1025, Count: 3976},
		{Proto: ports.TCP, Family: ports.IPv6, Start: 49152, Count: 16384},
		{Proto: ports.UDP, Family: ports.IPv4, Start: 50000, Count: 1000}, // raro, pero no molesta
		// UDP IPv6 no se pudo leer
	}
	casos := []struct {
		nombre string
		dyn    ports.DynamicRanges
		pedido pedidoConfig
		pasos  []string
		hechos int
		avisos int
	}{
		{"de fábrica: nada que hacer", deFabrica(), pedidoConfig{Dinamico: true}, nil, 4, 0},
		{"el rango de XP: los cuatro", viejo, pedidoConfig{Dinamico: true}, []string{
			"netsh interface ipv4 set dynamicport protocol=tcp startport=49152 numberofports=16384 store=persistent",
			"netsh interface ipv6 set dynamicport protocol=tcp startport=49152 numberofports=16384 store=persistent",
			"netsh interface ipv4 set dynamicport protocol=udp startport=49152 numberofports=16384 store=persistent",
			"netsh interface ipv6 set dynamicport protocol=udp startport=49152 numberofports=16384 store=persistent",
		}, 0, 1},
		{"con --tcp, solo TCP", viejo, pedidoConfig{Dinamico: true, TCP: true}, []string{
			"netsh interface ipv4 set dynamicport protocol=tcp startport=49152 numberofports=16384 store=persistent",
			"netsh interface ipv6 set dynamicport protocol=tcp startport=49152 numberofports=16384 store=persistent",
		}, 0, 1},
		{"cada uno por lo suyo", mixto, pedidoConfig{Dinamico: true}, []string{
			"netsh interface ipv4 set dynamicport protocol=tcp startport=49152 numberofports=16384 store=persistent",
		}, 2, 2},
	}
	for _, c := range casos {
		pl := planificarConfig(estadoRed{Dyn: c.dyn, WinnatCorriendo: true}, c.pedido)
		if got := pasosTexto(pl); !reflect.DeepEqual(got, c.pasos) {
			t.Errorf("%s:\n  pasos %q\n  quería %q", c.nombre, got, c.pasos)
		}
		if len(pl.Hechos) != c.hechos || len(pl.Avisos) != c.avisos {
			t.Errorf("%s: hechos %q, avisos %q", c.nombre, pl.Hechos, pl.Avisos)
		}
		if pl.tocaWinnat() {
			t.Errorf("%s: cambiar el rango dinámico no reinicia WinNAT por su cuenta", c.nombre)
		}
	}
	// Se deshace con los valores que tenía.
	pl := planificarConfig(estadoRed{Dyn: viejo}, pedidoConfig{Dinamico: true, UDP: true})
	if want := "netsh interface ipv4 set dynamicport protocol=udp startport=1025 numberofports=3976 store=persistent"; len(pl.Deshacer) != 2 || pl.Deshacer[0] != want {
		t.Errorf("deshacer = %q", pl.Deshacer)
	}
}

func TestConfigPedidosInvalidos(t *testing.T) {
	casos := map[string]pedidoConfig{
		"nada":                   {},
		"reservar y quitar":      {Reservar: ports.Of(3000), Liberar: ports.Of(3001)},
		"demasiados puertos":     {Reservar: ports.NewSet(ports.R(1, 2000))},
		"demasiados para quitar": {Liberar: ports.NewSet(ports.R(1000, 9000))},
	}
	muchos := ports.Of()
	for i := 0; i < maxRangosConfig+1; i++ {
		muchos = muchos.Union(ports.Of(uint16(3000 + 2*i)))
	}
	casos["demasiados rangos"] = pedidoConfig{Reservar: muchos}
	for nombre, ped := range casos {
		pl := planificarConfig(estadoRed{WinnatCorriendo: true}, ped)
		if len(pl.Pasos) != 0 || len(pl.Impedimentos) != 1 || pl.ejecutable() {
			t.Errorf("%s: %+v", nombre, pl)
		}
	}
	// Justo en el tope, sí.
	justo := planificarConfig(estadoRed{}, pedidoConfig{Reservar: ports.NewSet(ports.R(3000, 3000+maxPuertosConfig-1))})
	if !justo.ejecutable() {
		t.Errorf("en el tope: %+v", justo)
	}
}

// --- el sistema simulado -----------------------------------------------------

// redFalsa es un sistema de mentira: tiene su estado y hace con él lo que
// harían netsh y WinNAT. Anota cada cosa que le piden.
type redFalsa struct {
	estado  estadoRed
	bloques []ports.ExcludedRange // los que WinNAT reserva cada vez que arranca
	pedidos []string
	falla   map[string]error // pedido → error (no cambia nada)
	cancela string           // al recibir este pedido, cancela
	cancel  context.CancelFunc
}

var formaNetsh = regexp.MustCompile(`^interface ipv[46] (add|delete) excludedportrange protocol=(tcp|udp) startport=[1-9][0-9]{0,4} numberofports=[1-9][0-9]{0,4} store=persistent$` +
	`|^interface ipv[46] set dynamicport protocol=(tcp|udp) startport=49152 numberofports=16384 store=persistent$`)

func (r *redFalsa) anotar(pedido string) error {
	r.pedidos = append(r.pedidos, pedido)
	if pedido == r.cancela && r.cancel != nil {
		r.cancel()
	}
	return r.falla[pedido]
}

func (r *redFalsa) detenerWinnat(_ context.Context, _ time.Time) error {
	if err := r.anotar("net stop winnat"); err != nil {
		return err
	}
	r.estado.WinnatCorriendo = false
	// Suelta SUS bloques; lo que apartan http.sys o un programa queda.
	r.estado.Excl = slices.DeleteFunc(slices.Clone(r.estado.Excl), func(x ports.ExcludedRange) bool { return x.Origin() == ports.OriginWinNAT })
	return nil
}

func (r *redFalsa) arrancarWinnat(_ context.Context, _ time.Time) error {
	if err := r.anotar("net start winnat"); err != nil {
		return err
	}
	if r.estado.WinnatCorriendo {
		return nil
	}
	r.estado.WinnatCorriendo = true
	// WinNAT vuelve a reservar sus bloques, salteando lo que está excluido:
	// es justo lo que hace que reservar sirva.
	for _, b := range r.bloques {
		c := combo{b.Proto, b.Family}
		if ports.NewSet(b.Range).Intersect(administradas(r.estado.Excl, c)).IsEmpty() {
			r.estado.Excl = append(r.estado.Excl, b)
		}
	}
	return nil
}

func (r *redFalsa) netsh(_ context.Context, args ...string) (string, error) {
	pedido := strings.Join(args, " ")
	if !formaNetsh.MatchString(pedido) {
		panic("netsh recibió un pedido con otra forma: " + pedido)
	}
	if err := r.anotar("netsh " + pedido); err != nil {
		return "", err
	}
	c := combo{ports.TCP, ports.IPv4}
	if args[1] == "ipv6" {
		c.fam = ports.IPv6
	}
	var inicio, cuantos int
	for _, a := range args {
		switch k, v, _ := strings.Cut(a, "="); k {
		case "protocol":
			if v == "udp" {
				c.proto = ports.UDP
			}
		case "startport":
			fmt.Sscan(v, &inicio)
		case "numberofports":
			fmt.Sscan(v, &cuantos)
		}
	}
	rango := ports.R(uint16(inicio), uint16(inicio+cuantos-1))
	switch args[2] + " " + args[3] {
	case "add excludedportrange":
		// Como el de verdad: se niega si el rango pisa algo excluido o en uso.
		for _, x := range r.estado.Excl {
			if x.Proto == c.proto && x.Family == c.fam && seCruzan(x.Range, rango) {
				return "", &errorNetsh{Codigo: 1, Dijo: "El proceso no tiene acceso al archivo porque está siendo utilizado por otro proceso."}
			}
		}
		if !ports.NewSet(rango).Intersect(r.estado.Ocupados[c]).IsEmpty() {
			return "", &errorNetsh{Codigo: 1, Dijo: "El proceso no tiene acceso al archivo porque está siendo utilizado por otro proceso."}
		}
		r.estado.Excl = append(slices.Clone(r.estado.Excl), ports.ExcludedRange{Proto: c.proto, Family: c.fam, Range: rango, Administered: true})
	case "delete excludedportrange":
		antes := len(r.estado.Excl)
		r.estado.Excl = slices.DeleteFunc(slices.Clone(r.estado.Excl), func(x ports.ExcludedRange) bool {
			return x.Administered && x.Proto == c.proto && x.Family == c.fam && x.Range == rango
		})
		if len(r.estado.Excl) == antes {
			return "", &errorNetsh{Codigo: 1, Dijo: "El sistema no puede encontrar el archivo especificado."}
		}
	case "set dynamicport":
		dyn := slices.Clone(r.estado.Dyn)
		for i := range dyn {
			if dyn[i].Proto == c.proto && dyn[i].Family == c.fam {
				dyn[i].Start, dyn[i].Count = uint16(inicio), cuantos
			}
		}
		r.estado.Dyn = dyn
	}
	return "Aceptar\n", nil
}

// relojFijo es un reloj que avanza un milisegundo por consulta.
func relojFijo() func() time.Time {
	t := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	return func() time.Time { t = t.Add(time.Millisecond); return t }
}

func fines(rs []resultadoPaso) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Paso.Clase.clave()+"="+r.Fin.clave())
	}
	return strings.Join(out, " ")
}

// De punta a punta contra el sistema simulado: el caso del issue de Docker con
// más reacciones. El 3000 cae en un bloque de WinNAT; después de reservarlo,
// WinNAT arranca, elige sus bloques de nuevo y ya no lo puede tomar.
func TestConfigReservarDePuntaAPunta(t *testing.T) {
	bloque := excl(tcp4, 2980, 3079, false)
	red := &redFalsa{bloques: []ports.ExcludedRange{bloque, excl(tcp4, 55150, 55249, false)}}
	red.estado = estadoRed{Excl: slices.Clone(red.bloques), Dyn: deFabrica(), WinnatExiste: true, WinnatCorriendo: true}
	pedido := pedidoConfig{Reservar: ports.Of(3000)}

	plan := planificarConfig(red.estado, pedido)
	var narrado []string
	res := ejecutarConfig(context.Background(), red, plan, relojFijo(), func(r resultadoPaso) {
		narrado = append(narrado, r.Paso.Clase.clave()+":"+map[finPaso]string{0: "empieza"}[r.Fin]+r.Fin.clave())
	})
	if got := fines(res); got != "detener-winnat=hecho reservar=hecho reservar=hecho arrancar-winnat=hecho" || !todoHecho(res) {
		t.Fatalf("resultados = %s", got)
	}
	if !reflect.DeepEqual(red.pedidos, pasosTexto(plan)) {
		t.Fatalf("se pidió %q y el plan decía %q", red.pedidos, pasosTexto(plan))
	}
	if len(narrado) != 8 || narrado[0] != "detener-winnat:empiezasalteado" && !strings.HasPrefix(narrado[0], "detener-winnat:empieza") {
		t.Fatalf("la narración = %q", narrado)
	}
	for _, r := range res {
		if r.Tardo <= 0 {
			t.Errorf("%s: sin duración", r.Paso.texto())
		}
	}
	if !red.estado.WinnatCorriendo {
		t.Fatal("WinNAT quedó detenido")
	}
	// El bloque que tapaba al 3000 ya no está (WinNAT no pudo volver a
	// tomarlo); el otro, sí.
	if _, tapado := red.estado.Excl.Find(ports.TCP, ports.IPv4, 3050); tapado {
		t.Errorf("WinNAT volvió a reservar el bloque del 3000: %v", red.estado.Excl)
	}
	if x, ok := red.estado.Excl.Find(ports.TCP, ports.IPv4, 3000); !ok || !x.Administered {
		t.Errorf("el 3000 no quedó como exclusión administrada: %v", red.estado.Excl)
	}
	if _, ok := red.estado.Excl.Find(ports.TCP, ports.IPv4, 55200); !ok {
		t.Errorf("WinNAT perdió un bloque que no tenía nada que ver: %v", red.estado.Excl)
	}
	for _, k := range verificarConfig(red.estado, plan, pedido) {
		if !k.Cumple {
			t.Errorf("no cumple: %s (%s)", k.Que, k.Porque)
		}
	}
	// Idempotente: pedirlo de nuevo no hace nada.
	if otra := planificarConfig(red.estado, pedido); len(otra.Pasos) != 0 || len(otra.Hechos) != 2 {
		t.Errorf("la segunda vez: %+v", otra)
	}
}

// WinNAT vuelve SIEMPRE: falle lo que falle después de intentar detenerlo, o
// aunque el usuario corte.
func TestConfigWinnatVuelve(t *testing.T) {
	const (
		stop  = "net stop winnat"
		add4  = "netsh interface ipv4 add excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent"
		add6  = "netsh interface ipv6 add excludedportrange protocol=tcp startport=3000 numberofports=1 store=persistent"
		start = "net start winnat"
	)
	boom := errors.New("se rompió")
	casos := []struct {
		nombre  string
		falla   map[string]error
		cancela string
		pedidos []string
		fines   string
		corre   bool // WinNAT queda corriendo
	}{
		{"falla la primera exclusión", map[string]error{add4: boom}, "", []string{stop, add4, start},
			"detener-winnat=hecho reservar=error reservar=salteado arrancar-winnat=hecho", true},
		{"falla la segunda", map[string]error{add6: boom}, "", []string{stop, add4, add6, start},
			"detener-winnat=hecho reservar=hecho reservar=error arrancar-winnat=hecho", true},
		{"no se puede detener WinNAT: no se reserva nada, y se lo arranca por las dudas", map[string]error{stop: boom}, "", []string{stop, start},
			"detener-winnat=error reservar=salteado reservar=salteado arrancar-winnat=hecho", true},
		{"el usuario corta después de detenerlo", nil, stop, []string{stop, start},
			"detener-winnat=hecho reservar=salteado reservar=salteado arrancar-winnat=hecho", true},
		{"el usuario corta a mitad de las exclusiones", nil, add4, []string{stop, add4, start},
			"detener-winnat=hecho reservar=hecho reservar=salteado arrancar-winnat=hecho", true},
		{"no arranca: se informa", map[string]error{start: boom}, "", []string{stop, add4, add6, start},
			"detener-winnat=hecho reservar=hecho reservar=hecho arrancar-winnat=error", false},
	}
	for _, c := range casos {
		ctx, cancel := context.WithCancel(context.Background())
		red := &redFalsa{falla: c.falla, cancela: c.cancela, cancel: cancel, bloques: []ports.ExcludedRange{excl(tcp4, 2980, 3079, false)}}
		red.estado = estadoRed{Excl: slices.Clone(red.bloques), WinnatExiste: true, WinnatCorriendo: true}
		plan := planificarConfig(red.estado, pedidoConfig{Reservar: ports.Of(3000)})
		res := ejecutarConfig(ctx, red, plan, relojFijo(), nil)
		cancel()
		if !reflect.DeepEqual(red.pedidos, c.pedidos) {
			t.Errorf("%s: se pidió %q, esperaba %q", c.nombre, red.pedidos, c.pedidos)
		}
		if got := fines(res); got != c.fines {
			t.Errorf("%s: resultados = %s, esperaba %s", c.nombre, got, c.fines)
		}
		// En el caso "no se puede detener" el doble no lo detuvo, así que sigue
		// corriendo; en todos los demás tiene que haber vuelto.
		if red.estado.WinnatCorriendo != c.corre {
			t.Errorf("%s: WinNAT corriendo = %v", c.nombre, red.estado.WinnatCorriendo)
		}
		if todoHecho(res) {
			t.Errorf("%s: no salió todo bien", c.nombre)
		}
	}
}

// Un plan con impedimentos, o vacío, no ejecuta nada.
func TestConfigNoEjecutaLoQueNoDebe(t *testing.T) {
	red := &redFalsa{}
	red.estado = estadoRed{Ocupados: map[combo]ports.Set{tcp4: ports.Of(3000)}, WinnatCorriendo: true}
	plan := planificarConfig(red.estado, pedidoConfig{Reservar: ports.Of(3000)})
	// Aunque alguien le meta pasos a mano: con un impedimento no corre.
	plan.Pasos = []paso{{Clase: pasoDetenerWinnat}, {Clase: pasoExcluir, Combo: tcp6, Rango: ports.R(3000, 3000)}}
	res := ejecutarConfig(context.Background(), red, plan, relojFijo(), nil)
	if len(red.pedidos) != 0 || fines(res) != "detener-winnat=salteado reservar=salteado" {
		t.Fatalf("pedidos %q, resultados %s", red.pedidos, fines(res))
	}
	if rs := ejecutarConfig(context.Background(), red, planConfig{}, relojFijo(), nil); len(rs) != 0 || len(red.pedidos) != 0 || todoHecho(rs) {
		t.Fatalf("un plan vacío: %v, %q", rs, red.pedidos)
	}
}

// estadoAlAzar arma un estado cualquiera: bloques de WinNAT (de 100),
// exclusiones de http.sys (de un puerto) y de programas (de cualquier otro
// tamaño), exclusiones administradas, puertos en uso y rangos dinámicos, todo
// cerca del 3000 para que las cosas se crucen. bloques son los de WinNAT: los
// que vuelve a reservar cuando arranca.
func estadoAlAzar(rng *rand.Rand) (estadoRed, []ports.ExcludedRange) {
	e := estadoRed{WinnatExiste: true, WinnatCorriendo: rng.IntN(5) != 0, Ocupados: map[combo]ports.Set{}}
	var bloques []ports.ExcludedRange
	for _, c := range combos {
		tomado := ports.Of()
		for n := rng.IntN(3); n > 0; n-- {
			lo := uint16(2900 + rng.IntN(200))
			x := ports.ExcludedRange{Proto: c.proto, Family: c.fam, Range: ports.R(lo, lo+uint16(rng.IntN(60))), Administered: rng.IntN(2) == 0}
			switch {
			case x.Administered:
			case rng.IntN(2) == 0:
				x.Range = ports.R(lo, lo+ports.WinNATBlock-1)
			case rng.IntN(2) == 0:
				x.Range = ports.R(lo, lo)
				x.HTTP = ports.HTTPHold{Held: true, URL: "http://*:" + strconv.Itoa(int(lo)) + "/"}
			}
			if !ports.NewSet(x.Range).Intersect(tomado).IsEmpty() {
				continue
			}
			tomado = tomado.Union(ports.NewSet(x.Range))
			e.Excl = append(e.Excl, x)
			if x.Origin() == ports.OriginWinNAT {
				bloques = append(bloques, x)
			}
		}
		var uso []uint16
		for n := rng.IntN(3); n > 0; n-- {
			if p := uint16(2900 + rng.IntN(250)); !tomado.Contains(p) { // lo excluido no se pudo atar
				uso = append(uso, p)
			}
		}
		e.Ocupados[c] = ports.Of(uso...)
		switch rng.IntN(4) {
		case 0:
			e.Dyn = append(e.Dyn, ports.DynamicRange{Proto: c.proto, Family: c.fam, Start: 1025, Count: 3976})
		case 1:
			e.Dyn = append(e.Dyn, ports.DynamicRange{Proto: c.proto, Family: c.fam, Start: 50000, Count: 2000})
		case 2:
			e.Dyn = append(e.Dyn, ports.DynamicRange{Proto: c.proto, Family: c.fam, Start: ports.DynamicDefaultStart, Count: ports.DynamicDefaultCount})
		}
	}
	return e, bloques
}

func pedidoAlAzar(rng *rand.Rand) pedidoConfig {
	lo := uint16(2950 + rng.IntN(150))
	set := ports.NewSet(ports.R(lo, lo+uint16(rng.IntN(12))))
	if rng.IntN(3) == 0 {
		set = set.Union(ports.Of(uint16(2950 + rng.IntN(150))))
	}
	ped := pedidoConfig{TCP: rng.IntN(3) == 0, UDP: rng.IntN(3) == 0, Dinamico: rng.IntN(3) == 0}
	switch rng.IntN(3) {
	case 0:
		ped.Reservar = set
	case 1:
		ped.Liberar = set
	default:
		ped.Dinamico = true
	}
	return ped
}

// Propiedades del planificador, sobre miles de estados y pedidos al azar.
func TestConfigPropiedades(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 4))
	ejecutados := 0
	for iter := 0; iter < 3000; iter++ {
		estado, bloques := estadoAlAzar(rng)
		pedido := pedidoAlAzar(rng)
		plan := planificarConfig(estado, pedido)
		ctxt := fmt.Sprintf("iter %d\n  pedido %+v\n  estado %+v\n  plan %q", iter, pedido, estado, pasosTexto(plan))

		// 1. Con un impedimento no hay pasos ni forma de deshacer nada.
		if len(plan.Impedimentos) > 0 && (len(plan.Pasos) > 0 || len(plan.Deshacer) > 0) {
			t.Fatalf("impedimentos y pasos a la vez\n%s", ctxt)
		}
		// 2. WinNAT: o no se lo toca, o se lo detiene PRIMERO y se lo arranca
		//    ÚLTIMO; y se lo toca solo si un bloque SUYO tapa algo que se
		//    reserva (lo que apartan http.sys o un programa no cuenta).
		stop, start, tapa := 0, 0, false
		for i, p := range plan.Pasos {
			switch p.Clase {
			case pasoDetenerWinnat:
				stop++
				if i != 0 {
					t.Fatalf("WinNAT no se detiene primero\n%s", ctxt)
				}
			case pasoArrancarWinnat:
				start++
				if i != len(plan.Pasos)-1 {
					t.Fatalf("WinNAT no se arranca último\n%s", ctxt)
				}
			case pasoExcluir:
				for _, b := range bloquesDe(estado.Excl, p.Combo) {
					tapa = tapa || (b.Origin() == ports.OriginWinNAT && seCruzan(b.Range, p.Rango))
				}
			}
		}
		if stop != start || stop > 1 || (stop == 1) != tapa {
			t.Fatalf("WinNAT: %d detenciones, %d arranques, tapado %v\n%s", stop, start, tapa, ctxt)
		}
		if stop == 1 && (!estado.WinnatCorriendo || !strings.Contains(strings.Join(plan.Avisos, " "), "WinNAT")) {
			t.Fatalf("detiene WinNAT sin que corra o sin avisar\n%s", ctxt)
		}
		//    Lo que tapa una exclusión que no es de WinNAT no se reserva:
		//    impedimento, que dice que no es de WinNAT.
		for _, c := range enCombos(pedido.protocolos(ports.TCP)) {
			faltan := pedido.Reservar.Minus(administradas(estado.Excl, c))
			for _, b := range bloquesDe(estado.Excl, c) {
				if b.Origin() != ports.OriginWinNAT && !ports.NewSet(b.Range).Intersect(faltan).IsEmpty() &&
					!strings.Contains(strings.Join(plan.Impedimentos, " "), "cae en la exclusión "+b.Range.String()) {
					t.Fatalf("reserva lo que tapa %v (%v) sin decir que no es de WinNAT\n%s", b.Range, b.Origin(), ctxt)
				}
			}
		}
		// 3. Cada paso toca SOLO lo que el usuario nombró, en los protocolos
		//    que pidió, y tiene la forma cerrada de siempre.
		for _, p := range plan.Pasos {
			if args := p.netsh(); args != nil && !formaNetsh.MatchString(strings.Join(args, " ")) {
				t.Fatalf("paso con otra forma: %q\n%s", args, ctxt)
			}
			enProto := func(porDefecto ...ports.Proto) bool {
				return slices.Contains(pedido.protocolos(porDefecto...), p.Combo.proto)
			}
			switch p.Clase {
			case pasoExcluir:
				r := ports.NewSet(p.Rango)
				if !r.Minus(pedido.Reservar).IsEmpty() || !enProto(ports.TCP) {
					t.Fatalf("reserva %v %v, que no se pidió\n%s", p.Rango, p.Combo, ctxt)
				}
				if !r.Intersect(administradas(estado.Excl, p.Combo)).IsEmpty() || !r.Intersect(estado.Ocupados[p.Combo]).IsEmpty() {
					t.Fatalf("reserva %v %v, que ya estaba reservado o está en uso\n%s", p.Rango, p.Combo, ctxt)
				}
			case pasoQuitar:
				if !ports.NewSet(p.Rango).Minus(pedido.Liberar).IsEmpty() || !enProto(ports.TCP) {
					t.Fatalf("quita %v %v, que no se nombró entero\n%s", p.Rango, p.Combo, ctxt)
				}
				if !slices.Contains(estado.Excl, ports.ExcludedRange{Proto: p.Combo.proto, Family: p.Combo.fam, Range: p.Rango, Administered: true}) {
					t.Fatalf("quita %v %v, que no es una exclusión administrada\n%s", p.Rango, p.Combo, ctxt)
				}
			case pasoDinamico:
				d, ok := estado.Dyn.Find(p.Combo.proto, p.Combo.fam)
				if !pedido.Dinamico || !enProto(ports.TCP, ports.UDP) || !ok || !d.Low() {
					t.Fatalf("cambia el rango dinámico de %v sin motivo\n%s", p.Combo, ctxt)
				}
			}
		}
		// 4. La huella identifica a los pasos.
		if otro := planificarConfig(estado, pedido); otro.huella() != plan.huella() || !reflect.DeepEqual(otro, plan) {
			t.Fatalf("planificar dos veces lo mismo dio planes distintos\n%s", ctxt)
		}
		if len(plan.Pasos) > 0 {
			corto := plan
			corto.Pasos = plan.Pasos[:len(plan.Pasos)-1]
			if corto.huella() == plan.huella() {
				t.Fatalf("la huella no cambia al sacar un paso\n%s", ctxt)
			}
		}
		if !plan.ejecutable() {
			continue
		}
		// 5. Ejecutado contra el sistema simulado (que se niega igual que el de
		//    verdad), sale todo bien, queda como se pidió, y pedirlo de nuevo
		//    no hace nada.
		ejecutados++
		red := &redFalsa{estado: estado, bloques: bloques}
		res := ejecutarConfig(context.Background(), red, plan, relojFijo(), nil)
		if !todoHecho(res) {
			t.Fatalf("la ejecución falló: %s (%v)\n%s", fines(res), res, ctxt)
		}
		for _, k := range verificarConfig(red.estado, plan, pedido) {
			if !k.Cumple {
				t.Fatalf("después de ejecutar no cumple: %s (%s)\n  quedó %+v\n%s", k.Que, k.Porque, red.estado, ctxt)
			}
		}
		if red.estado.WinnatCorriendo != estado.WinnatCorriendo {
			t.Fatalf("WinNAT no quedó como estaba\n%s", ctxt)
		}
		if otra := planificarConfig(red.estado, pedido); len(otra.Pasos) != 0 {
			t.Fatalf("no es idempotente: la segunda vez quiere %q\n%s", pasosTexto(otra), ctxt)
		}
		// 6. Lo que no se nombró quedó EXACTAMENTE como estaba: las exclusiones
		//    administradas de los otros puertos y protocolos, y los rangos
		//    dinámicos que no se pidió cambiar.
		tocado := pedido.Reservar.Union(pedido.Liberar)
		for _, x := range estado.Excl {
			if x.Administered && ports.NewSet(x.Range).Intersect(tocado).IsEmpty() && !slices.Contains(red.estado.Excl, x) {
				t.Fatalf("desapareció la exclusión %v, que nadie nombró\n  quedó %+v\n%s", x, red.estado.Excl, ctxt)
			}
		}
		for _, x := range red.estado.Excl {
			if x.Administered && !slices.Contains(estado.Excl, x) && !ports.NewSet(x.Range).Minus(pedido.Reservar).IsEmpty() {
				t.Fatalf("apareció la exclusión %v, que nadie pidió\n%s", x, ctxt)
			}
		}
		if !pedido.Dinamico && !reflect.DeepEqual(red.estado.Dyn, estado.Dyn) {
			t.Fatalf("cambió el rango dinámico sin que se pidiera\n%s", ctxt)
		}
	}
	if ejecutados < 300 {
		t.Fatalf("las propiedades de la ejecución casi no se ejercieron: %d planes", ejecutados)
	}
}

// Deshacer deja todo como estaba: reservar y después el comando que el plan
// dio para deshacerlo devuelve las exclusiones al punto de partida.
func TestConfigDeshacerEsExacto(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 5))
	probados := 0
	for iter := 0; iter < 2000; iter++ {
		estado, bloques := estadoAlAzar(rng)
		estado.WinnatCorriendo = true
		lo := uint16(2950 + rng.IntN(150))
		pedido := pedidoConfig{Reservar: ports.NewSet(ports.R(lo, lo+uint16(rng.IntN(12)))), UDP: rng.IntN(2) == 0}
		plan := planificarConfig(estado, pedido)
		if !plan.ejecutable() {
			continue
		}
		red := &redFalsa{estado: estado, bloques: bloques}
		if res := ejecutarConfig(context.Background(), red, plan, relojFijo(), nil); !todoHecho(res) {
			t.Fatalf("iter %d: %s", iter, fines(res))
		}
		for _, cmd := range plan.Deshacer {
			switch {
			case strings.HasPrefix(cmd, "killport "):
				vuelta := pedidoConfig{Liberar: pedido.Reservar, TCP: pedido.TCP, UDP: pedido.UDP}
				if want := "killport " + specDe(pedido.Reservar) + " --unreserve" + flagsProto(pedido); cmd != want {
					t.Fatalf("iter %d: deshacer = %q, esperaba %q", iter, cmd, want)
				}
				if res := ejecutarConfig(context.Background(), red, planificarConfig(red.estado, vuelta), relojFijo(), nil); !todoHecho(res) {
					t.Fatalf("iter %d: deshacer falló: %s", iter, fines(res))
				}
			case strings.HasPrefix(cmd, "netsh "):
				if _, err := red.netsh(context.Background(), strings.Fields(strings.TrimPrefix(cmd, "netsh "))...); err != nil {
					t.Fatalf("iter %d: %q: %v", iter, cmd, err)
				}
			default:
				t.Fatalf("iter %d: no sé deshacer con %q", iter, cmd)
			}
		}
		probados++
		admin := func(x ports.Exclusions) []ports.ExcludedRange {
			var out []ports.ExcludedRange
			for _, r := range x {
				if r.Administered {
					out = append(out, r)
				}
			}
			slices.SortFunc(out, func(a, b ports.ExcludedRange) int {
				return strings.Compare(fmt.Sprint(a.Proto, a.Family, a.Lo), fmt.Sprint(b.Proto, b.Family, b.Lo))
			})
			return out
		}
		if antes, despues := admin(estado.Excl), admin(red.estado.Excl); !reflect.DeepEqual(antes, despues) {
			t.Fatalf("iter %d: después de deshacer %+v\n  antes   %v\n  después %v\n  deshacer %q", iter, pedido, antes, despues, plan.Deshacer)
		}
	}
	if probados < 200 {
		t.Fatalf("casi no se ejerció: %d", probados)
	}
}

func TestConfigOcupados(t *testing.T) {
	socks := []ports.Socket{
		sockDe(ports.TCP, "0.0.0.0:3000", ports.Listen, 100),
		sockDe(ports.TCP, "[::1]:3000", ports.Listen, 100),
		sockDe(ports.TCP, "10.0.0.2:51234", ports.Established, 200), // una conexión saliente tiene tomado su puerto
		sockDe(ports.TCP, "10.0.0.2:51300", ports.TimeWait, 0),      // un resto sin dueño, no
		sockDe(ports.UDP, "0.0.0.0:5353", ports.StateNone, 300),
	}
	got := ocupadosDe(socks)
	want := map[combo]ports.Set{tcp4: ports.Of(3000, 51234), tcp6: ports.Of(3000), udp4: ports.Of(5353)}
	if len(got) != len(want) {
		t.Fatalf("ocupados = %v", got)
	}
	for c, s := range want {
		if !got[c].Equal(s) {
			t.Errorf("%v: %v, esperaba %v", c, got[c], s)
		}
	}
	if !got[udp6].IsEmpty() {
		t.Errorf("UDP IPv6 = %v", got[udp6])
	}
}

// sockDe arma un socket a partir de "dirección:puerto".
func sockDe(proto ports.Proto, local string, state ports.State, pid uint32) ports.Socket {
	return ports.Socket{Proto: proto, Local: netip.MustParseAddrPort(local), State: state, PID: pid}
}

// La huella es lo que viaja al proceso elevado en lugar de los pasos: dos
// planes tienen la misma si y solo si harían exactamente lo mismo.
func TestConfigHuella(t *testing.T) {
	base := estadoRed{Excl: ports.Exclusions{excl(tcp4, 2980, 3079, false), excl(tcp6, 2980, 3079, false)},
		Dyn: dinamicos(1025, 3976), WinnatExiste: true, WinnatCorriendo: true}
	huella := func(e estadoRed, ped pedidoConfig) string { return planificarConfig(e, ped).huella() }
	ref := huella(base, pedidoConfig{Reservar: ports.Of(3000)})
	if len(ref) != 32 || strings.Trim(ref, "0123456789abcdef") != "" {
		t.Fatalf("la huella %q no tiene la forma que exige el trabajo", ref)
	}
	if otra := huella(base, pedidoConfig{Reservar: ports.Of(3000)}); otra != ref {
		t.Errorf("el mismo plan dio dos huellas: %s y %s", ref, otra)
	}
	sinBloque := base
	sinBloque.Excl = nil
	ocupado := base
	ocupado.Ocupados = map[combo]ports.Set{tcp4: ports.Of(8080)}
	distintos := map[string]string{
		"otro puerto":                huella(base, pedidoConfig{Reservar: ports.Of(3001)}),
		"un puerto más":              huella(base, pedidoConfig{Reservar: ports.Of(3000, 3001)}),
		"un rango que lo contiene":   huella(base, pedidoConfig{Reservar: ports.NewSet(ports.R(3000, 3002))}),
		"otro protocolo":             huella(base, pedidoConfig{Reservar: ports.Of(3000), UDP: true}),
		"los dos protocolos":         huella(base, pedidoConfig{Reservar: ports.Of(3000), TCP: true, UDP: true}),
		"además, el rango dinámico":  huella(base, pedidoConfig{Reservar: ports.Of(3000), Dinamico: true}),
		"solo el rango dinámico":     huella(base, pedidoConfig{Dinamico: true}),
		"sin tener que tocar WinNAT": huella(sinBloque, pedidoConfig{Reservar: ports.Of(3000)}),
		"un plan sin pasos":          huella(ocupado, pedidoConfig{Reservar: ports.Of(8080)}),
	}
	vistas := map[string]string{ref: "el de referencia"}
	for nombre, h := range distintos {
		if otro, repetida := vistas[h]; repetida {
			t.Errorf("%q tiene la misma huella que %q (%s): harían cosas distintas", nombre, otro, h)
		}
		vistas[h] = nombre
	}
	// Quitar lo que se reservó no es lo mismo que reservarlo.
	reservado := estadoRed{Excl: ports.Exclusions{excl(tcp4, 3000, 3000, true), excl(tcp6, 3000, 3000, true)}, WinnatCorriendo: true}
	if huella(reservado, pedidoConfig{Liberar: ports.Of(3000)}) == huella(sinBloque, pedidoConfig{Reservar: ports.Of(3000)}) {
		t.Error("reservar y quitar la reserva del mismo puerto tienen la misma huella")
	}
}
