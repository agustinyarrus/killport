package reaper

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Huérfanos.
//
// El dolor diario más votado de Windows: el servidor de desarrollo que quedó
// escuchando después de cerrar la terminal, el editor o el npm que lo lanzó.
// La receta de los foros es `taskkill /f /im node.exe`, que se lleva puestos
// también los language servers, el CLI que está corriendo en otra pestaña y
// cualquier otro node. Lo que distingue al que quedó suelto es que su PADRE ya
// no está: un servidor que alguien sigue cuidando (nodemon, npm, la terminal)
// tiene padre vivo.
//
// Un dueño de un socket que escucha es HUÉRFANO si:
//
//	(a) es raíz del bosque: su padre murió, o el PID de su padre lo usa ahora
//	    otro proceso más nuevo (la regla de NewForest: un padre no puede haber
//	    nacido después que su hijo);
//	(b) corre en la sesión de killport (lo de otra sesión es de otro usuario o
//	    de un servicio);
//	(c) no es de Windows ni de la maquinaria: no es un servicio, ni una imagen
//	    de %SystemRoot%, ni crítico, ni un intermediario (Docker, WSL, VS
//	    Code), ni killport o su terminal.
//
// Y además, para ofrecerlo sin que el usuario lo pida con --system:
//
//	(d) parece de desarrollo: una imagen de las que corren servidores (node,
//	    python, dotnet, java…), un binario de `go run` (vive en go-build, en
//	    los temporales), o su carpeta es la de un proyecto (eso lo sabe la
//	    herramienta, que lee la carpeta del proceso: dev).
//
// Lo que cumple (a)–(c) y no (d) es casi siempre una app de la bandeja que
// quedó sin padre cuando se reinició el Explorador: se muestra solo si se
// pide, marcado "no parece de desarrollo".
//
// Deliberadamente NO cuenta como huérfano el que tiene padre vivo aunque ese
// padre sea "raro" (una tarea programada cuelga de un svchost, un helper de su
// app): un supervisor vivo es exactamente lo que se quiere respetar.

// Orphan es un dueño de un socket que escucha y quedó suelto.
type Orphan struct {
	Proc
	Why string // por qué está suelto, para mostrar tal cual
	Dev bool   // parece de desarrollo (ver (d))
}

// devImages son las imágenes que suelen correr servidores de desarrollo, en
// minúsculas.
var devImages = map[string]bool{
	"node.exe": true, "bun.exe": true, "deno.exe": true,
	"python.exe": true, "pythonw.exe": true, "python3.exe": true, "py.exe": true, "uvicorn.exe": true,
	"dotnet.exe": true, "java.exe": true, "javaw.exe": true,
	"ruby.exe": true, "php.exe": true, "php-cgi.exe": true,
	"esbuild.exe": true,
}

// DevImage dice si la imagen es de las que corren servidores de desarrollo, o
// un binario que dejó `go run` (se compila en una carpeta go-build de los
// temporales).
func DevImage(p Proc) bool {
	if devImages[strings.ToLower(strings.TrimSpace(p.Name))] {
		return true
	}
	path := strings.ToLower(filepath.ToSlash(p.Path))
	return strings.Contains(path, "/go-build")
}

// Orphans busca los huérfanos entre los dueños de los sockets que escuchan
// (TCP en LISTEN, UDP atado a un puerto elegido: no los clientes efímeros).
// dev, si no es nil, dice si la carpeta del proceso es la de un proyecto (el
// criterio (d) que el núcleo no puede ver). Pura; en el orden del primer
// puerto de cada uno. O(s + n) más la profundidad de la cadena de killport.
func Orphans(f *Forest, socks []ports.Socket, svc map[uint32][]Service, cfg Config, dev func(Proc) bool) []Orphan {
	w := newWorld(f, svc, cfg)
	session, knownSession := uint32(0), false
	if self, ok := f.Lookup(cfg.Self); ok {
		session, knownSession = self.SessionID, true
	}
	first := map[ID]uint16{}
	var owners []Proc
	for _, s := range socks {
		if !(s.Proto == ports.TCP && s.State == ports.Listen) && !(s.Proto == ports.UDP && !s.Ephemeral) {
			continue
		}
		// Quién sostiene el socket. Si el dueño murió, lo sostienen sus
		// herederos, que no tienen filas propias en la tabla (el socket
		// figura a nombre del muerto): se los juzga como si fueran el dueño.
		// Es el caso típico del huérfano, además: el recargador de uvicorn o
		// de werkzeug que quedó con el socket de su padre muerto.
		h := f.Hold(s.PID, s.Since)
		holders := h.Heirs
		if h.Live {
			holders = []Proc{h.Owner}
		}
		for _, p := range holders {
			if old, seen := first[p.ID]; !seen || s.Port() < old {
				if !seen {
					owners = append(owners, p)
				}
				first[p.ID] = s.Port()
			}
		}
	}
	var out []Orphan
	for _, p := range owners {
		why, loose := w.loose(p)
		switch {
		case !loose:
		case knownSession && p.SessionID != session, !knownSession && p.SessionID == 0:
		case !w.ordinaryOwner(p):
		default:
			out = append(out, Orphan{Proc: p, Why: why, Dev: DevImage(p) || (dev != nil && dev(p))})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return first[out[i].ID] < first[out[j].ID] })
	return out
}

// loose dice si p es raíz del bosque, y por qué: su padre ya no existe, o su
// PID lo tiene ahora un proceso que nació después.
func (w *world) loose(p Proc) (string, bool) {
	if _, ok := w.f.Parent(p.ID); ok {
		return "", false
	}
	if p.Parent == 0 || p.Parent == p.PID {
		return "no tiene padre", true
	}
	if now, ok := w.f.LivePID(p.Parent); ok {
		return "su padre (PID " + itoa(p.Parent) + ") ya no existe: ese PID ahora es " + displayName(now) + ", que nació después", true
	}
	return "su padre (PID " + itoa(p.Parent) + ") ya no existe", true
}

// ordinaryOwner: no es de Windows ni de la maquinaria (el criterio (c)).
func (w *world) ordinaryOwner(p Proc) bool {
	_, isSvc := w.svc[p.PID]
	switch {
	case p.PID <= kernelPID, isSvc, w.untouchable(p):
		return false
	case p.Path != "" && w.cfg.SystemRoot != "" && UnderDir(p.Path, w.cfg.SystemRoot) && windowsImage(p, w.cfg.SystemRoot):
		return false // de Windows (no lo que alguien puso en C:\Windows\Temp ni un script de PowerShell)
	}
	return true
}

// OrphanOf dice si UN proceso quedó suelto, con el mismo juicio que Orphans
// (sin el criterio de desarrollo, que decide quién lo ofrece): para contar
// en el bloque de un objetivo que su padre ya no existe. f es el bosque de
// donde salió p.
func OrphanOf(f *Forest, p Proc, svc map[uint32][]Service, cfg Config) (Orphan, bool) {
	w := newWorld(f, svc, cfg)
	why, loose := w.loose(p)
	if !loose || !w.ordinaryOwner(p) {
		return Orphan{}, false
	}
	if self, ok := f.Lookup(cfg.Self); ok && self.SessionID != p.SessionID {
		return Orphan{}, false
	}
	return Orphan{Proc: p, Why: why, Dev: DevImage(p)}, true
}
