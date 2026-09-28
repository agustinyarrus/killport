package killport

// config.go: el PLANIFICADOR de las acciones de configuración de red, puro.
//
//	killport 3000 --reserve      que WinNAT no vuelva a tomar ese puerto
//	killport 3000 --unreserve    deshacerlo
//	killport --fix-dynamic       devolver el rango dinámico a 49152–65535
//
// Las tres cambian la configuración de TODO el sistema, y por eso están
// partidas en dos mitades que no se tocan:
//
//   - esta, que a partir de lo que se LEYÓ del sistema (estadoRed) y de lo que
//     pidió el usuario (pedidoConfig) arma la lista de pasos. No ejecuta nada,
//     no corre nada, no sabe si es administrador. Se prueba con tablas.
//   - el ejecutor (config_windows.go), que corre los pasos de un plan, uno por
//     uno, contra una interfaz.
//
// El plan es MÍNIMO y depende del estado: no se reserva lo que ya está
// reservado, no se toca un rango dinámico que ya es el de fábrica, y WinNAT
// solo se detiene si un bloque suyo tapa de verdad lo que se pide (detenerlo
// corta por un instante la red de WSL, Docker y Hyper-V: no es gratis).
//
// Esa misma pureza es la que hace seguro al trabajo elevado. Por los
// argumentos del proceso elevado no viaja una lista de comandos: viaja el
// PEDIDO y la huella del plan que el usuario vio y confirmó. El proceso
// elevado vuelve a leer el sistema, vuelve a planificar con ESTA función y
// solo ejecuta si el plan que le sale es el mismo. Nada que venga por
// argumentos puede convertirse en un paso que el planificador no habría
// puesto.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/agustinyarrus/killport/internal/ports"
)

// combo es un protocolo en una familia: la unidad de las tablas de netsh.
type combo struct {
	proto ports.Proto
	fam   ports.Family
}

// combos son los cuatro, en el orden fijo en que se arma todo resultado.
var combos = [...]combo{
	{ports.TCP, ports.IPv4}, {ports.TCP, ports.IPv6},
	{ports.UDP, ports.IPv4}, {ports.UDP, ports.IPv6},
}

// ipv y prot son los nombres que netsh les da a la familia y al protocolo.
func (c combo) ipv() string {
	if c.fam == ports.IPv6 {
		return "ipv6"
	}
	return "ipv4"
}

func (c combo) prot() string {
	if c.proto == ports.UDP {
		return "udp"
	}
	return "tcp"
}

// String lo nombra para el usuario: "TCP IPv4".
func (c combo) String() string { return c.proto.String() + " " + c.fam.String() }

// controladorWinnat es el controlador de WinNAT en el administrador de
// servicios. Es el ÚNICO servicio que las acciones de configuración detienen
// o arrancan, y su nombre no sale de ningún argumento.
const controladorWinnat = "winnat"

// Topes de lo que se puede pedir de una vez. Reservar es permanente y afecta a
// toda la máquina: un pedido enorme es casi seguro un error de tipeo.
const (
	maxPuertosConfig = 1024
	maxRangosConfig  = 16
)

// pedidoConfig es lo que pidió el usuario.
type pedidoConfig struct {
	Reservar ports.Set // --reserve
	Liberar  ports.Set // --unreserve
	Dinamico bool      // --fix-dynamic
	// TCP y UDP son los flags de protocolo. Sin ninguno, reservar y liberar
	// van por TCP —lo que usa un servidor de desarrollo; cada exclusión es un
	// cambio permanente y se hace el mínimo— y el rango dinámico, por los dos.
	TCP, UDP bool
}

// vacio dice si no se pidió nada.
func (p pedidoConfig) vacio() bool {
	return p.Reservar.IsEmpty() && p.Liberar.IsEmpty() && !p.Dinamico
}

// validar dice por qué el pedido no se puede atender, o nil.
func (p pedidoConfig) validar() error {
	switch {
	case p.vacio():
		return fmt.Errorf("no hay nada que configurar")
	case !p.Reservar.IsEmpty() && !p.Liberar.IsEmpty():
		return fmt.Errorf("--reserve y --unreserve no van juntos: elegí uno")
	}
	for _, s := range []ports.Set{p.Reservar, p.Liberar} {
		if n := s.Len(); n > maxPuertosConfig {
			return fmt.Errorf("son %d puertos y el tope es %d por vez: una reserva es permanente y vale para toda la máquina", n, maxPuertosConfig)
		}
		if n := len(s.Ranges()); n > maxRangosConfig {
			return fmt.Errorf("son %d rangos y el tope es %d por vez", n, maxRangosConfig)
		}
	}
	return nil
}

// protocolos son los que alcanza el pedido: los de los flags, o el que
// corresponde por defecto.
func (p pedidoConfig) protocolos(porDefecto ...ports.Proto) []ports.Proto {
	return protosPedidos(p.TCP, p.UDP, porDefecto...)
}

// protosPedidos es la regla de --tcp/--udp para lo que busca o cambia un
// puerto (--free, las alternativas, --config): los flags que se dieron, o, sin
// ninguno, los de por defecto de cada modo. Los filtros de cerrar y de --wait
// son otra cosa (opciones.filtro y filtroEspera): ahí sin flags se miran los
// dos protocolos.
func protosPedidos(tcp, udp bool, porDefecto ...ports.Proto) []ports.Proto {
	switch {
	case tcp && udp:
		return []ports.Proto{ports.TCP, ports.UDP}
	case tcp:
		return []ports.Proto{ports.TCP}
	case udp:
		return []ports.Proto{ports.UDP}
	}
	return porDefecto
}

// enCombos son los combos de esos protocolos, en el orden fijo de siempre
// (las dos familias: un servidor que escucha en localhost ata las dos).
func enCombos(protos []ports.Proto) []combo {
	var out []combo
	for _, c := range combos {
		for _, p := range protos {
			if c.proto == p {
				out = append(out, c)
			}
		}
	}
	return out
}

// estadoRed es lo que se leyó del sistema.
type estadoRed struct {
	Excl ports.Exclusions
	Dyn  ports.DynamicRanges
	// Ocupados son los puertos locales que algún socket tiene tomados ahora,
	// por protocolo y familia: Windows no deja excluir un puerto en uso.
	Ocupados map[combo]ports.Set
	// WinNAT: si el controlador existe en esta máquina y si está corriendo.
	WinnatExiste    bool
	WinnatCorriendo bool
}

// ocupadosDe arma Ocupados a partir de las tablas de sockets. Cuenta todo
// socket con dueño, en cualquier estado: una conexión saliente también tiene
// tomado su puerto local. Los restos sin dueño (TIME_WAIT, PID 0) no.
func ocupadosDe(socks []ports.Socket) map[combo]ports.Set {
	juntos := map[combo][]uint16{}
	for _, s := range socks {
		if s.PID == 0 || s.Port() == 0 {
			continue
		}
		c := combo{s.Proto, s.Family()}
		juntos[c] = append(juntos[c], s.Port())
	}
	out := make(map[combo]ports.Set, len(juntos))
	for c, ps := range juntos {
		out[c] = ports.Of(ps...)
	}
	return out
}

// administradas son las exclusiones administradas de ese combo, como conjunto.
func administradas(x ports.Exclusions, c combo) ports.Set {
	var rs []ports.Range
	for _, r := range x {
		if r.Proto == c.proto && r.Family == c.fam && r.Administered {
			rs = append(rs, r.Range)
		}
	}
	return ports.NewSet(rs...)
}

// bloquesDe son las filas NO administradas de ese combo: las que Windows
// aparta por su cuenta (WinNAT, http.sys, la reserva de un programa; de quién
// es cada una lo dice Origin).
func bloquesDe(x ports.Exclusions, c combo) []ports.ExcludedRange {
	var rs []ports.ExcludedRange
	for _, r := range x {
		if r.Proto == c.proto && r.Family == c.fam && !r.Administered {
			rs = append(rs, r)
		}
	}
	return rs
}

func seCruzan(a, b ports.Range) bool { return a.Lo <= b.Hi && b.Lo <= a.Hi }

// clasePaso es qué hace un paso del plan. El vocabulario es CERRADO: estos
// cinco, y ninguno más, son lo que el proceso elevado sabe hacer con la red.
type clasePaso uint8

const (
	pasoDetenerWinnat  clasePaso = iota + 1 // detener el controlador de WinNAT
	pasoExcluir                             // agregar una exclusión administrada persistente
	pasoQuitar                              // quitar una exclusión administrada
	pasoDinamico                            // devolver un rango dinámico al de fábrica
	pasoArrancarWinnat                      // volver a arrancar WinNAT
)

func (c clasePaso) clave() string {
	switch c {
	case pasoDetenerWinnat:
		return "detener-winnat"
	case pasoExcluir:
		return "reservar"
	case pasoQuitar:
		return "quitar-reserva"
	case pasoDinamico:
		return "rango-dinamico"
	case pasoArrancarWinnat:
		return "arrancar-winnat"
	}
	return "desconocido"
}

// paso es un paso del plan.
type paso struct {
	Clase clasePaso
	Combo combo       // de qué protocolo y familia (los pasos de WinNAT no tienen)
	Rango ports.Range // qué puertos (los pasos de WinNAT y del rango dinámico no tienen)
}

// netsh son los argumentos de netsh del paso, o nil si el paso no es de netsh
// (detener y arrancar WinNAT van por el administrador de servicios). Todos los
// valores salen de tipos —un protocolo, una familia, dos números—: no hay un
// solo texto libre.
func (p paso) netsh() []string {
	rango := []string{"protocol=" + p.Combo.prot(), "startport=" + strconv.Itoa(int(p.Rango.Lo)),
		"numberofports=" + strconv.Itoa(p.Rango.Len()), "store=persistent"}
	switch p.Clase {
	case pasoExcluir:
		return append([]string{"interface", p.Combo.ipv(), "add", "excludedportrange"}, rango...)
	case pasoQuitar:
		return append([]string{"interface", p.Combo.ipv(), "delete", "excludedportrange"}, rango...)
	case pasoDinamico:
		return []string{"interface", p.Combo.ipv(), "set", "dynamicport", "protocol=" + p.Combo.prot(),
			"startport=" + strconv.Itoa(ports.DynamicDefaultStart), "numberofports=" + strconv.Itoa(ports.DynamicDefaultCount), "store=persistent"}
	}
	return nil
}

// comando es el paso como lo escribiría una persona en una consola de
// administrador: lo que se muestra antes de preguntar.
func (p paso) comando() string {
	switch p.Clase {
	case pasoDetenerWinnat:
		return "net stop " + controladorWinnat
	case pasoArrancarWinnat:
		return "net start " + controladorWinnat
	}
	return "netsh " + strings.Join(p.netsh(), " ")
}

// texto dice qué hace el paso.
func (p paso) texto() string {
	switch p.Clase {
	case pasoDetenerWinnat:
		return "detener WinNAT"
	case pasoArrancarWinnat:
		return "volver a arrancar WinNAT"
	case pasoExcluir:
		return "reservar " + p.Rango.String() + " " + p.Combo.String()
	case pasoQuitar:
		return "quitar la reserva de " + p.Rango.String() + " " + p.Combo.String()
	case pasoDinamico:
		return "rango dinámico de " + p.Combo.String() + " a 49152–65535"
	}
	return "paso desconocido"
}

// planConfig es lo que hay que hacer, y lo que hay que saber antes.
type planConfig struct {
	Pasos []paso
	// Hechos: lo que ya está como se pidió (no hay nada que hacer ahí).
	Hechos []string
	// Avisos: lo que hay que saber antes de confirmar.
	Avisos []string
	// Impedimentos: por qué no se puede. Con uno solo, el plan no se ejecuta:
	// no se hace la mitad de un pedido.
	Impedimentos []string
	// Deshacer: los comandos que dejan todo como estaba.
	Deshacer []string
}

// ejecutable dice si el plan tiene algo que hacer y nada que lo impida.
func (p planConfig) ejecutable() bool { return len(p.Pasos) > 0 && len(p.Impedimentos) == 0 }

// tocaWinnat dice si el plan detiene WinNAT.
func (p planConfig) tocaWinnat() bool {
	for _, x := range p.Pasos {
		if x.Clase == pasoDetenerWinnat {
			return true
		}
	}
	return false
}

// huella identifica a los PASOS del plan, en orden: dos planes tienen la misma
// huella si y solo si harían exactamente lo mismo. Es lo que viaja al proceso
// elevado en lugar de los pasos.
func (p planConfig) huella() string {
	h := sha256.New()
	for _, x := range p.Pasos {
		fmt.Fprintf(h, "%s|%d|%d|%d|%d\n", x.Clase.clave(), x.Combo.proto, x.Combo.fam, x.Rango.Lo, x.Rango.Hi)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// planificarConfig arma el plan. Pura y total: para cualquier estado y
// cualquier pedido devuelve un plan (quizás sin pasos, quizás con
// impedimentos); nunca falla.
func planificarConfig(e estadoRed, ped pedidoConfig) planConfig {
	var pl planConfig
	if err := ped.validar(); err != nil {
		pl.Impedimentos = append(pl.Impedimentos, err.Error())
		return pl
	}
	var red []paso // los pasos de netsh, en orden
	tapado := false

	// --- reservar ---
	// entero: en todos los combos hay que reservar exactamente lo pedido (no
	// había nada reservado de antes). Decide cómo se deshace.
	entero := true
	for _, c := range enCombos(ped.protocolos(ports.TCP)) {
		if ped.Reservar.IsEmpty() {
			break
		}
		faltan := ped.Reservar.Minus(administradas(e.Excl, c))
		entero = entero && faltan.Equal(ped.Reservar)
		if faltan.IsEmpty() {
			pl.Hechos = append(pl.Hechos, ped.Reservar.String()+" ya está reservado en "+c.String())
			continue
		}
		if ya := ped.Reservar.Intersect(administradas(e.Excl, c)); !ya.IsEmpty() {
			pl.Hechos = append(pl.Hechos, ya.String()+" ya está reservado en "+c.String())
		}
		if enUso := faltan.Intersect(e.Ocupados[c]); !enUso.IsEmpty() {
			pl.Impedimentos = append(pl.Impedimentos, enUso.String()+" "+c.String()+" está en uso y Windows no deja reservar un puerto tomado: `killport "+
				specDe(enUso)+"` muestra quién lo tiene y lo libera")
			continue
		}
		for _, r := range faltan.Ranges() {
			for _, b := range bloquesDe(e.Excl, c) {
				switch {
				case !seCruzan(r, b.Range):
				case b.Origin() != ports.OriginWinNAT:
					// Lo que aparta http.sys o un programa no se va deteniendo
					// WinNAT: ni se ofrece cortarles la red a WSL y Docker.
					pl.Impedimentos = append(pl.Impedimentos, porQueNoReservo(r, c, b))
				default:
					tapado = true
					if !e.WinnatCorriendo {
						winnat := "que está detenido"
						if !e.WinnatExiste {
							winnat = "esta máquina no lo tiene"
						}
						pl.Impedimentos = append(pl.Impedimentos, r.String()+" "+c.String()+" cae en el rango excluido "+b.Range.String()+
							", y no es de WinNAT ("+winnat+"): no sé quién lo reservó, así que no lo toco")
					}
				}
			}
			red = append(red, paso{Clase: pasoExcluir, Combo: c, Rango: r})
		}
	}
	// Deshacer una reserva es quitar EXACTAMENTE lo que se agregó. Si se
	// agregó lo pedido entero, alcanza con el --unreserve de lo mismo; si parte
	// ya estaba reservada —por otro programa, o por el usuario otro día—, ese
	// comando se la llevaría puesta: ahí se da el comando de cada exclusión.
	switch {
	case ped.Reservar.IsEmpty():
	case entero:
		pl.Deshacer = append(pl.Deshacer, "killport "+specDe(ped.Reservar)+" --unreserve"+flagsProto(ped))
	default:
		for _, x := range red {
			pl.Deshacer = append(pl.Deshacer, (paso{Clase: pasoQuitar, Combo: x.Combo, Rango: x.Rango}).comando())
		}
	}

	// --- quitar la reserva ---
	for _, c := range enCombos(ped.protocolos(ports.TCP)) {
		if ped.Liberar.IsEmpty() {
			break
		}
		toco := false
		for _, x := range e.Excl {
			if x.Proto != c.proto || x.Family != c.fam || !x.Administered {
				continue
			}
			dentro := ports.NewSet(x.Range).Minus(ped.Liberar).IsEmpty()
			switch {
			case dentro:
				toco = true
				red = append(red, paso{Clase: pasoQuitar, Combo: c, Rango: x.Range})
				pl.Deshacer = append(pl.Deshacer, (paso{Clase: pasoExcluir, Combo: c, Rango: x.Range}).comando())
			case !ports.NewSet(x.Range).Intersect(ped.Liberar).IsEmpty():
				toco = true
				// Una exclusión se quita entera o no se quita: killport no borra
				// un rango que el usuario no nombró.
				pl.Avisos = append(pl.Avisos, "en "+c.String()+", "+ports.NewSet(x.Range).Intersect(ped.Liberar).String()+" es parte de la reserva "+x.Range.String()+
					", que no se parte: para quitarla entera, `killport "+specDe(ports.NewSet(x.Range))+" --unreserve"+flagsProto(ped)+"`")
			}
		}
		if !toco {
			pl.Hechos = append(pl.Hechos, ped.Liberar.String()+" no tiene ninguna reserva en "+c.String())
		}
	}

	// --- rango dinámico ---
	if ped.Dinamico {
		for _, c := range enCombos(ped.protocolos(ports.TCP, ports.UDP)) {
			d, ok := e.Dyn.Find(c.proto, c.fam)
			switch {
			case !ok:
				pl.Avisos = append(pl.Avisos, "no pude leer el rango dinámico de "+c.String()+": no lo toco")
			case d.Default():
				pl.Hechos = append(pl.Hechos, "el rango dinámico de "+c.String()+" ya es el de fábrica (49152–65535)")
			case !d.Low():
				pl.Hechos = append(pl.Hechos, "el rango dinámico de "+c.String()+" es "+d.Range().String()+
					": no es el de fábrica, pero no pisa los puertos de desarrollo; no lo toco")
			default:
				red = append(red, paso{Clase: pasoDinamico, Combo: c})
				pl.Deshacer = append(pl.Deshacer, "netsh interface "+c.ipv()+" set dynamicport protocol="+c.prot()+
					" startport="+strconv.Itoa(int(d.Start))+" numberofports="+strconv.Itoa(d.Count)+" store=persistent")
			}
		}
	}

	if len(pl.Impedimentos) > 0 {
		// No se hace la mitad de un pedido: con un impedimento, ningún paso.
		pl.Deshacer = nil
		return pl
	}
	if len(red) == 0 {
		pl.Deshacer = nil
		return pl
	}
	if tapado {
		pl.Pasos = append(pl.Pasos, paso{Clase: pasoDetenerWinnat})
		pl.Pasos = append(pl.Pasos, red...)
		pl.Pasos = append(pl.Pasos, paso{Clase: pasoArrancarWinnat})
		pl.Avisos = append(pl.Avisos, "detener WinNAT corta por un instante la red de WSL, Docker y Hyper-V: las conexiones abiertas con contenedores y distros se caen")
	} else {
		pl.Pasos = red
	}
	for _, x := range red {
		if x.Clase == pasoDinamico {
			pl.Avisos = append(pl.Avisos, "el rango dinámico es de toda la máquina; los bloques que WinNAT ya reservó siguen donde están hasta que se reinicie WinNAT (`net stop winnat` y `net start winnat`) o Windows")
			break
		}
	}
	for _, x := range red {
		if x.Clase == pasoQuitar {
			// Windows no guarda quién puso una exclusión: killport no puede
			// saber si es la que dejó un --reserve o la de otro programa.
			pl.Avisos = append(pl.Avisos, "Windows no dice quién puso una reserva: si no la pusiste vos, puede ser de otro programa que cuenta con ella")
			break
		}
	}
	return pl
}

// specDe escribe un conjunto como lo aceptaría la línea de comandos:
// "3000,5000-5003".
func specDe(s ports.Set) string {
	rs := s.Ranges()
	parts := make([]string, len(rs))
	for i, r := range rs {
		if r.Lo == r.Hi {
			parts[i] = strconv.Itoa(int(r.Lo))
		} else {
			parts[i] = strconv.Itoa(int(r.Lo)) + "-" + strconv.Itoa(int(r.Hi))
		}
	}
	return strings.Join(parts, ",")
}

// flagsProto son los flags de protocolo del pedido, para repetirlos en un
// comando que se le muestra al usuario.
func flagsProto(p pedidoConfig) string {
	s := ""
	if p.TCP {
		s += " --tcp"
	}
	if p.UDP {
		s += " --udp"
	}
	return s
}

// --- después de ejecutar: ¿quedó como se pidió? -----------------------------

// comprobacion es el resultado de verificar UNA cosa del pedido contra el
// estado que se volvió a leer.
type comprobacion struct {
	Que    string // "3000 TCP IPv4 reservado"
	Cumple bool
	Porque string // si no cumple
	// Salud: no es lo pedido sino cómo quedó la máquina (WinNAT volvió a
	// arrancar). Si falla, el código no es 0; si se cumple, no cuenta como
	// "algo de lo pedido salió" (no convierte una falla total en parcial).
	Salud bool
}

// deshacerHechos son los comandos que deshacen los pasos que SÍ se hicieron,
// para cuando el plan no terminó entero: una reserva que quedó a medias es
// persistente, y quien la pidió tiene que saber cómo sacarla. Uno por paso,
// exacto (el --unreserve de todo el pedido podría llevarse una reserva que ya
// estaba). Detener y arrancar WinNAT no se deshacen: WinNAT se verifica
// aparte. Puro.
func deshacerHechos(plan planConfig, res []resultadoPaso) []string {
	var out []string
	for _, r := range res {
		if r.Fin != pasoHecho {
			continue
		}
		switch x := r.Paso; x.Clase {
		case pasoExcluir:
			out = append(out, paso{Clase: pasoQuitar, Combo: x.Combo, Rango: x.Rango}.comando())
		case pasoQuitar:
			out = append(out, paso{Clase: pasoExcluir, Combo: x.Combo, Rango: x.Rango}.comando())
		case pasoDinamico:
			// El rango de antes quedó en el Deshacer del plan.
			pref := "netsh interface " + x.Combo.ipv() + " set dynamicport protocol=" + x.Combo.prot() + " "
			for _, d := range plan.Deshacer {
				if strings.HasPrefix(d, pref) {
					out = append(out, d)
				}
			}
		}
	}
	return out
}

// verificarConfig compara el pedido con el estado DE DESPUÉS. No mira qué
// dijo el que ejecutó: mira cómo quedó el sistema.
func verificarConfig(despues estadoRed, antes planConfig, ped pedidoConfig) []comprobacion {
	var out []comprobacion
	hizo := map[clasePaso]map[combo]bool{}
	for _, p := range antes.Pasos {
		if hizo[p.Clase] == nil {
			hizo[p.Clase] = map[combo]bool{}
		}
		hizo[p.Clase][p.Combo] = true
	}
	for _, c := range enCombos(ped.protocolos(ports.TCP)) {
		if !ped.Reservar.IsEmpty() {
			falta := ped.Reservar.Minus(administradas(despues.Excl, c))
			k := comprobacion{Que: ped.Reservar.String() + " " + c.String() + " reservado", Cumple: falta.IsEmpty()}
			if !k.Cumple {
				k.Porque = falta.String() + " no figura como exclusión administrada"
			}
			out = append(out, k)
		}
		if !ped.Liberar.IsEmpty() && hizo[pasoQuitar][c] {
			queda := ports.Of()
			for _, p := range antes.Pasos {
				if p.Clase == pasoQuitar && p.Combo == c {
					queda = queda.Union(ports.NewSet(p.Rango).Intersect(administradas(despues.Excl, c)))
				}
			}
			k := comprobacion{Que: "reserva de " + ped.Liberar.String() + " " + c.String() + " quitada", Cumple: queda.IsEmpty()}
			if !k.Cumple {
				k.Porque = queda.String() + " sigue figurando como exclusión administrada"
			}
			out = append(out, k)
		}
	}
	if antes.tocaWinnat() && despues.WinnatExiste {
		// Detenerlo era un paso intermedio: si no volvió, la red de WSL,
		// Docker y Hyper-V quedó sin servicio, y eso no es "verificado".
		k := comprobacion{Que: "WinNAT corriendo", Cumple: despues.WinnatCorriendo, Salud: true}
		if !k.Cumple {
			k.Porque = "quedó detenido y sin él no anda la red de WSL, Docker ni Hyper-V: arrancalo con `net start winnat` como administrador"
		}
		out = append(out, k)
	}
	if ped.Dinamico {
		for _, c := range enCombos(ped.protocolos(ports.TCP, ports.UDP)) {
			if !hizo[pasoDinamico][c] {
				continue
			}
			d, ok := despues.Dyn.Find(c.proto, c.fam)
			k := comprobacion{Que: "rango dinámico de " + c.String() + " en 49152–65535", Cumple: ok && d.Default()}
			switch {
			case !ok:
				k.Porque = "no pude volver a leerlo"
			case !k.Cumple:
				k.Porque = "quedó en " + d.Range().String()
			}
			out = append(out, k)
		}
	}
	return out
}
