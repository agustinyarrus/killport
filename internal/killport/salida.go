//go:build windows

package killport

// salida.go: la salida JSON, estable para scripts. Se escribe en la salida
// estándar tal cual (sin colores ni escapes de terminal), pase lo que pase con
// la consola. Un script hace `killport 3000 --json | ConvertFrom-Json`.

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

// jsonFila es una fila del listado en JSON.
type jsonFila struct {
	Puerto uint16 `json:"puerto"`
	Proto  string `json:"proto"`
	// direcciones: las de verdad, una por una ("0.0.0.0", "::1"). alcance:
	// esta_pc | red | interfaces | wsl; interfaces: sus nombres ("Wi-Fi").
	// Todo es dato: lo que la tabla muestra armado para leer ("∗ v4+v6", la
	// línea de comandos acortada, "servicio: X") no va al JSON.
	Direcciones []string `json:"direcciones,omitempty"`
	Alcance     string   `json:"alcance,omitempty"`
	Interfaces  []string `json:"interfaces,omitempty"`
	// compartido: ese puerto (TCP) lo escuchan también otros procesos; quién
	// contesta cada destino está en compartidos, en la raíz.
	Compartido bool          `json:"compartido,omitempty"`
	Proyecto   *jsonProyecto `json:"proyecto,omitempty"`
	PID        uint32        `json:"pid"`
	Proceso    string        `json:"proceso"`
	// heredado_de: el PID que figura en la tabla murió y este proceso (pid)
	// sostiene el socket porque lo heredó.
	HeredadoDe uint32 `json:"heredado_de,omitempty"`
	Servicio   string `json:"servicio,omitempty"`
	Desde      string `json:"desde,omitempty"` // RFC3339, vacío si no se supo
	// comando y ruta: la línea de comandos y el ejecutable del proceso de
	// Windows, tal cual (vacíos si no se pudieron leer, o si la fila es de un
	// servicio o de algo detrás de un intermediario: eso va en su objeto).
	Comando   string `json:"comando,omitempty"`
	Ruta      string `json:"ruta,omitempty"`
	DeWindows bool   `json:"de_windows"`
	// intermediario: el dueño es un relay (contenedores | wsl | vscode |
	// http.sys | portproxy): el puerto es de otro y killport no lo mata. Cuando
	// se supo qué hay detrás, via es la imagen del proceso de Windows que
	// sostiene el socket (pid es el suyo) y contenedor, wsl, http o regla, lo de
	// atrás. solo_wsl: escucha dentro de WSL y no se reenvía (pid 0).
	Intermediario string          `json:"intermediario,omitempty"`
	Via           string          `json:"via,omitempty"`
	Contenedor    *jsonContenedor `json:"contenedor,omitempty"`
	WSL           *jsonLinux      `json:"wsl,omitempty"`
	HTTP          *jsonHTTP       `json:"http,omitempty"`
	Regla         *jsonRegla      `json:"regla,omitempty"`
	OtrasReglas   []*jsonRegla    `json:"otras_reglas,omitempty"` // las demás reglas de portproxy del mismo puerto
	SoloWSL       bool            `json:"solo_wsl,omitempty"`
	// no_pude_mirar: la fila es de un intermediario de Windows y no se pudo
	// averiguar qué hay detrás (el motivo); se muestra en vez de ocultarse.
	NoPudeMirar string `json:"no_pude_mirar,omitempty"`
	// impostor: lleva el nombre de un proceso de Windows y no lo es (el motivo).
	Impostor string `json:"impostor,omitempty"`
}

// jsonProyecto es de qué proyecto es un proceso: la raíz (donde está el
// marcador), su nombre y la rama de git. carpeta es la carpeta de trabajo
// del proceso; por_script, que la carpeta no era de un proyecto y el
// proyecto salió del script de su línea de comandos.
type jsonProyecto struct {
	Raiz      string `json:"raiz"`
	Nombre    string `json:"nombre"`
	Marcador  string `json:"marcador"` // package.json | go.mod | Cargo.toml | pyproject.toml | X.csproj | X.sln | .git
	Repo      string `json:"repo,omitempty"`
	Rama      string `json:"rama,omitempty"`
	Commit    string `json:"commit,omitempty"` // HEAD desacoplado
	Carpeta   string `json:"carpeta,omitempty"`
	PorScript bool   `json:"por_script,omitempty"`
}

func proyectoJSON(p *proyecto) *jsonProyecto {
	if p == nil {
		return nil
	}
	return &jsonProyecto{Raiz: p.Raiz, Nombre: p.Nombre, Marcador: p.Marcador, Repo: p.Repo, Rama: p.Rama, Commit: p.Commit,
		Carpeta: p.Carpeta, PorScript: p.PorScript}
}

// jsonAlMargen es alguien que escucha un puerto pedido y no se pidió (el
// pedido apuntó a una dirección que no contesta, o un selector no lo
// incluye): no se toca, y sigue ahí.
type jsonAlMargen struct {
	PID         uint32   `json:"pid"`
	Proceso     string   `json:"proceso,omitempty"`
	Puertos     []uint16 `json:"puertos"`
	Direcciones []string `json:"direcciones"`
}

func direccionesJSON(socks []ports.Socket) []string {
	out := []string{}
	for _, a := range direccionesDe(socks) {
		out = append(out, a.String())
	}
	return out
}

// jsonHTTP es quién está detrás de un puerto de http.sys: el proceso que
// registró las URL. Si en ese puerto hay URL de varios procesos, este es el
// primero que no es de Windows y los demás van en otros.
type jsonHTTP struct {
	PID     uint32     `json:"pid"`
	Proceso string     `json:"proceso,omitempty"`
	URLs    []string   `json:"urls"`
	Otros   []jsonHTTP `json:"otros,omitempty"` // los otros, sin otros propios
}

// jsonRegla es una regla de `netsh interface portproxy`.
type jsonRegla struct {
	Clase   string `json:"clase"`   // v4tov4 | v4tov6 | v6tov4 | v6tov6
	Escucha string `json:"escucha"` // dirección donde escucha ("*" = todas)
	Puerto  uint16 `json:"puerto"`
	Destino string `json:"destino"` // adonde conecta, dirección:puerto
	Borrar  string `json:"borrar"`  // el comando que la borra (pide administrador)
}

func reglaJSON(r ports.ProxyRule) *jsonRegla {
	destino := r.Target
	if i := strings.LastIndexByte(destino, '/'); i >= 0 {
		destino = destino[:i] + ":" + destino[i+1:]
	}
	return &jsonRegla{Clase: r.Kind.String(), Escucha: r.ListenAddr, Puerto: r.ListenPort, Destino: destino,
		Borrar: "netsh " + strings.Join(r.DeleteArgs(), " ")}
}

// jsonInicio es cómo arranca un servicio: si vuelve solo después de detenerlo.
type jsonInicio struct {
	Inicio       string   `json:"inicio"` // auto | auto-diferido | manual | deshabilitado | arranque | sistema
	Disparadores int      `json:"disparadores"`
	Recuperacion []string `json:"recuperacion"` // qué hace Windows si se cae: reiniciar-servicio | reiniciar-equipo | comando | nada
	VuelveSolo   bool     `json:"vuelve_solo"`
}

func inicioJSON(c win.ServiceConfig) *jsonInicio {
	vuelve, _ := vuelveSolo(c)
	return &jsonInicio{Inicio: inicioClave(c), Disparadores: c.Triggers, Recuperacion: recuperacionClaves(c), VuelveSolo: vuelve}
}

// jsonContenedor describe un contenedor que publica puertos en el host.
type jsonContenedor struct {
	ID       string          `json:"id"`
	Nombre   string          `json:"nombre"`
	Imagen   string          `json:"imagen,omitempty"`
	Motor    string          `json:"motor,omitempty"`
	Proyecto string          `json:"proyecto,omitempty"` // compose
	Servicio string          `json:"servicio,omitempty"` // compose
	Reinicio string          `json:"reinicio,omitempty"` // no | always | unless-stopped | on-failure
	Puertos  []jsonPublicado `json:"puertos"`
}

type jsonPublicado struct {
	Host       uint16 `json:"host"`
	Contenedor uint16 `json:"contenedor"`
	Proto      string `json:"proto"`
	IP         string `json:"ip,omitempty"`
}

// jsonLinux describe un proceso Linux dentro de una distro de WSL.
type jsonLinux struct {
	Distro   string `json:"distro"`
	PID      uint32 `json:"pid"`
	Arranque uint64 `json:"arranque"` // starttime (ticks desde el arranque): con el PID, su identidad
	Proceso  string `json:"proceso"`
	Usuario  string `json:"usuario,omitempty"`
	Comando  string `json:"comando,omitempty"`
	Desde    string `json:"desde,omitempty"`
}

func contenedorJSON(c *contenedor) *jsonContenedor {
	if c == nil {
		return nil
	}
	jc := &jsonContenedor{ID: c.ID, Nombre: c.Nombre, Imagen: c.Imagen, Motor: c.motorNombre(),
		Proyecto: c.Proyecto, Servicio: c.Servicio, Reinicio: c.Reinicio, Puertos: []jsonPublicado{}}
	for _, p := range c.Puertos {
		jc.Puertos = append(jc.Puertos, jsonPublicado{Host: p.Publico, Contenedor: p.Privado, Proto: strings.ToLower(p.Proto.String()), IP: p.IP})
	}
	return jc
}

func linuxJSON(p *procesoLinux) *jsonLinux {
	if p == nil {
		return nil
	}
	jl := &jsonLinux{Distro: p.Distro, PID: p.PID, Arranque: p.Arranque, Proceso: p.Comm, Usuario: p.Usuario, Comando: p.Cmdline}
	if !p.Desde.IsZero() {
		jl.Desde = p.Desde.Format(time.RFC3339)
	}
	return jl
}

type jsonListado struct {
	Escuchando []jsonFila `json:"escuchando"`
	// ocultos: filas que no se muestran; de ellas, los procesos de Windows
	// distintos (--system) y los sockets de clientes UDP (--all-states).
	Ocultos         int `json:"ocultos"`
	OcultosWindows  int `json:"ocultos_windows"`
	OcultosEfimeros int `json:"ocultos_efimeros"`
	// compartidos: los puertos marcados con "!" (ver jsonCompartido).
	Compartidos []jsonCompartido `json:"compartidos,omitempty"`
	// wsl (solo con --wsl): qué distros corrían y si se pudo mirar adentro.
	// Sin esto, un script no distingue "en WSL no escucha nadie" de "no pude
	// mirar".
	WSL *miradaWSL `json:"wsl,omitempty"`
}

func imprimirJSONListado(t *tui.Term, filas []fila, ocultas sinMostrar, mirada *miradaWSL, cs []compartido) int {
	doc := construirJSONListado(filas, ocultas)
	doc.WSL = mirada
	se := map[uint16]bool{}
	for _, f := range filas {
		se[f.Port] = se[f.Port] || f.Compartido
	}
	var visibles []compartido
	for _, c := range cs {
		if se[c.port] {
			visibles = append(visibles, c)
		}
	}
	doc.Compartidos = compartidosJSON(visibles)
	return escribirJSON(t, doc)
}

// construirJSONListado arma el documento del listado general.
func construirJSONListado(filas []fila, oc sinMostrar) jsonListado {
	doc := jsonListado{Ocultos: oc.filas, OcultosWindows: oc.windows, OcultosEfimeros: oc.efimeros, Escuchando: []jsonFila{}}
	for _, f := range filas {
		al := f.Alcance
		if al.texto == "" {
			al = alcanceFila(nil, f)
		}
		jf := jsonFila{
			Puerto: f.Port, Proto: f.Proto.String(),
			Alcance: al.clave, Interfaces: al.interfaces, Compartido: f.Compartido, Proyecto: proyectoJSON(f.Proyecto),
			PID: f.PID, Proceso: f.Name, HeredadoDe: f.Heredado, Servicio: f.Servicio, Comando: f.Linea, Ruta: f.Ruta, DeWindows: f.EsWindows,
			Intermediario: intermediarioJSON(f.Relay),
			Contenedor:    contenedorJSON(f.Contenedor), WSL: linuxJSON(f.Linux), SoloWSL: f.SoloWSL,
			HTTP: f.HTTP, Impostor: f.Impostor, NoPudeMirar: f.SinMirar,
		}
		if f.Regla != nil {
			jf.Regla = reglaJSON(*f.Regla)
		}
		for _, r := range f.OtrasReglas {
			jf.OtrasReglas = append(jf.OtrasReglas, reglaJSON(r))
		}
		if f.Contenedor != nil || f.Linux != nil || f.HTTP != nil || f.Regla != nil {
			jf.Via = f.Via
		}
		if !f.Since.IsZero() {
			jf.Desde = f.Since.Format(formatoHora)
		}
		switch {
		case len(f.socks) > 0:
			jf.Direcciones = direccionesJSON(f.socks)
		case f.SoloWSL && f.Direccion != "":
			jf.Direcciones = []string{f.Direccion}
		}
		doc.Escuchando = append(doc.Escuchando, jf)
	}
	return doc
}

type jsonPuerto struct {
	Puerto     uint16     `json:"puerto"`
	Proto      string     `json:"proto,omitempty"`
	Estado     string     `json:"estado"` // libre | fantasma | respawn | reservado | ocupado | no_se_toca | contesta_otro (la dirección pedida la contesta ahora al_margen)
	Dueño      *jsonDueño `json:"dueno,omitempty"`
	Supervisor *jsonDueño `json:"supervisor,omitempty"`
	Motivo     string     `json:"motivo,omitempty"`
	Consejo    string     `json:"consejo,omitempty"`
	// arrancando (con estado respawn): el relanzado todavía no escucha; el
	// puerto está libre en este instante pero lo van a volver a tomar.
	Arrancando bool `json:"arrancando,omitempty"`
	// Un puerto sin dueño: el rango excluido en el que cae, el rango dinámico
	// cuando es parte de la explicación, y las salidas que hay.
	Reservado *jsonRango    `json:"reservado,omitempty"`
	Dinamico  *jsonDinamico `json:"dinamico,omitempty"`
	Remedios  []jsonRemedio `json:"remedios,omitempty"`
	// al_margen: quedó libre PARA LO PEDIDO y sigue escuchando alguien que no
	// se pidió (con una dirección pedida: quien la contesta ahora, si alguien).
	AlMargen *jsonDueño `json:"al_margen,omitempty"`
	// alternativa: no quedó libre (o el usuario dijo que no) y este es el
	// próximo puerto libre de verdad, por si sirve otro (ver --free).
	Alternativa uint16 `json:"alternativa,omitempty"`
}

// jsonRango es un rango de puertos excluido. administrada: es una exclusión
// que puso alguien a mano (la fila con "*"): Windows no reparte esos puertos,
// pero un programa que los pide los puede usar.
type jsonRango struct {
	Inicio       uint16 `json:"inicio"`
	Fin          uint16 `json:"fin"`
	Administrada bool   `json:"administrada"`
}

// jsonDinamico es el rango de puertos que Windows reparte solo.
type jsonDinamico struct {
	Proto     string `json:"proto"`
	Familia   string `json:"familia"`
	Inicio    uint16 `json:"inicio"`
	Cantidad  int    `json:"cantidad"`
	DeFabrica bool   `json:"de_fabrica"`
}

type jsonVictima struct {
	PID     uint32 `json:"pid"`
	Proceso string `json:"proceso,omitempty"`
	Destino string `json:"destino"` // cerrado | terminado | ya_no_estaba | reciclado | denegado | protegido | critico | servicio_detenido | error
	Metodo  string `json:"metodo,omitempty"`
	MS      int64  `json:"ms,omitempty"` // cuánto tardó en cerrar
}

type jsonReporte struct {
	Puertos     []jsonPuerto   `json:"puertos"`
	Victimas    []jsonVictima  `json:"victimas"`
	Libres      int            `json:"libres"`
	Total       int            `json:"total"`
	Cancelado   bool           `json:"cancelado"`
	SoloListado bool           `json:"solo_listado"` // true si fue --json sin --yes (no se actuó)
	Objetivos   []jsonObjetivo `json:"objetivos"`    // qué se planeó cerrar (o se cerraría)
	Negativas   []jsonNegativa `json:"negativas"`    // qué no se toca y por qué
	Externos    []jsonExterno  `json:"externos"`     // cómo le fue a cada contenedor / proceso Linux / regla
	// manual (solo con --manual): cómo quedó el inicio de cada servicio.
	Manual []jsonManual `json:"manual,omitempty"`
	// al_margen: lo que escuchaba un puerto pedido y no se pidió;
	// compartidos: los puertos pedidos que escuchan varios a la vez.
	AlMargen    []jsonAlMargen   `json:"al_margen"`
	Compartidos []jsonCompartido `json:"compartidos,omitempty"`
	// sin_resolver: hosts con nombre que vinieron en el pedido (api.local:3000)
	// y no se resolvieron: esos puertos se tomaron enteros.
	SinResolver []string `json:"sin_resolver,omitempty"`
}

// jsonObjetivo es un objetivo del plan. tipo: proceso | fantasma | servicio |
// contenedor | linux | regla. Para contenedor, linux y regla, pid/proceso son
// los del intermediario de Windows (via) y el objeto de su tipo dice lo de
// atrás. Un proceso o servicio que está detrás de http.sys lleva via (System)
// y las urls que registró.
type jsonObjetivo struct {
	Tipo       string          `json:"tipo"`
	PID        uint32          `json:"pid,omitempty"`
	Proceso    string          `json:"proceso,omitempty"`
	Servicio   string          `json:"servicio,omitempty"`
	Puertos    []uint16        `json:"puertos"`
	Via        *jsonDueño      `json:"via,omitempty"`
	Contenedor *jsonContenedor `json:"contenedor,omitempty"`
	WSL        *jsonLinux      `json:"wsl,omitempty"`
	Regla      *jsonRegla      `json:"regla,omitempty"`
	URLs       []string        `json:"urls,omitempty"`
	Motivo     string          `json:"motivo,omitempty"`
	Cautela    string          `json:"cautela,omitempty"`
	// impostor: lleva el nombre de un proceso de Windows y no lo es (el motivo).
	Impostor string `json:"impostor,omitempty"`
	// inicio (servicios): cómo arranca, y si vuelve solo después de detenerlo.
	Inicio *jsonInicio `json:"inicio,omitempty"`
	// conocido: qué es, si está en la tabla de sospechosos de siempre.
	Conocido string `json:"conocido,omitempty"`
	// direcciones y alcance de sus sockets; proyecto, si tiene; huerfano:
	// quedó suelto (el motivo: "su padre (PID 900) ya no existe").
	Direcciones []string      `json:"direcciones"`
	Alcance     string        `json:"alcance,omitempty"`
	Proyecto    *jsonProyecto `json:"proyecto,omitempty"`
	Huerfano    string        `json:"huerfano,omitempty"`
	// desde: cuándo empezó a escuchar (el socket más viejo, RFC3339); comando
	// y ruta: la línea de comandos y el ejecutable, tal cual (como en el
	// listado).
	Desde   string `json:"desde,omitempty"`
	Comando string `json:"comando,omitempty"`
	Ruta    string `json:"ruta,omitempty"`
}

// jsonNegativa es un dueño que no se toca. intermediario: contenedores | wsl |
// vscode | http.sys | portproxy si lo es. via: el proceso negado está DETRÁS
// de ese intermediario (el que figura en la tabla es el intermediario).
type jsonNegativa struct {
	PID           uint32      `json:"pid"`
	Proceso       string      `json:"proceso,omitempty"`
	Puertos       []uint16    `json:"puertos"`
	Motivo        string      `json:"motivo,omitempty"`
	Consejo       string      `json:"consejo,omitempty"`
	Intermediario string      `json:"intermediario,omitempty"`
	Via           *jsonDueño  `json:"via,omitempty"`
	URLs          []string    `json:"urls,omitempty"`
	Servicio      string      `json:"servicio,omitempty"`
	Inicio        *jsonInicio `json:"inicio,omitempty"`
	Conocido      string      `json:"conocido,omitempty"`
	Impostor      string      `json:"impostor,omitempty"`
	Desde         string      `json:"desde,omitempty"`
	Comando       string      `json:"comando,omitempty"`
	Ruta          string      `json:"ruta,omitempty"`
}

// jsonManual es cómo le fue a un servicio con --manual. resultado: manual (se
// lo dejó en inicio manual) | ya_estaba | no_se_toca | sigue_corriendo |
// denegado | error.
type jsonManual struct {
	Servicio  string `json:"servicio"`
	Resultado string `json:"resultado"`
	Antes     string `json:"antes,omitempty"` // el tipo de inicio que tenía
	Motivo    string `json:"motivo,omitempty"`
	Deshacer  string `json:"deshacer,omitempty"` // el comando que lo devuelve a como estaba
	Aviso     string `json:"aviso,omitempty"`
}

func manualJSON(rs []resultadoManual) []jsonManual {
	var out []jsonManual
	for _, r := range rs {
		jm := jsonManual{Servicio: r.Servicio, Resultado: r.Fin.clave(), Motivo: r.Motivo, Deshacer: r.Deshacer, Aviso: r.Aviso}
		if r.Antes.Name != "" {
			jm.Antes = inicioClave(r.Antes)
		}
		out = append(out, jm)
	}
	return out
}

// jsonExterno es cómo le fue a un objetivo externo.
type jsonExterno struct {
	Tipo    string `json:"tipo"` // contenedor | linux | regla
	Nombre  string `json:"nombre"`
	Clave   string `json:"clave"` // id del contenedor, o distro/PID@arranque
	Destino string `json:"destino"`
	Metodo  string `json:"metodo,omitempty"`
	MS      int64  `json:"ms,omitempty"`
	Error   string `json:"error,omitempty"`
}

func tipoObjetivo(t reaper.Target) string {
	switch t.Kind {
	case reaper.KillPhantom:
		return "fantasma"
	case reaper.StopThirdSvc:
		return "servicio"
	case reaper.StopExternal:
		return tipoExterno(t.External.Kind)
	}
	return "proceso"
}

func intermediarioJSON(r reaper.Relay) string {
	switch r {
	case reaper.RelayContainer:
		return "contenedores"
	case reaper.RelayWSL:
		return "wsl"
	case reaper.RelayEditor:
		return "vscode"
	case reaper.RelayHTTP:
		return "http.sys"
	case reaper.RelayPortProxy:
		return "portproxy"
	}
	return ""
}

func listaPuertos(s ports.Set) []uint16 {
	out := []uint16{}
	for p := range s.All() {
		out = append(out, p)
	}
	return out
}

// estadoPuertoJSON / destinoJSON mapean los enums a claves estables (no a la
// etiqueta en español, que puede cambiar de redacción).
func estadoPuertoJSON(s reaper.PortStatus) string {
	switch s {
	case reaper.PortFree:
		return "libre"
	case reaper.PortPhantom:
		return "fantasma"
	case reaper.PortRespawned:
		return "respawn"
	case reaper.PortReserved:
		return "reservado"
	case reaper.PortBusy:
		return "ocupado"
	case reaper.PortRefused:
		return "no_se_toca"
	case reaper.PortAnswered:
		return "contesta_otro"
	}
	return "desconocido"
}

func destinoJSON(o reaper.Outcome) string {
	switch o {
	case reaper.OutcomeClosed:
		return "cerrado"
	case reaper.OutcomeTerminated:
		return "terminado"
	case reaper.OutcomeGone:
		return "ya_no_estaba"
	case reaper.OutcomeRecycled:
		return "reciclado"
	case reaper.OutcomeDenied:
		return "denegado"
	case reaper.OutcomeProtected:
		return "protegido"
	case reaper.OutcomeCritical:
		return "critico"
	case reaper.OutcomeServiceStopped:
		return "servicio_detenido"
	case reaper.OutcomeError:
		return "error"
	}
	return "pendiente"
}

func construirJSONReporte(rep reaper.Report, plan reaper.Plan, soloListado bool) jsonReporte {
	doc := jsonReporte{Cancelado: rep.Canceled, SoloListado: soloListado, Puertos: []jsonPuerto{}, Victimas: []jsonVictima{},
		Objetivos: []jsonObjetivo{}, Negativas: []jsonNegativa{}, Externos: []jsonExterno{}, AlMargen: []jsonAlMargen{}}
	for _, b := range plan.Bystanders {
		doc.AlMargen = append(doc.AlMargen, jsonAlMargen{PID: b.PID, Proceso: b.Name, Puertos: listaPuertos(b.Ports), Direcciones: direccionesJSON(b.Sockets)})
	}
	doc.Compartidos = compartidosJSON(compartidos(ocupantesDelPlan(plan)))
	for _, t := range plan.Targets {
		jo := jsonObjetivo{Tipo: tipoObjetivo(t), PID: t.Root.PID, Proceso: t.Name, Servicio: t.Service,
			Puertos: listaPuertos(t.Ports), Motivo: t.Reason, Cautela: t.Caution, Impostor: t.Impostor,
			Direcciones: direccionesJSON(t.Sockets), Alcance: alcanceDe(direccionesDe(t.Sockets), nil).clave, Desde: desdeJSON(t.Sockets)}
		switch {
		case t.Kind == reaper.StopExternal:
			jo.PID, jo.Proceso = 0, ""
			jo.Via = &jsonDueño{PID: t.Via.PID, Proceso: t.ViaName}
			if ex, ok := t.External.Ref.(externo); ok {
				ex.anotar(&jo) // el objeto de su dominio: "contenedor", "wsl", "regla"
			}
		case !t.Via.Zero():
			jo.Via = &jsonDueño{PID: t.Via.PID, Proceso: t.ViaName}
		}
		doc.Objetivos = append(doc.Objetivos, jo)
	}
	for _, r := range plan.Refusals {
		jn := jsonNegativa{PID: r.PID, Proceso: r.Name, Puertos: listaPuertos(r.Ports),
			Motivo: r.Reason, Consejo: r.Advice, Intermediario: intermediarioJSON(r.Relay),
			Servicio: r.Service, Impostor: r.Impostor, Desde: desdeJSON(r.Sockets)}
		if !r.Via.Zero() {
			jn.Via = &jsonDueño{PID: r.Via.PID, Proceso: r.ViaName}
		}
		doc.Negativas = append(doc.Negativas, jn)
	}
	for _, x := range rep.Externals {
		jx := jsonExterno{Tipo: tipoExterno(x.External.Kind), Nombre: x.External.Name, Clave: x.External.Key,
			Destino: destinoJSON(x.Outcome), Metodo: x.Method.String()}
		if x.Elapsed > 0 {
			jx.MS = x.Elapsed.Milliseconds()
		}
		if x.Err != nil {
			jx.Error = x.Err.Error()
		}
		doc.Externos = append(doc.Externos, jx)
	}
	free, total := rep.FreedPorts()
	doc.Libres, doc.Total = free, total
	for _, p := range rep.Ports {
		jp := jsonPuerto{Puerto: p.Port, Estado: estadoPuertoJSON(p.Status), Motivo: p.Reason, Consejo: p.Advice}
		if p.Proto != 0 {
			jp.Proto = p.Proto.String()
		}
		if p.Reserved != (ports.ExcludedRange{}) {
			jp.Reservado = &jsonRango{Inicio: p.Reserved.Lo, Fin: p.Reserved.Hi, Administrada: p.Reserved.Administered}
		}
		if d := p.Dynamic; d != (ports.DynamicRange{}) {
			jp.Dinamico = &jsonDinamico{Proto: d.Proto.String(), Familia: d.Family.String(), Inicio: d.Start, Cantidad: d.Count, DeFabrica: d.Default()}
		}
		jp.Remedios = remediosJSON(p.Remedies)
		if !p.Owner.Zero() {
			jp.Dueño = &jsonDueño{PID: p.Owner.PID, Proceso: p.OwnerName}
		}
		if !p.Supervisor.Zero() {
			jp.Supervisor = &jsonDueño{PID: p.Supervisor.PID, Proceso: p.SupervisorName}
		}
		jp.Arrancando = p.Starting
		if !p.Bystander.Zero() {
			jp.AlMargen = &jsonDueño{PID: p.Bystander.PID, Proceso: p.BystanderName}
		}
		doc.Puertos = append(doc.Puertos, jp)
	}
	for _, v := range rep.Victims {
		jv := jsonVictima{PID: v.ID.PID, Proceso: v.Name, Destino: destinoJSON(v.Outcome), Metodo: v.Method.String()}
		if v.Elapsed > 0 {
			jv.MS = v.Elapsed.Milliseconds()
		}
		doc.Victimas = append(doc.Victimas, jv)
	}
	return doc
}

// desdeJSON es desde cuándo escuchan unos sockets (el más viejo), o "".
func desdeJSON(socks []ports.Socket) string {
	var d time.Time
	for _, s := range socks {
		if !s.Since.IsZero() && (d.IsZero() || s.Since.Before(d)) {
			d = s.Since
		}
	}
	if d.IsZero() {
		return ""
	}
	return d.Format(formatoHora)
}

// formatoHora es cómo va una hora en el JSON: RFC 3339 con la zona de la
// máquina y hasta 100 ns (la resolución de las horas de Windows).
const formatoHora = "2006-01-02T15:04:05.9999999Z07:00"

// escribirJSON serializa indentado a la salida estándar. Devuelve OK salvo un
// fallo de codificación (que no debería pasar con estas estructuras).
func escribirJSON(t *tui.Term, v any) int {
	b, err := textoJSON(v)
	if err != nil {
		return fallo(t, err)
	}
	t.Print(string(b))
	return cli.ExitOK
}

// textoJSON es el documento indentado y terminado en un salto de línea. Sin
// el escapado para HTML que encoding/json aplica de fábrica: esto va a una
// consola o a un script, y los comandos que lleva adentro (un `<puerto>`, un
// `&`) se tienen que poder leer y copiar tal cual.
func textoJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
