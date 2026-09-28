//go:build windows

package killport

// listado.go: el listado general (sin puertos). Una fila por (proceso,
// protocolo, puerto), fusionando las direcciones IPv4 e IPv6 del mismo servidor
// —un dev server en 0.0.0.0 y [::] son la misma cosa para el usuario—,
// ocultando lo de Windows salvo --system, y en consola interactiva numerando
// las filas y ofreciendo liberar una.
//
// Las columnas se reparten el ancho de la consola (tui.Table): en 100
// columnas se lee PUERTO, PROTO, ALCANCE, PID, PROCESO, PROYECTO y COMANDO;
// con más lugar aparecen DESDE y la DIRECCIÓN exacta. Un puerto que escuchan
// dos procesos distintos va marcado con "!", y debajo de la tabla se dice a
// cuál llega cada destino (alcance.go).

import (
	"net/netip"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

// fila es una línea del listado, ya fusionada.
type fila struct {
	Port     uint16
	Proto    ports.Proto
	Familias familias // qué familias (v4/v6) aportaron
	PID      uint32
	Owner    reaper.ID
	// Name es la imagen del proceso de Windows que sostiene el socket (el
	// dato: lo que va al JSON). Lo que se MUESTRA en PROCESO es Titulo, si
	// está: lo que hay detrás de un intermediario ("http.sys · pwsh.exe", el
	// contenedor, el proceso Linux).
	Name   string
	Titulo string
	Since  time.Time
	// Heredado: el dueño que figura en la tabla (este PID) murió y el socket
	// lo sostiene este proceso, que lo heredó; 0 si la fila es de su dueño.
	Heredado  uint32
	Servicio  string // nombre del servicio dueño (svchost) si se supo
	EsWindows bool
	efimero   bool   // solo clientes UDP efímeros: ruido en el listado
	Comando   string // lo que se muestra en COMANDO (legible, recortable); se completa después
	// Linea y Ruta son la línea de comandos y la ruta del ejecutable DE VERDAD
	// (el JSON): Comando es para leer, no para ejecutar ni comparar.
	Linea string
	Ruta  string

	// Intermediarios: la fila de un relay dice qué hay detrás.
	Relay      reaper.Relay     // el dueño es un intermediario (contenedores, WSL, VS Code, http.sys, portproxy)
	Via        string           // imagen del relay de Windows (cuando Name pasa a ser lo de atrás)
	Contenedor *contenedor      // el contenedor que publica este puerto
	Linux      *procesoLinux    // el proceso Linux que escucha detrás de wslrelay (--wsl)
	HTTP       *jsonHTTP        // quién registró las URL de este puerto en http.sys
	Regla      *ports.ProxyRule // la regla de portproxy que tiene este puerto
	// OtrasReglas: las demás reglas del mismo puerto. La fila funde IPv4 e IPv6,
	// y una regla que escucha en 0.0.0.0 y otra en [::] (v4tov4 y v6tov4) son
	// dos reglas distintas sobre el mismo puerto: se borran por separado.
	OtrasReglas []ports.ProxyRule
	SoloWSL     bool // escucha dentro de WSL y no se reenvía a Windows
	// SinMirar: la fila es de un intermediario de Windows (http.sys, el IP
	// Helper) y no se pudo averiguar qué hay detrás: el motivo.
	SinMirar  string
	Direccion string // dirección para mostrar en las filas SoloWSL

	// Lo que se completa al mostrar (anotarProcesos, marcarCompartidos).
	Proyecto   *proyecto // el proyecto del proceso que se nombra, si tiene
	Alcance    alcance   // hasta dónde llega (con los nombres de las interfaces)
	Compartido bool      // el puerto lo escucha también otro proceso (TCP)
	keep       int       // columnas del programa al principio de Comando (el recorte las respeta)

	// Impostor: el dueño lleva el nombre de un proceso de Windows y no lo es
	// (el motivo, para mostrar). Una fila así no se oculta nunca.
	Impostor  string
	impNombre string // quién es el posible impostor (el dueño, o el que está detrás de http.sys)
	impPID    uint32

	socks []ports.Socket // los sockets que se fundieron en la fila

	// alt y altKeep: la forma corta de Comando (comandoCompacto), para cuando
	// no entra en la columna.
	alt     string
	altKeep int
}

// familias registra si una fila apareció en IPv4, IPv6 o ambas. Es lo único
// que se sabe de una fila armada sin sus sockets (lo que escucha solo dentro
// de WSL); las demás muestran sus direcciones de verdad.
type familias struct{ v4, v6 bool }

func (f familias) etiqueta() string {
	switch {
	case f.v4 && f.v6:
		return "∗ v4+v6"
	case f.v6:
		return "[::]"
	default:
		return "0.0.0.0"
	}
}

// filasDeListado arma las filas fusionadas a partir de los sockets. Agrupa por
// (dueño, protocolo, puerto) uniendo familias; marca cuáles son de Windows y
// cuáles clientes UDP efímeros (ruido) para poder ocultarlas. Puro y ordenado
// (menorFila): la misma tabla en otro orden da las mismas filas en el mismo
// orden. O(sockets · log).
func filasDeListado(socks []ports.Socket, f *reaper.Forest, svc map[uint32][]reaper.Service, cfg reaper.Config) []fila {
	// Todo lo que "ocupa" un puerto: TCP en LISTEN y cualquier UDP.
	holds := ports.Filter{}
	type clave struct {
		owner reaper.ID
		proto ports.Proto
		port  uint16
	}
	orden := []clave{}
	filas := map[clave]*fila{}
	for _, s := range socks {
		if holds.Relate(s) != ports.Holds {
			continue
		}
		// Una fila por quien sostiene el socket (reaper: Forest.Hold): su
		// dueño real, o los que lo heredaron si el dueño murió. El proceso que
		// hoy tiene el PID de un muerto no es el dueño: la fila no puede salir
		// a su nombre (ni heredar su clase, y ocultarse si es de Windows).
		duenos, heredado := duenosSocket(f, s)
		if len(duenos) == 0 {
			duenos = []reaper.Proc{{}} // nadie: la fila del PID que ya no está
		}
		for _, p := range duenos {
			owner, pid := p.ID, p.PID
			if pid == 0 {
				owner, pid = reaper.ID{PID: s.PID}, s.PID
			}
			k := clave{owner, s.Proto, s.Port()}
			fl := filas[k]
			if fl == nil {
				nombre, esWin := nombreYclase(f, s, p, svc, cfg)
				fl = &fila{
					Port: s.Port(), Proto: s.Proto, PID: pid, Owner: owner, Heredado: heredado,
					Name: nombre, Servicio: servicioDe(f, s, p, svc, cfg), EsWindows: esWin, Since: s.Since,
				}
				if p.PID != 0 {
					if fl.Relay = reaper.RelayFor(p, s, f, svc, cfg); fl.Relay != reaper.RelayNone {
						fl.Via = nombre
					}
					if st, por := reaper.StandingOf(p, f, cfg.SystemRoot); st == reaper.StandingImpostor {
						fl.Impostor, fl.impNombre, fl.impPID = por, nombre, p.PID
					}
				}
				filas[k] = fl
				orden = append(orden, k)
			}
			fl.socks = append(fl.socks, s)
			if s.Family() == ports.IPv6 {
				fl.Familias.v6 = true
			} else {
				fl.Familias.v4 = true
			}
			if s.Ephemeral {
				fl.efimero = true
			}
			if fl.Servicio == "" {
				fl.Servicio = servicioDe(f, s, p, svc, cfg)
			}
		}
	}
	out := make([]fila, 0, len(orden))
	for _, k := range orden {
		out = append(out, *filas[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return menorFila(out[i], out[j]) })
	return out
}

// menorFila es el orden del listado: puerto, protocolo, PID y la primera
// dirección. Sin los dos últimos, las filas de un mismo puerto (el 5353 de
// chrome, adb y el overlay de NVIDIA) salían en el orden de la tabla de
// Windows, que cambia entre corridas.
func menorFila(a, b fila) bool {
	if a.Port != b.Port {
		return a.Port < b.Port
	}
	if a.Proto != b.Proto {
		return a.Proto < b.Proto
	}
	if pa, pb := pidFila(a), pidFila(b); pa != pb {
		return pa < pb
	}
	if a.Linux != nil && b.Linux != nil && a.Linux.PID != b.Linux.PID {
		return a.Linux.PID < b.Linux.PID // el mismo wslrelay para dos procesos Linux
	}
	return direccionFila(a) < direccionFila(b)
}

// mostrar es qué del listado se ve además de lo común.
type mostrar struct {
	system   bool // --system: lo de Windows
	efimeros bool // --all-states: los clientes UDP efímeros (no escuchan: reciben las respuestas de lo que mandaron)
}

// sinMostrar cuenta lo que no se muestra. Para el pie, los procesos de Windows
// DISTINTOS (un svchost con diez puertos es uno) y, aparte, los sockets de
// clientes UDP (chrome tiene decenas: no son de Windows ni escuchan nada).
type sinMostrar struct {
	filas    int // filas ocultas
	windows  int // procesos de Windows distintos entre ellas
	efimeros int // filas de clientes UDP efímeros (que no son de Windows)
	linux    int // con --wsl: sockets de los demonios del sistema de una distro
}

// visibles separa las filas a mostrar de las ocultas: lo de Windows salvo
// --system, los clientes UDP efímeros salvo --all-states.
func visibles(filas []fila, m mostrar) (muestra []fila, oc sinMostrar) {
	pids := map[uint32]bool{}
	for _, f := range filas {
		switch {
		case f.EsWindows && !m.system:
			oc.filas++
			if !pids[f.PID] {
				pids[f.PID] = true
				oc.windows++
			}
		case f.efimero && !f.EsWindows && !m.efimeros:
			oc.filas++
			oc.efimeros++
		default:
			muestra = append(muestra, f)
		}
	}
	return muestra, oc
}

// ownerID resuelve el PID de un socket a una identidad viva; si no hay proceso
// vivo con ese PID (dueño fantasma), queda solo el PID.
func ownerID(f *reaper.Forest, pid uint32) reaper.ID {
	if p, ok := f.LivePID(pid); ok {
		return p.ID
	}
	return reaper.ID{PID: pid}
}

// duenosSocket dice quién sostiene un socket de la tabla, con la MISMA regla
// que la planificación (reaper.Forest.Hold): el dueño vivo si nació antes que
// el socket; si no, los que heredaron el socket del muerto (heredado es el PID
// muerto que figura en la tabla). Vacío: nadie lo sostiene (el kernel lo está
// soltando).
func duenosSocket(f *reaper.Forest, s ports.Socket) (duenos []reaper.Proc, heredado uint32) {
	h := f.Hold(s.PID, s.Since)
	if h.Live {
		return []reaper.Proc{h.Owner}, 0
	}
	if len(h.Heirs) == 0 {
		return nil, 0
	}
	return h.Heirs, s.PID
}

// servicioDe da el NOMBRE DEL SERVICIO dueño del socket, pero solo cuando eso
// aporta: dentro de un svchost (donde el exe es inútil y el servicio es lo que
// importa) o cuando el PID aloja servicios. Para un proceso común, el "módulo
// dueño" que informa la pila TCP/IP es apenas el nombre del exe (node.exe): no
// es un servicio y no se muestra como tal. Tampoco para un programa que solo
// se LLAMA svchost.exe: de un impostor se muestra la línea de comandos.
func servicioDe(f *reaper.Forest, s ports.Socket, p reaper.Proc, svc map[uint32][]reaper.Service, cfg reaper.Config) string {
	if s.Service == "" || p.PID != s.PID {
		return "" // sin dato, o la fila es de un heredero: el módulo es el del muerto
	}
	esSvchost := reaper.IsServiceHost(p, f, cfg.SystemRoot)
	_, alojaServicios := svc[s.PID]
	if esSvchost || alojaServicios {
		return s.Service
	}
	return ""
}

// nombreYclase da el nombre a mostrar del dueño (p, el que salió de
// duenosSocket; cero si nadie sostiene el socket) y si es un proceso de
// Windows.
func nombreYclase(f *reaper.Forest, s ports.Socket, p reaper.Proc, svc map[uint32][]reaper.Service, cfg reaper.Config) (string, bool) {
	if p.PID == 0 {
		if s.PID == 0 {
			return "—", true // resto sin dueño (TIME_WAIT): cosa del sistema
		}
		return "PID " + u32(s.PID) + " (ya no está)", false
	}
	return nombreVisible(p.Name, p.PID), reaper.IsWindowsProc(p, f, svc, cfg)
}

// listadoGeneral es el comando sin puertos: muestra qué escucha la máquina.
func listadoGeneral(t *tui.Term, sys *sistema, o opciones) int {
	ctx, stop := contexto()
	defer stop()
	forest, socks, svc, cfg, err := recolectar(ctx, sys, o)
	if err != nil {
		return fallo(t, err)
	}
	filas := fundirPublicaciones(filasDeListado(socks, forest, svc, cfg))
	// Lo que figura a nombre de Windows y es de otro (una URL de http.sys, una
	// regla de portproxy) se averigua ANTES de ocultar lo de Windows: el
	// servidor de un programa del usuario no puede quedar escondido detrás de
	// "System".
	filas = anotarWindows(ctx, sys, filas, forest, svc, cfg)
	// Un puerto compartido se juzga con TODO lo que lo escucha, también lo que
	// después se oculta: un servicio de Windows en [::] y tu node en
	// 127.0.0.1 son la misma historia.
	compartidos := marcarCompartidos(filas)
	muestra, ocultas := visibles(filas, mostrar{system: o.system, efimeros: o.allStates})
	var soloWSL []fila
	muestra, soloWSL, ocultas.linux = anotarListado(ctx, sys, muestra, forest, o)
	ocultas.filas += ocultas.linux
	anotarProcesos(sys, forest, muestra)

	var mirada *miradaWSL
	if o.wsl {
		mirada = sys.wsl.mirada()
	}
	if o.json {
		return imprimirJSONListado(t, append(muestra, soloWSL...), ocultas, mirada, compartidos)
	}

	t.Lines(t.Header("killport", "qué escucha en cada puerto", o.version))
	if len(muestra) == 0 && len(soloWSL) == 0 {
		t.Lines(estadoCon(t, tui.Margin, tui.Info, "No hay nada escuchando", pieOcultas(ocultas)))
		t.Lines(notasWSL(t, mirada))
		t.Blank()
		return cli.ExitOK
	}
	numerar := t.CanPrompt() && !o.list && len(muestra) > 0
	if len(muestra) > 0 {
		t.Lines(tablaListado(t, sys, muestra, numerar, false))
		t.Blank()
	}
	if len(soloWSL) > 0 {
		t.Line(t.Paint(tui.Subtle, "Solo dentro de WSL (no se reenvían a Windows):"))
		t.Lines(tablaListado(t, sys, soloWSL, false, true))
		t.Blank()
	}
	t.Lines(notasCompartidos(t, compartidos, muestra))
	t.Lines(notasImpostores(t, muestra))
	t.Lines(notasWSL(t, mirada))
	t.Lines(pieListado(t, ocultas))
	t.Blank()

	if numerar {
		return preguntarCual(t, sys, muestra, o)
	}
	return cli.ExitOK
}

// anotarProcesos completa lo que se muestra del proceso de cada fila: la
// columna COMANDO —el nombre del servicio si lo hay, o la línea de comandos
// legible (comando.go)— y el PROYECTO. Una consulta por dueño mostrado (son
// pocos tras ocultar lo de Windows), recordada en el sistema. El proceso de
// una fila de http.sys es el que registró la URL; una fila de un contenedor o
// de un proceso Linux no tiene proyecto de Windows que mostrar.
func anotarProcesos(sys *sistema, forest *reaper.Forest, filas []fila) {
	home := perfil()
	for i := range filas {
		f := &filas[i]
		f.Alcance = alcanceFila(sys, *f)
		id := f.Owner
		if f.HTTP != nil {
			id = ownerID(forest, f.HTTP.PID)
		}
		if f.Contenedor == nil && f.Linux == nil && f.Regla == nil && id.PID > 4 {
			if p, ok := sys.proyectoDe(id); ok {
				f.Proyecto = &p
			}
		}
		switch {
		case f.Comando != "":
			continue // ya lo dijo lo de atrás del relay (contenedor, proceso Linux, URL)
		case f.Servicio != "":
			// El nombre del servicio es el dato: el recorte lo respeta y se come
			// el rótulo ("Dnscache · servicio" → "Dnscache · se…").
			f.Comando, f.keep = f.Servicio+" · servicio", tui.Width(f.Servicio)
			continue
		case f.Owner.PID == 0:
			continue
		}
		if p, ok := forest.Lookup(f.Owner); ok {
			f.Ruta = p.Path
		}
		if d, err := sys.detalle(f.Owner); err == nil {
			f.Linea = unaLinea(d.CommandLine)
			f.Comando, f.keep = comandoLegible(f.Linea, rutasDe{cwd: d.Cwd, home: home})
			if alt, k := comandoCompacto(f.Linea); alt != f.Comando {
				f.alt, f.altKeep = alt, k
			}
		}
	}
}

// perfil es la carpeta del usuario, para acortar rutas con ~.
func perfil() string { return os.Getenv("USERPROFILE") }

// marcarCompartidos marca las filas de los puertos TCP que escuchan dos o más
// procesos distintos, y devuelve esos puertos con quién contesta cada
// destino (para las notas y el JSON).
func marcarCompartidos(filas []fila) []compartido {
	ocs := ocupantesDe(filas)
	cs := compartidos(ocs)
	marcados := map[uint16]bool{}
	for _, c := range cs {
		marcados[c.port] = true
	}
	for i := range filas {
		if filas[i].Proto == ports.TCP && marcados[filas[i].Port] {
			filas[i].Compartido = true
		}
	}
	return cs
}

// ocupantesDe arma los ocupantes de los puertos a partir de las filas: una
// fila es un proceso en un puerto.
func ocupantesDe(filas []fila) []ocupante {
	ocs := make([]ocupante, len(filas))
	for i, f := range filas {
		ocs[i] = ocupante{nombre: quienEs(nombreFila(f), pidFila(f)), proceso: nombreFila(f), pid: pidFila(f), socks: f.socks, externo: f.Contenedor != nil || f.Linux != nil}
	}
	return ocs
}

// notasCompartidos cuenta, debajo de la tabla, cada puerto marcado con "!":
// quién lo escucha y a quién llega cada destino. Solo los puertos que tienen
// alguna fila visible (el resto no se ve en la tabla).
func notasCompartidos(t *tui.Term, cs []compartido, visibles []fila) []string {
	se := map[uint16]bool{}
	for _, f := range visibles {
		if f.Compartido {
			se[f.Port] = true
		}
	}
	var out []string
	for _, c := range cs {
		if !se[c.port] {
			continue
		}
		titulo, detalle := c.lineas()
		out = append(out, tui.Margin+t.Status(tui.Warn, titulo, ""))
		for _, d := range detalle {
			out = append(out, renglones(t, tui.Margin+"    ", tui.Faint, d)...)
		}
		out = append(out, renglones(t, tui.Margin+"    ", tui.Faint, "→ killport localhost:"+u32(uint32(c.port))+" cierra solo al que contesta localhost; killport "+u32(uint32(c.port))+", a todos")...)
	}
	return out
}

// notasImpostores explica, debajo de la tabla, las filas marcadas "(!)": un
// proceso que lleva el nombre de uno de Windows y no está donde tendría que
// estar, o no lo lanzó quien tendría que haberlo lanzado. Una nota por
// proceso, con el motivo entero (en la tabla no entra).
func notasImpostores(t *tui.Term, filas []fila) []string {
	var out []string
	visto := map[uint32]bool{}
	for _, f := range filas {
		if f.Impostor == "" || visto[f.impPID] {
			continue
		}
		visto[f.impPID] = true
		out = append(out, estadoCon(t, tui.Margin, tui.Warn, nombreVisible(f.impNombre, f.impPID)+" (PID "+u32(f.impPID)+"): posible impostor", f.Impostor)...)
	}
	return out
}

// notasWSL dice cómo le fue a --wsl: que no se pudo preguntar, que no corre
// ninguna distro (y por eso no hay nada que sumar: killport no las arranca) o
// en cuál no se pudo mirar. Sin --wsl (mirada nil) o si salió todo bien, nada.
func notasWSL(t *tui.Term, m *miradaWSL) []string {
	var out []string
	switch {
	case m == nil:
	case m.Error != "":
		out = append(out, estadoCon(t, tui.Margin, tui.Warn, "No pude mirar dentro de WSL", m.Error)...)
	case len(m.Distros) == 0:
		out = append(out, pista(t, "WSL: no hay ninguna distro corriendo (killport no arranca distros)")...)
	case !slices.ContainsFunc(m.Distros, func(d string) bool { return !distroDeMotor(d) }):
		// La distro de Docker Desktop no es del usuario: lo suyo se ve por sus
		// contenedores. Para --wsl es lo mismo que ninguna.
		out = append(out, pista(t, "WSL: solo corre "+strings.Join(m.Distros, ", ")+", la de Docker Desktop; ninguna distro tuya (killport no arranca distros)")...)
	default:
		for _, d := range m.Distros {
			if e, mal := m.Fallas[d]; mal {
				out = append(out, estadoCon(t, tui.Margin, tui.Warn, "No pude mirar dentro de "+d, e)...)
			}
		}
	}
	return out
}

// tablaListado arma la tabla sin bordes. Con numerar, una primera columna "#"
// para que el usuario elija qué liberar.
//
// Qué cede cuando no hay lugar (tui.Table): ALCANCE, PROCESO, PROYECTO y
// COMANDO se angostan —la más ancha primero— hasta su mínimo; si ni así
// entra, se van la DIRECCIÓN exacta (el ALCANCE ya dice lo que importa),
// DESDE y PROYECTO (el bloque de `killport P` lo dice entero). ALCANCE no
// baja de lo que ocupa su texto más largo que no es una interfaz: el "(v6)"
// de "toda la red (v6)" es justo lo que la columna vino a decir. PROYECTO no
// aparece si ninguna fila tiene proyecto. soloWSL: la tabla de lo que
// escucha solo dentro de WSL, donde el ALCANCE es el mismo en todas.
func tablaListado(t *tui.Term, sys *sistema, filas []fila, numerar, soloWSL bool) []string {
	conProyecto := false
	minAlcance := 12
	for _, f := range filas {
		conProyecto = conProyecto || proyectoFila(f) != ""
		if al := textoAlcance(sys, f); len(f.Alcance.interfaces) == 0 {
			minAlcance = max(minAlcance, min(tui.Width(al), 16))
		}
	}
	cols := []tui.Col{
		{Title: "PUERTO", Align: tui.Right},
		{Title: "PROTO"},
		{Title: "ALCANCE", Flex: true, Min: minAlcance},
		{Title: "DIRECCIÓN", Drop: 1},
		{Title: "PID", Align: tui.Right},
		// PROCESO se recorta por el medio: lo que distingue a dos contenedores
		// de compose (o a un heredero) está al final del nombre.
		{Title: "PROCESO", Flex: true, Min: 14, Middle: true},
	}
	if conProyecto {
		cols = append(cols, tui.Col{Title: "PROYECTO", Flex: true, Min: 10, Drop: 3})
	}
	cols = append(cols, tui.Col{Title: "DESDE", Drop: 2}, tui.Col{Title: "COMANDO", Flex: true, Min: 20, Middle: true})
	if numerar {
		cols = append([]tui.Col{{Title: "#", Align: tui.Right}}, cols...)
	}
	if soloWSL {
		cols = slices.DeleteFunc(cols, func(c tui.Col) bool { return c.Title == "ALCANCE" })
	}
	rows := make([][]tui.Cell, len(filas))
	now := time.Now()
	for i, f := range filas {
		puerto := tui.C(strconv.Itoa(int(f.Port)), tui.Lavender)
		if f.Compartido {
			puerto = tui.C("! "+strconv.Itoa(int(f.Port)), tui.Peach)
		}
		celdas := []tui.Cell{
			puerto,
			tui.C(f.Proto.String(), tui.Subtle),
			tui.C(textoAlcance(sys, f), tui.Subtle),
			tui.C(direccionFila(f), tui.Faint),
			tui.C(textoPID(pidFila(f)), tui.Subtle),
			tui.C(nombreFila(f), tui.Text),
		}
		if conProyecto {
			celdas = append(celdas, tui.C(proyectoFila(f), tui.Sky))
		}
		celdas = append(celdas, tui.C(desde(f.Since, now), tui.Subtle), tui.Cell{Text: comandoListado(f), Color: tui.Faint, Keep: f.keep, Alt: f.alt, AltKeep: f.altKeep})
		if numerar {
			celdas = append([]tui.Cell{tui.C(strconv.Itoa(i+1), tui.Peach)}, celdas...)
		}
		if soloWSL {
			k := 2 // PUERTO, PROTO, ALCANCE…
			if numerar {
				k++
			}
			celdas = slices.Delete(celdas, k, k+1)
		}
		rows[i] = celdas
	}
	return t.Table(cols, rows)
}

// proyectoFila es la columna PROYECTO: el proyecto del proceso (su carpeta,
// su rama), o el proyecto y servicio de compose de un contenedor.
func proyectoFila(f fila) string {
	switch {
	case f.Proyecto != nil:
		return f.Proyecto.corto()
	case f.Contenedor != nil && f.Contenedor.Proyecto != "":
		s := f.Contenedor.Proyecto
		if f.Contenedor.Servicio != "" {
			s += "/" + f.Contenedor.Servicio
		}
		return s
	}
	return ""
}

// textoAlcance es la columna ALCANCE (la ya calculada, o la que se calcula).
func textoAlcance(sys *sistema, f fila) string {
	if f.Alcance.texto != "" {
		return f.Alcance.texto
	}
	return alcanceFila(sys, f).texto
}

// alcanceFila es la columna ALCANCE: hasta dónde llega lo que escucha, con el
// nombre de la interfaz si está atado a una dirección concreta. Lo que
// escucha solo dentro de WSL no llega a Windows.
func alcanceFila(sys *sistema, f fila) alcance {
	if f.SoloWSL {
		return alcance{texto: "dentro de WSL", clave: "wsl"}
	}
	var nombre func(netip.Addr) string
	if sys != nil {
		nombre = sys.interfaz
	}
	return alcanceDe(direccionesDe(f.socks), nombre)
}

// pidFila es la columna PID: siempre un PID de Windows. El del proceso que
// se nombra si está detrás de http.sys (el que registró la URL); el de
// wslrelay en una fila de un proceso Linux (el PID de Linux es de otro
// espacio: `taskkill /PID` con él le pegaría a otro proceso; va en PROCESO).
// 0 en lo que escucha solo dentro de WSL (no hay proceso de Windows).
func pidFila(f fila) uint32 {
	if f.HTTP != nil {
		return f.HTTP.PID
	}
	return f.PID
}

// textoPID es la celda de PID: "—" si no hay proceso de Windows.
func textoPID(pid uint32) string {
	if pid == 0 {
		return "—"
	}
	return u32(pid)
}

// tituloLinux nombra a un proceso Linux con SU PID y su distro: "python3
// (323) en Ubuntu".
func tituloLinux(p procesoLinux) string {
	return p.Comm + " (" + u32(p.PID) + ") en " + p.Distro
}

// nombreFila es la columna PROCESO; un posible impostor va marcado, y la nota
// al pie de la tabla dice por qué.
func nombreFila(f fila) string {
	n := f.Name
	if f.Titulo != "" {
		n = f.Titulo
	}
	if f.Heredado != 0 {
		n += " (heredado del PID " + u32(f.Heredado) + ")"
	}
	if f.Impostor != "" {
		return n + " (!)"
	}
	return n
}

// direccionFila es la columna DIRECCIÓN: las direcciones de verdad de sus
// sockets ("127.0.0.1", "[::1]", "∗ v4+v6" si son los dos comodines), o la
// dirección Linux de lo que escucha solo dentro de WSL. Una fila armada sin
// sus sockets dice al menos las familias.
func direccionFila(f fila) string {
	switch {
	case f.SoloWSL && f.Direccion != "":
		return f.Direccion
	case len(f.socks) > 0:
		return textoDirecciones(direccionesDe(f.socks))
	}
	return f.Familias.etiqueta()
}

func comandoListado(f fila) string {
	if f.Comando != "" {
		return f.Comando
	}
	return "—"
}

// desde es la columna DESDE, con una sola unidad ("hace 28 h"): en una
// columna, el detalle de "hace 28 h 41 min" ocupa el doble y no cambia la
// lectura. El bloque de un objetivo dice el tiempo entero.
func desde(tt, now time.Time) string {
	if tt.IsZero() {
		return "—"
	}
	return tui.AgoShort(tt, now)
}

// pieListado son las pistas del final del listado: qué no se muestra y cómo
// cerrar. Se parten al ancho de la consola, como las demás pistas: a 100
// columnas, la de lo oculto la cortaba la consola en "--" / "all-states".
func pieListado(t *tui.Term, oc sinMostrar) []string {
	var out []string
	if oc.filas > 0 {
		out = append(out, pista(t, pieOcultas(oc))...)
	}
	return append(out, pista(t, "killport <puerto> cierra lo que ocupa ese puerto")...)
}

// pieOcultas dice qué no se muestra y cómo verlo: "19 procesos de Windows
// ocultos · --system los muestra; 75 sockets UDP de clientes ocultos ·
// --all-states los muestra".
func pieOcultas(oc sinMostrar) string {
	var partes []string
	if oc.windows > 0 {
		partes = append(partes, tui.Count(int64(oc.windows), "proceso de Windows oculto", "procesos de Windows ocultos")+" · --system los muestra")
	}
	if oc.efimeros > 0 {
		partes = append(partes, tui.Count(int64(oc.efimeros), "socket UDP de clientes oculto", "sockets UDP de clientes ocultos")+" · --all-states los muestra")
	}
	if oc.linux > 0 {
		partes = append(partes, tui.Count(int64(oc.linux), "socket del sistema de Linux oculto", "sockets del sistema de Linux ocultos")+" · --system los muestra")
	}
	return strings.Join(partes, "; ")
}

// preguntarCual ofrece liberar una de las filas numeradas. Enter o "n" salen
// sin tocar nada.
func preguntarCual(t *tui.Term, sys *sistema, filas []fila, o opciones) int {
	resp, ok := t.Prompt(t.Paint(tui.Lavender, "¿Cuál liberar?") + t.Paint(tui.Subtle, " número, o Enter para salir: "))
	if !ok || resp == "" || strings.EqualFold(resp, "n") {
		return cli.ExitOK
	}
	n, err := strconv.Atoi(resp)
	if err != nil || n < 1 || n > len(filas) {
		t.Line(t.Status(tui.Warn, "No entendí", "poné el número de una fila"))
		return cli.ExitOK
	}
	t.Blank()
	f := filas[n-1]
	if f.Compartido && !f.Owner.Zero() {
		// Un puerto que escuchan varios: la fila que se eligió es UN proceso,
		// y es a ese al que se apunta (los demás quedan al margen).
		o.elegido = f.Owner
	}
	return correrPuertos(t, sys, ports.Whole(ports.Of(f.Port)), o)
}
