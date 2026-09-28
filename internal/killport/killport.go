//go:build windows

// Package killport implementa `killport`: muestra qué proceso ocupa un puerto,
// lo cierra con la prudencia de apretar Ctrl+C en su ventana (y escalando a la
// fuerza solo si hace falta) y verifica que el puerto haya quedado libre. Sin
// puertos, lista qué escucha la máquina.
//
// La lógica del cierre vive en dos paquetes puros —internal/ports (vocabulario
// de puertos) e internal/reaper (identidad, bosque de procesos, planificación,
// motor de escalada y verificación)— probados contra un simulador de eventos
// discretos. Este paquete es la interfaz de línea de comandos, el ADAPTADOR
// que implementa reaper.OS con internal/win (sistema.go) y los dominios que
// miran detrás de los intermediarios: el motor de contenedores (docker*.go,
// con su cliente HTTP mínimo, http1.go), WSL (wsl*.go), http.sys (httpsys.go)
// y portproxy. Además tiene lógica propia de peso: la configuración de red
// (config*.go: el planificador de --reserve/--fix-dynamic y el trabajo del
// proceso elevado, trabajo.go), la detección de proyectos (proyecto.go) y la
// presentación (listado, bloques, JSON). Lo que es puro va sin etiqueta de
// compilación (y se prueba igual en cualquier sistema); lo que toca la API,
// con //go:build windows.
package killport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

type opciones struct {
	list      bool
	yes       bool
	force     bool
	tree      bool
	tcp       bool
	udp       bool
	allStates bool
	system    bool
	json      bool
	timeout   time.Duration
	noDocker  bool // --no-docker: no preguntarle al motor de contenedores
	wsl       bool // --wsl: en el listado, mirar dentro de las distros de WSL que corren
	manual    bool // --manual: el servicio que se detiene queda en inicio manual

	// Las acciones de configuración de red (config.go): no cierran nada.
	reserve    bool // --reserve: que WinNAT no vuelva a tomar esos puertos
	unreserve  bool // --unreserve: deshacerlo
	fixDynamic bool // --fix-dynamic: devolver el rango dinámico al de fábrica

	// Elegir sin puertos (seleccion.go): --orphans, -n/--name, --project.
	sel seleccion

	// --free (libre_windows.go): buscar puertos libres de verdad; no cierra nada.
	free      bool
	count     int  // --count N: cuántos (1 si no se dijo)
	countDado bool // se pasó --count (sin --free no tiene sentido)

	// --wait / --wait-free (espera_windows.go): esperar; no cierra nada.
	wait, waitFree         time.Duration // el plazo de cada una
	waitDado, waitFreeDado bool
	any                    bool          // --any: alcanza con uno de los puertos
	stable                 time.Duration // --stable: que se sostenga ese tiempo
	stableDado             bool
	nuevos                 bool // --new: solo lo que empieza a escuchar después de arrancar

	timeoutDado bool // se pasó --timeout (es del cierre prolijo: con --free o --wait no aplica)

	version string // la de internal/version, para la cabecera

	// elegido: del listado interactivo se eligió la fila de UN proceso en un
	// puerto que escuchan varios; se apunta a ese (los demás quedan al margen).
	elegido reaper.ID

	// Ocultos: los modos internos.
	elevatedJob string // el trabajo codificado que corre la copia elevada
	resultPath  string // dónde escribe su resultado la copia elevada
}

// settleWindow es cuánto se confirma que un puerto recién liberado SIGA libre
// antes de cantarlo: atrapa al supervisor (nodemon/pm2) que lo recupera al
// instante. Corto para no demorar el caso normal, largo para ganarle a un
// respawn típico de node (~40 ms de arranque).
const settleWindow = 350 * time.Millisecond

// filtro arma el ports.Filter a partir de los flags de protocolo y estado.
func (o opciones) filtro() ports.Filter {
	return ports.Filter{TCP: o.tcp, UDP: o.udp, AllStates: o.allStates}
}

// Main es el punto de entrada del subcomando.
func Main(t *tui.Term, version string, args []string) int {
	// Modos ayudante (consola de otro proceso): corren en la copia DETACHED,
	// sin tui ni ceremonia, antes de tocar nada. Devuelven su propio código.
	if code, ok := modoAyudante(args); ok {
		return code
	}

	o := opciones{timeout: 3 * time.Second, version: version}
	app := cli.New("killport", version, "libera el puerto que necesitás, con cuidado")
	app.Usage = []string{
		"killport                     lista qué escucha y pregunta cuál liberar",
		"killport 3000                cierra lo que ocupa el 3000 (pregunta antes)",
		"killport 3000 5173 8080,8081 5000-5003   varios puertos y rangos",
		"killport localhost:5173      solo el que contesta localhost en el 5173",
		"killport --orphans           lo que quedó escuchando sin su terminal",
		"killport 5173 --wait 60s     espera a que el 5173 escuche (no cierra nada)",
	}
	app.Section("qué hacer")
	app.Bool(&o.list, "list", 'l', "solo mirar: no cierra nada")
	app.Bool(&o.yes, "yes", 'y', "no preguntar: cerrar directamente")
	app.Bool(&o.force, "force", 'f', "sin cierre prolijo: terminar en seco")
	app.Bool(&o.tree, "tree", 't', "también los procesos hijos")
	app.Bool(&o.manual, "manual", 0, "el servicio que se detiene queda en inicio manual: no vuelve al reiniciar")
	app.Func("timeout", 0, "TIEMPO", "espera del cierre prolijo antes de terminar en seco", "3s", func(v string) error {
		d, err := plazoProlijo(v)
		if err == nil {
			o.timeout, o.timeoutDado = d, true
		}
		return err
	})
	app.Section("elegir sin nombrar puertos (se combinan: cada uno achica)")
	app.Bool(&o.sel.huerfanos, "orphans", 0, "los que quedaron sueltos: su padre (la terminal, npm, el editor) ya no existe")
	app.Func("name", 'n', "NOMBRE", "solo los procesos con ese nombre o ruta: node, python*, C:\\dev\\*; se repite o va con comas", "", o.sel.sumarNombres)
	app.Func("project", 0, "CARPETA", "solo lo que corre dentro de esa carpeta (. = esta)", "", func(v string) error {
		dir, err := carpetaDeProyecto(v)
		o.sel.carpeta = dir
		return err
	})
	app.Section("un puerto libre (no cierra nada)")
	app.Bool(&o.free, "free", 0, "el primer puerto libre de verdad desde el que nombrás (o en un rango; sin nada, 1024–49151)")
	app.Func("count", 0, "N", "cuántos puertos libres, de 1 a 100", "1", func(v string) error {
		n, err := cantidadLibres(v)
		o.count, o.countDado = n, true
		return err
	})
	app.Section("esperar (no cierra nada)")
	app.Func("wait", 0, "TIEMPO", "hasta que los puertos ESCUCHEN, como máximo ese tiempo (hasta 30 min)", "", func(v string) error {
		d, err := plazoEspera(v)
		o.wait, o.waitDado = d, true
		return err
	})
	app.Func("wait-free", 0, "TIEMPO", "hasta que se liberen, sin cerrar nada", "", func(v string) error {
		d, err := plazoEspera(v)
		o.waitFree, o.waitFreeDado = d, true
		return err
	})
	app.Bool(&o.any, "any", 0, "alcanza con uno de los puertos (si no, todos a la vez)")
	app.Func("stable", 0, "TIEMPO", "que se sostenga ese tiempo: un reinicio (nodemon) vuelve la cuenta a cero", "", func(v string) error {
		d, err := plazoEspera(v)
		o.stable, o.stableDado = d, true
		return err
	})
	app.Bool(&o.nuevos, "new", 0, "con --wait: solo cuenta lo que empieza a escuchar después de lanzar killport")
	app.Section("puertos que reserva Windows")
	app.Bool(&o.reserve, flagReserve, 0, "reservar esos puertos para vos: WinNAT no los vuelve a tomar (permanente)")
	app.Bool(&o.unreserve, "unreserve", 0, "quitar esa reserva")
	app.Bool(&o.fixDynamic, flagFixDynamic, 0, "devolver el rango de puertos dinámicos a 49152–65535")
	app.Section("qué mirar")
	app.Bool(&o.tcp, "tcp", 0, "solo TCP")
	app.Bool(&o.udp, flagUDP, 0, "solo UDP")
	app.Bool(&o.allStates, "all-states", 'a', "además de lo que escucha, las conexiones sobre ese puerto (en el listado, los clientes UDP)")
	app.Bool(&o.system, "system", 's', "en el listado, mostrar también lo de Windows")
	app.Bool(&o.wsl, flagWSL, 0, "en el listado, sumar lo que escucha dentro de WSL (nunca arranca una distro)")
	app.Bool(&o.noDocker, "no-docker", 0, "no preguntarle a Docker/Podman qué contenedor publica el puerto")
	app.Section("salida")
	app.Bool(&o.json, "json", 0, "salida JSON para scripts (sin --yes, solo describe); sale con el mismo código que el texto")
	// Ocultos: la copia elevada y el archivo por el que devuelve su resultado.
	app.Hidden("elevated-job", "DATOS", func(v string) error { o.elevatedJob = v; return nil })
	app.Hidden("result", "RUTA", func(v string) error { o.resultPath = v; return nil })
	// Registrados para que la ayuda de errores no se queje si se cuelan; el
	// modo ayudante ya los interceptó arriba.
	app.Hidden("console-of", "PID", func(string) error { return nil })
	app.Hidden("ctrl-c-helper", "PID", func(string) error { return nil })
	app.Hidden("allow", "IDS", func(string) error { return nil })

	// Un ejemplo por cosa que sabe hacer, en el orden de la ayuda: cerrar, lo
	// que hay detrás de cada intermediario, elegir, un puerto libre, esperar,
	// lo que reserva Windows y la salida para scripts.
	app.Examples = []cli.Example{
		{Cmd: "killport 3000", Desc: "el caso típico: el dev server quedó tomando el puerto"},
		{Cmd: "killport 5173 -y", Desc: "sin preguntar, para un script"},
		{Cmd: "killport 3000 --tree", Desc: "cierra también los hijos (npm → node → …)"},
		{Cmd: "killport", Desc: "lista todo lo que escucha (oculta lo de Windows; --system lo muestra)"},
		{Cmd: "killport localhost:5173", Desc: "el navegador muestra la versión vieja: cierra solo al que contesta localhost"},
		{Cmd: "killport 8080", Desc: "si lo publica un contenedor, detiene el contenedor (no Docker Desktop)"},
		{Cmd: "killport 80 --list", Desc: "si figura System, dice qué programa registró la URL en http.sys"},
		{Cmd: "killport --wsl", Desc: "también lo que escucha dentro de las distros de WSL que corren"},
		{Cmd: "killport 3306 --manual", Desc: "detiene el servicio y lo deja en inicio manual (dice cómo deshacerlo)"},
		{Cmd: "killport --orphans -n node", Desc: "los node que quedaron escuchando después de cerrar la terminal"},
		{Cmd: "killport --project . --list", Desc: "qué escucha de este proyecto (su carpeta, su rama)"},
		{Cmd: "killport --free 3000", Desc: "el primer puerto libre de verdad desde el 3000"},
		{Cmd: "$port = killport --free 5173", Desc: "sin consola sale solo el número (en bash, PORT=$(killport --free 5173))"},
		{Cmd: "killport 5173 --wait 60s", Desc: "en PowerShell 7 o bash: npm run dev & killport 5173 --wait 60s && npx playwright test"},
		{Cmd: "killport 5173 --wait 1m --new", Desc: "solo cuenta un servidor nuevo, no el que quedó escuchando de antes"},
		{Cmd: "killport 5173 --wait 1m --stable 1s", Desc: "que se sostenga 1 s: nodemon escucha, se reinicia y vuelve"},
		{Cmd: "killport 3000 --wait-free 30s", Desc: "espera a que se libere, sin cerrar nada"},
		{Cmd: "killport 3000 --reserve", Desc: "el 3000 lo reserva Windows cada tanto: que no lo vuelva a tomar"},
		{Cmd: "killport 5432 --json", Desc: "qué ocupa el 5432, en JSON"},
	}
	app.Notes = []string{
		"Cierra como apretarías Ctrl+C en esa ventana: primero prolijo, y solo si no cede, en seco.",
		"Nunca se cierra a sí mismo ni a la terminal desde donde lo corrés. El Ctrl+C le llega a toda la consola: si el servidor la comparte con quien lo lanzó (npm, un supervisor), killport lo dice antes de preguntar.",
		"Si lo que no se toca (un servicio de Windows) sigue ocupando el puerto, cerrar a los demás no lo libera: killport lo dice, y con --yes no cierra a nadie.",
		"Un puerto de un servicio (SQL, PostgreSQL, Docker) se libera deteniendo el servicio, no matando el proceso; puede pedir administrador.",
		"Un puerto que publica un contenedor se libera deteniendo EL CONTENEDOR (por la API de Docker/Podman); Docker Desktop no se toca.",
		"Un puerto que reenvía WSL (wslrelay) se libera cerrando el proceso Linux que escucha adentro: SIGINT, SIGTERM y recién después SIGKILL.",
		"Un puerto a nombre de System (PID 4) casi siempre es http.sys trabajando para otro: killport busca quién registró la URL. El 445 y el 139 son compartir archivos.",
		"Un puerto de svchost con una regla de portproxy se libera borrando LA REGLA (pide administrador); el servicio IP Helper no se toca.",
		"Los puertos que reserva Windows (WinNAT/WSL/Docker) no tienen proceso que cerrar: killport lo explica y --reserve lo arregla para siempre.",
		"--reserve, --unreserve y --fix-dynamic cambian la red de toda la máquina: muestran el plan y preguntan siempre, también con --yes.",
		"Un proceso que lleva el nombre de uno de Windows (svchost, lsass…) sin estar en System32 ni tener el padre que corresponde se muestra como posible impostor, y no se protege ni se oculta.",
		"Una dirección apunta: localhost:5173 es el que contesta localhost (el navegador prueba ::1 primero), 127.0.0.1:5173 el que contesta esa dirección; :5173 o 5173, todo el puerto.",
		"Dos procesos pueden escuchar el mismo puerto en direcciones distintas sin que ninguno dé error: el listado los marca con ! y dice a cuál llega cada destino.",
		"--orphans, --name y --project eligen entre lo que ESCUCHA un puerto (nunca un node sin puerto, como un language server); lo que comparte el puerto con lo elegido queda al margen.",
		"--free da un puerto que nadie usa en ninguna dirección, fuera de lo que reserva Windows y del rango dinámico, confirmado con un bind exclusivo en IPv4 y en IPv6. Entre que lo da y que lo usás, otro programa lo puede tomar: usalo enseguida. Cuando killport no puede liberar un puerto, sugiere el próximo libre.",
		"--wait y --wait-free miran la tabla de Windows, no se conectan: no hay trampa de localhost ni aviso del Firewall. El plazo es obligatorio; si WinNAT tapa el puerto, --wait no espera en vano.",
		"Lo que busca o espera un servidor nuevo (--free, --wait) es TCP salvo --udp; lo que mira lo que ya está (cerrar, --wait-free, el listado), los dos protocolos.",
		"Sin consola (salida a un pipe o archivo) no pregunta: hace falta --yes para actuar.",
		"Códigos de salida: 0 listo (o ya estaba libre), 1 una parte, 2 la línea de comandos, 3 no se pudo (o venció la espera), 130 cancelado. Con --list, 0 si pudo mirar. Con --json, los mismos.",
	}

	pos, done, code := app.Start(t, args)
	if done {
		return code
	}

	sys, err := nuevoSistema()
	if err != nil {
		return fallo(t, err)
	}

	// La copia elevada: hace el trabajo codificado y escribe el resultado.
	if o.elevatedJob != "" {
		return correrElevado(sys, o)
	}

	if err := validarModos(pos, o); err != nil {
		return falloUsage(t, err)
	}
	switch {
	case o.free:
		return correrLibre(t, sys, pos, o)
	case o.waitDado || o.waitFreeDado:
		return correrEspera(t, sys, pos, o)
	}
	if o.reserve || o.unreserve || o.fixDynamic {
		if err := validarConfig(pos, o); err != nil {
			return falloUsage(t, err)
		}
		requested, err := ports.ParseSpecs(pos)
		if err != nil {
			return falloUsage(t, err)
		}
		return correrConfig(t, sys, requested, o)
	}
	if len(pos) == 0 && !o.sel.activa() {
		return listadoGeneral(t, sys, o)
	}
	req, err := ports.ParseRequest(pos)
	if err != nil {
		return falloUsage(t, err)
	}
	return correrPuertos(t, sys, req, o)
}

// carpetaDeProyecto resuelve la carpeta de --project (relativa a donde se
// corre killport) y comprueba que exista.
func carpetaDeProyecto(v string) (string, error) {
	if v == "" {
		return "", errors.New("falta la carpeta (ej.: --project . o --project C:\\dev\\shop)")
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return "", fmt.Errorf("%q no es una carpeta: %w", v, err)
	}
	st, err := os.Stat(abs)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("no existe la carpeta %s", abs)
	}
	abs = filepath.Clean(abs)
	if por := noEsProyecto(abs, perfil(), win.WindowsDirectory()); por != "" {
		return "", fmt.Errorf("eso no es un proyecto: %s (elegiría todo lo que corre ahí adentro); nombrá la carpeta del proyecto", por)
	}
	return abs, nil
}

// contextoT es un alias corto para no importar "context" en cada archivo del
// paquete (todos comparten el mismo contexto cancelable por Ctrl+C).
type contextoT = context.Context

// contexto arma el contexto que se cancela con el primer Ctrl+C del usuario.
func contexto() (context.Context, context.CancelFunc) {
	return cli.Interrupt(nil)
}

// recolectar toma la foto del sistema: sockets, procesos (bosque) y servicios,
// más la configuración (identidad propia, consola propia, carpeta de Windows).
// Es la base tanto del listado como de la planificación. No es atómica (son
// varias lecturas): el motor reconcilia por identidad, no asume una sola foto.
func recolectar(ctx context.Context, sys *sistema, o opciones) (*reaper.Forest, []ports.Socket, map[uint32][]reaper.Service, reaper.Config, error) {
	socks, err := sys.Sockets(ctx)
	if err != nil {
		return nil, nil, nil, reaper.Config{}, fmt.Errorf("no pude leer las tablas de red: %w", err)
	}
	procs, err := sys.Snapshot(ctx)
	if err != nil {
		return nil, nil, nil, reaper.Config{}, fmt.Errorf("no pude leer la lista de procesos: %w", err)
	}
	// Sin la lista de servicios no se sigue: un servicio de terceros se
	// terminaría como un proceso común (y el administrador de servicios lo
	// relanza), y las negativas de svchost perderían el nombre del servicio.
	svc, err := sys.Services()
	if err != nil {
		return nil, nil, nil, reaper.Config{}, fmt.Errorf("no pude leer la lista de servicios: %w", err)
	}
	forest := reaper.NewForest(procs)
	cfg := reaper.Config{
		Self:       identidadPropia(forest),
		OwnConsole: consolaPropia(),
		SystemRoot: win.WindowsDirectory(),
		Tree:       o.tree,
		Force:      o.force,
	}
	return forest, socks, svc, cfg, nil
}

// identidadPropia busca a killport en el bosque para no cerrarse a sí mismo ni
// a su terminal. Si no aparece (rarísimo), queda solo el PID.
func identidadPropia(f *reaper.Forest) reaper.ID {
	self := win.CurrentProcessID()
	if p, ok := f.LivePID(self); ok {
		return p.ID
	}
	return reaper.ID{PID: self}
}

// consolaPropia son los PID de la consola de killport: jamás se les manda
// Ctrl+C (matarían la terminal del usuario y a killport mismo). Vacío si
// killport no tiene consola (salida redirigida).
func consolaPropia() map[uint32]bool {
	pids, err := win.ConsoleProcessList()
	if err != nil {
		return nil
	}
	own := make(map[uint32]bool, len(pids))
	for _, p := range pids {
		own[p] = true
	}
	return own
}

// holding filtra los sockets que OCUPAN los puertos pedidos (los que el motor
// va a atacar), según el filtro de protocolo/estado.
func holding(socks []ports.Socket, filter ports.Filter, requested ports.Set) []ports.Socket {
	filter.Ports = requested
	var out []ports.Socket
	for _, s := range socks {
		if filter.Relate(s) == ports.Holds {
			out = append(out, s)
		}
	}
	return out
}

// fallo imprime un error y devuelve el código de "no se pudo".
func fallo(t *tui.Term, err error) int {
	t.Blank()
	t.Line(t.Paint(tui.Rose, "✗ ") + t.Paint(tui.Text, err.Error()))
	t.Blank()
	return cli.ExitFailure
}

// falloUsage imprime un error de línea de comandos y devuelve el código 2.
func falloUsage(t *tui.Term, err error) int {
	t.Blank()
	t.Line(t.Paint(tui.Rose, "✗ ") + t.Paint(tui.Text, err.Error()))
	t.Line(t.Paint(tui.Faint, "› killport --help muestra todas las opciones"))
	t.Blank()
	return cli.ExitUsage
}
