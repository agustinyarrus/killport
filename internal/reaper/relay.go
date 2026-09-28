package reaper

import (
	"strings"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Relay es la clase de un INTERMEDIARIO: un proceso de Windows que sostiene
// sockets en nombre de otro. La tabla dice "com.docker.backend.exe escucha el
// 5432", y es verdad, pero el dueño real es el contenedor db-1; dice
// "wslrelay.exe escucha el 3000" cuando el que escucha es un node dentro de
// Ubuntu. Matar al intermediario es el error clásico de un killport ingenuo:
// tira Docker Desktop entero (o todos los reenvíos de WSL) y ni siquiera
// resuelve nada, porque el contenedor sigue definido y vuelve a publicar.
//
// Regla: un intermediario NUNCA es víctima, ni como dueño ni como parte del
// subárbol de otro objetivo (--tree). Es una negativa que la herramienta puede
// DESENMASCARAR (ver Unmask): mirar detrás y convertirla en el objetivo real,
// que no es un proceso de Windows (un contenedor, un proceso Linux…).
//
// El reconocimiento es por nombre de imagen y a propósito NO exige una ruta
// de instalación conocida: Docker Desktop, Podman y WSL se instalan en lugares
// distintos según la versión (Program Files, la carpeta del usuario, System32
// en el WSL de la caja). La asimetría manda: no reconocer un relay verdadero
// tira el motor del usuario; reconocer de más a un programa que se llama igual
// solo hace que killport no lo cierre, y el bloque lo muestra con su ruta.
type Relay uint8

const (
	RelayNone      Relay = iota
	RelayContainer       // motor de contenedores: Docker Desktop, Podman
	RelayWSL             // reenvío de localhost de WSL (wslrelay, wslhost)
	RelayEditor          // el reenvío de puertos de VS Code (o una extensión que escucha adentro)
	// RelayHTTP es http.sys, el servidor HTTP del kernel: el socket figura a
	// nombre de System (PID 4) y atiende por cuenta del proceso que registró la
	// URL (IIS, SSRS, un HttpListener de .NET…).
	RelayHTTP
	// RelayPortProxy es el servicio IP Helper (iphlpsvc, dentro de un svchost)
	// escuchando por una regla de `netsh interface portproxy`.
	RelayPortProxy
)

// relayImages son las imágenes de los intermediarios conocidos, en minúsculas.
// Docker Desktop publica los puertos de los contenedores desde
// com.docker.backend.exe (y en versiones anteriores com.docker.proxy.exe o
// vpnkit.exe); Podman, desde gvproxy.exe (máquinas Hyper-V) o win-sshproxy.exe
// (el túnel de su API); WSL, desde wslrelay.exe (uno por VM) y wslhost.exe.
// VS Code escucha en el proceso principal cuando reenvía un puerto de una
// sesión remota (WSL, SSH, contenedor) y en el host de extensiones cuando una
// extensión levanta un servidor: matarlo cierra el editor entero.
var relayImages = map[string]Relay{
	"com.docker.backend.exe": RelayContainer,
	"com.docker.proxy.exe":   RelayContainer,
	"vpnkit.exe":             RelayContainer,
	"gvproxy.exe":            RelayContainer,
	"win-sshproxy.exe":       RelayContainer,
	"wslrelay.exe":           RelayWSL,
	"wslhost.exe":            RelayWSL,
	"code.exe":               RelayEditor,
	"code - insiders.exe":    RelayEditor,
}

// RelayOf clasifica un proceso POR SU IMAGEN: qué clase de intermediario es,
// o RelayNone. Los dos intermediarios de Windows mismo (RelayHTTP y
// RelayPortProxy) no salen de acá: no se reconocen por el nombre del proceso
// —son el kernel y un svchost— sino por QUÉ sostienen, y eso lo decide la
// planificación al armar la negativa (refuseKernel, refuseSvchost).
func RelayOf(p Proc) Relay {
	return relayImages[strings.ToLower(strings.TrimSpace(p.Name))]
}

// RelayFor clasifica UN socket con su dueño: qué clase de intermediario lo
// sostiene, o RelayNone. Es la misma vara que usa la planificación al negar
// (RelayOf para los que se reconocen por imagen; para System y el IP Helper,
// lo que sostienen), puesta a disposición del listado general, que tiene que
// decidir fila por fila si vale la pena mirar detrás. f es el bosque de donde
// salió owner.
func RelayFor(owner Proc, s ports.Socket, f *Forest, svcByPID map[uint32][]Service, cfg Config) Relay {
	if r := RelayOf(owner); r != RelayNone {
		return r
	}
	w := &world{f: f, svc: svcByPID, cfg: cfg}
	if w.f == nil {
		w.f = NewForest(nil)
	}
	switch {
	case owner.PID == kernelPID:
		return kernelRelay(s.Proto, s.Port())
	case owner.PID < kernelPID:
		return RelayNone
	case w.svchost(owner):
		return svchostRelay(s, svcByPID[owner.PID])
	}
	return RelayNone
}

// Unmaskable dice si se puede mirar DETRÁS de esta clase de intermediario:
// es lo que Plan.Masked le ofrece a la herramienta y lo único que Unmask
// acepta reescribir. VS Code no expone a quién le reenvía, así que su negativa
// es definitiva. Este es el único lugar que decide qué clases participan del
// protocolo: una clase nueva que se pueda desenmascarar se suma acá.
func (r Relay) Unmaskable() bool {
	switch r {
	case RelayContainer, RelayWSL, RelayHTTP, RelayPortProxy:
		return true
	}
	return false
}

// String nombra la clase para mostrar ("el motor de contenedores").
func (r Relay) String() string {
	switch r {
	case RelayContainer:
		return "el motor de contenedores"
	case RelayWSL:
		return "el reenvío de WSL"
	case RelayEditor:
		return "VS Code"
	case RelayHTTP:
		return "http.sys, el servidor HTTP de Windows"
	case RelayPortProxy:
		return "el reenvío de puertos de Windows (portproxy)"
	}
	return ""
}

// relayRefusal arma la negativa de un intermediario TODAVÍA NO desenmascarado:
// lo que se dice si la herramienta no pudo (o no quiso: --no-docker) mirar
// detrás. Unmask la reemplaza con lo que se vio.
func relayRefusal(base Refusal, r Relay) Refusal {
	base.Relay = r
	switch r {
	case RelayContainer:
		base.Reason = "lo sirve " + displayName(Proc{ID: base.Owner, Name: base.Name}) + ", el motor de contenedores (Docker Desktop/Podman)"
		base.Advice = "el puerto lo publica un contenedor: `docker ps` muestra cuál y `docker stop NOMBRE` lo libera"
	case RelayWSL:
		base.Reason = "es el reenvío de WSL (" + displayName(Proc{ID: base.Owner, Name: base.Name}) + ")"
		base.Advice = "lo que escucha está dentro de una distro de WSL"
	case RelayEditor:
		base.Reason = "es VS Code: un puerto reenviado de una sesión remota o una extensión que escucha adentro; matarlo cierra el editor"
		base.Advice = "cerralo desde el panel Puertos de VS Code (o detené la extensión que lo abrió)"
	}
	return base
}

// untouchable dice si un proceso no puede ser víctima por NINGÚN camino: ni
// como dueño (eso lo explica refuseOwner) ni arrastrado por el subárbol de
// otro objetivo con --tree. Sin esta poda, `killport 3000 --tree` sobre un
// proceso que desciende de la terminal… o del que desciende un relay, se
// llevaría puesto lo que la planificación prometió no tocar.
//
// Un posible impostor NO es intocable por llamarse como un proceso de
// Windows (ver standing.go): solo lo protege lo que de verdad es.
func (w *world) untouchable(p Proc) bool {
	return p.PID <= kernelPID || w.critical(p) || w.svchost(p) || w.selfChain[p.ID] || RelayOf(p) != RelayNone
}
