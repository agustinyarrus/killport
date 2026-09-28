//go:build windows

package killport

// sistema.go es el ADAPTADOR: implementa reaper.OS (las capacidades que el
// motor puro necesita) sobre internal/win. El núcleo decide; acá se toca
// el sistema. Cada método traduce, además, los errores crudos de win a los
// centinelas que el motor compara con errors.Is.
//
// Dos capacidades no se pueden atender en proceso y salen por un AYUDANTE que
// killport lanza de sí mismo con DETACHED_PROCESS (sin consola propia):
// consultar y usar la consola de OTRO proceso. killport corre en la terminal
// del usuario; si él mismo hiciera AttachConsole perdería su propia salida
// (un proceso solo puede estar atado a UNA consola). El ayudante, sin consola,
// se engancha a la del objetivo, hace lo suyo y muere. Ver ayudante.go.

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/win"
)

// sistema es el reaper.OS real. exe es la ruta de killport (para relanzar el
// ayudante de consola). Cada dominio que se consulta vive en su propio
// cliente, que memoiza lo que vio y no pregunta si no hace falta.
type sistema struct {
	exe    string
	admin  func() bool // si killport corre elevado (las pruebas lo fijan)
	red    *red        // lo que netsh sabe de la red: rangos excluidos y dinámicos (red.go)
	docker *docker     // el motor de contenedores (Docker Desktop, Podman) por su pipe
	wsl    *wsl        // las distros de WSL que corren, por wsl.exe
	http   *httpsys    // las colas de http.sys: quién registró cada URL (httpsys.go)
	proxy  *portproxy  // las reglas de `netsh interface portproxy` (portproxy.go)
	svc    *servicios  // cómo está configurado cada servicio (servicios.go)
	// Lo que se muestra de cada dueño, averiguado una vez por corrida: el
	// detalle (línea de comandos, carpeta), su proyecto y los nombres de las
	// interfaces de red.
	detalles  map[reaper.ID]detalleMemo
	proyectos *detector
	ifaces    map[netip.Addr]string
	ifacesOK  bool
	// Los dobles de las pruebas. nil = lo de verdad; las pruebas lo fijan:
	// ninguna prueba detiene WinNAT, corre un netsh que cambie algo, ni depende
	// de qué escucha en la máquina. Cada uno se resuelve en UN lugar, el método
	// que lo usa (Sockets, AllSockets, Listening, ProbeBind, Services,
	// estadoWinnat): los llamadores no saben si hay doble.
	winnat func() (existe, corriendo bool, err error)        // el estado del controlador de WinNAT
	config configurador                                      // quien ejecuta los pasos
	tablas func(ctx context.Context) ([]ports.Socket, error) // las tablas de sockets (con y sin dueños)
	// escuchando reemplaza la tabla liviana (Listening) en las pruebas.
	escuchando func(ctx context.Context) ([]ports.Socket, error)
	sondeo     func(ports.Proto, ports.Family, uint16) ports.BindResult // el sondeo de bind
	// listaServicios reemplaza la enumeración de servicios (win.Services).
	listaServicios func() ([]win.Service, error)
	// consolaDe reemplaza al ayudante que lee quiénes comparten una consola
	// (ConsolePIDs).
	consolaDe func(pid uint32) (miembros []uint32, hay bool, err error)
}

// nuevoSistema arma el adaptador. Falla solo si no se puede saber cuál es el
// propio ejecutable (sin eso no se puede lanzar el ayudante de consola).
func nuevoSistema() (*sistema, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	netsh := correrNetsh
	return &sistema{exe: exe, admin: win.IsElevated, red: nuevaRed(netsh), docker: nuevoDocker(), wsl: nuevoWSL(),
		http: nuevoHTTPSys(netsh), proxy: nuevoPortproxy(netsh), svc: nuevosServicios(), proyectos: nuevoDetector()}, nil
}

// estadoWinnat dice si el controlador de WinNAT existe y si corre.
func (s *sistema) estadoWinnat() (existe, corriendo bool, err error) {
	if s.winnat != nil {
		return s.winnat()
	}
	return estadoWinnat()
}

// esAdmin dice si killport corre con el token de administrador.
func (s *sistema) esAdmin() bool {
	if s.admin == nil {
		return win.IsElevated()
	}
	return s.admin()
}

func (s *sistema) Now() time.Time { return time.Now() }

// Sleep espera d o hasta que se cancele ctx (el único retardo del motor, el
// backoff de la verificación). Sin polling: un timer y el canal de ctx.
func (s *sistema) Sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Sockets lee las cuatro tablas y arma ports.Socket. El módulo/servicio dueño
// (Owner, otra llamada al sistema por socket) se pide SOLO para los que ocupan
// un puerto —TCP en LISTEN y UDP— que son los únicos donde importa (identificar
// el servicio dentro de un svchost) y un puñado frente a los cientos de
// conexiones efímeras. Un error al pedir el dueño no aborta la lectura: queda
// el socket sin Service.
func (s *sistema) Sockets(ctx context.Context) ([]ports.Socket, error) {
	if s.tablas != nil {
		return s.tablas(ctx)
	}
	raw, err := win.AllSockets()
	if err != nil {
		return nil, err
	}
	return socketsDe(raw, true), nil
}

// AllSockets es la tabla completa SIN preguntar por el módulo dueño de cada
// socket (una llamada al sistema por socket: 7 ms contra 0,5 ms en esta
// máquina). Es lo que alcanza cuando no hace falta saber qué servicio de un
// svchost tiene el puerto: buscar un puerto libre, esperar con --all-states.
func (s *sistema) AllSockets(ctx context.Context) ([]ports.Socket, error) {
	if s.tablas != nil {
		return s.tablas(ctx)
	}
	raw, err := win.AllSockets()
	if err != nil {
		return nil, err
	}
	return socketsDe(raw, false), nil
}

// Listening es la tabla LIVIANA: lo que escucha (TCP en LISTEN y UDP), con la
// hora de creación de cada socket y sin dueños. Es la que se relee al esperar
// que un puerto escuche o se libere (reaper.WaitPorts).
func (s *sistema) Listening(ctx context.Context) ([]ports.Socket, error) {
	if s.escuchando != nil {
		return s.escuchando(ctx)
	}
	raw, err := win.ListeningSockets()
	if err != nil {
		return nil, err
	}
	return socketsDe(raw, false), nil
}

// socketsDe traduce las filas de win a ports.Socket. conDueño pide, además, el
// módulo dueño de lo que ocupa un puerto (TCP en LISTEN y UDP): en un svchost
// es el nombre del servicio. Un error al pedirlo no aborta: queda sin Service.
func socketsDe(raw []win.Socket, conDueño bool) []ports.Socket {
	out := make([]ports.Socket, 0, len(raw))
	for _, r := range raw {
		ps := ports.Socket{
			Proto:     protoDe(r.Proto),
			Local:     r.Local,
			Remote:    r.Remote,
			State:     ports.State(r.State),
			PID:       r.PID,
			Since:     r.Created,
			Ephemeral: r.Proto == win.UDP && !r.SpecificPortBind,
		}
		if conDueño && (r.State == win.TCPListen || r.Proto == win.UDP) {
			if m, err := r.Owner(); err == nil {
				ps.Service = m.Name
			}
		}
		out = append(out, ps)
	}
	return out
}

// Snapshot toma la foto de todos los procesos sin abrir ninguno y le agrega la
// ruta del ejecutable (ImagePath, con la clase 88 del kernel: no abre el
// proceso, así que anda con los protegidos). La ruta es lo que la planificación
// necesita para saber si una imagen es "de Windows" o un servicio de terceros.
// Critical no se puede saber para todos sin abrirlos, así que queda en false y
// la clasificación se apoya en la lista de nombres críticos conocidos (el
// detalle por víctima lo refina en Detail).
func (s *sistema) Snapshot(ctx context.Context) ([]reaper.Proc, error) {
	raw, err := win.AllProcesses()
	if err != nil {
		return nil, err
	}
	out := make([]reaper.Proc, len(raw))
	for i, p := range raw {
		path, _ := win.ImagePath(p.PID)
		out[i] = reaper.Proc{
			ID:        reaper.ID{PID: p.PID, Created: p.Created},
			Parent:    p.PPID,
			Name:      p.Image,
			Path:      path,
			SessionID: p.SessionID,
		}
	}
	return out, nil
}

// Detail trae los datos caros de un proceso, best-effort, verificando la
// identidad (no vaya a mostrar la línea de comandos de un PID reciclado).
func (s *sistema) Detail(id reaper.ID) (reaper.Detail, error) {
	if id.PID <= 4 {
		return reaper.Detail{}, nil // el kernel: no hay línea de comandos ni carpeta que leer
	}
	d, err := win.QueryDetailWith(id.PID, id.Created, conCarpeta(win.WindowsDirectory()))
	if err != nil {
		if errors.Is(err, win.ErrGone) || errors.Is(err, win.ErrRecycled) {
			return reaper.Detail{}, reaper.ErrGone
		}
		return reaper.Detail{}, err
	}
	return reaper.Detail{
		CommandLine:   d.CommandLine,
		User:          d.User,
		Critical:      d.Critical,
		CriticalKnown: d.CriticalOK,
		Cwd:           d.Cwd,
	}, nil
}

// conCarpeta decide si vale la pena leer la carpeta de trabajo de un proceso
// (pedir PROCESS_VM_READ y leer su memoria: ver win.QueryDetailWith). Solo
// sirve para decir de qué proyecto es algo del usuario: nunca para un
// proceso crítico ni para una imagen de Windows (lsass, wininit, services,
// svchost… viven en System32), ni si no se pudo leer su ruta. Sí para lo que
// corre desde una carpeta de Windows donde escribe cualquiera o en un host de
// scripts (un servidor de Windows PowerShell tiene proyecto).
func conCarpeta(root string) func(path string, critico bool) bool {
	return func(path string, critico bool) bool {
		if critico || path == "" {
			return false
		}
		return !reaper.WindowsImage(reaper.Proc{Name: filepath.Base(path), Path: path}, root)
	}
}

// detalleMemo es un Detail ya pedido (con su error).
type detalleMemo struct {
	d   reaper.Detail
	err error
}

// detalle es Detail recordado: lo que se muestra de un mismo dueño (su línea
// de comandos, su carpeta, su proyecto) sale de UNA consulta por corrida.
func (s *sistema) detalle(id reaper.ID) (reaper.Detail, error) {
	id = id.Canon()
	if m, ok := s.detalles[id]; ok {
		return m.d, m.err
	}
	d, err := s.Detail(id)
	if s.detalles == nil {
		s.detalles = map[reaper.ID]detalleMemo{}
	}
	s.detalles[id] = detalleMemo{d, err}
	return d, err
}

// proyectoDe es el proyecto de un proceso (su carpeta de trabajo, o la de su
// script), si se puede saber.
func (s *sistema) proyectoDe(id reaper.ID) (proyecto, bool) {
	if s.proyectos == nil || id.PID <= 4 {
		return proyecto{}, false
	}
	d, err := s.detalle(id)
	if err != nil {
		return proyecto{}, false
	}
	return s.proyectos.delProceso(d.Cwd, d.CommandLine)
}

// interfaz nombra la interfaz de red de una dirección ("Wi-Fi"), o "" si no
// se sabe. Las interfaces se leen una vez, la primera vez que hacen falta.
func (s *sistema) interfaz(a netip.Addr) string {
	if !s.ifacesOK {
		s.ifacesOK, s.ifaces = true, map[netip.Addr]string{}
		if ifs, err := win.Interfaces(); err == nil {
			for _, it := range ifs {
				for _, x := range it.Addrs {
					s.ifaces[x.WithZone("")] = it.Name
				}
			}
		}
	}
	return s.ifaces[a.Unmap().WithZone("")]
}

// Services mapea PID → servicios activos que corren en ese proceso.
func (s *sistema) Services() (map[uint32][]reaper.Service, error) {
	lista := win.Services
	if s.listaServicios != nil {
		lista = s.listaServicios
	}
	svcs, err := lista()
	if err != nil {
		return nil, err
	}
	m := map[uint32][]reaper.Service{}
	for _, sv := range svcs {
		if sv.PID == 0 {
			continue
		}
		m[sv.PID] = append(m[sv.PID], reaper.Service{Name: sv.Name, Display: sv.DisplayName, Shared: sv.Shared})
	}
	return m, nil
}

// Excluded y DynamicRanges salen de una misma lectura de netsh (ver red.go).
// Las exclusiones van con lo que se sabe de http.sys en las que pueden ser
// suyas (exclusiones_windows.go): el diagnóstico dice de quién es cada una.
func (s *sistema) Excluded() (ports.Exclusions, error) {
	return s.exclusiones()
}

func (s *sistema) DynamicRanges() (ports.DynamicRanges, error) {
	l := s.red.leer()
	return l.dyn, l.errDyn
}

// Open abre un handle con identidad y distingue el proceso protegido (PPL) del
// simplemente inaccesible: al primero no lo mata ni un administrador, así que
// ofrecer elevación sería en vano.
//
// Antes que nada mira si el proceso está MARCADO crítico. Es la red que queda
// debajo de la regla de impostores: la planificación ya no protege a nadie
// por llamarse lsass.exe, así que lo que de verdad tumbaría la máquina se
// reconoce por la marca que le puso el kernel, no por el nombre. Un proceso
// crítico no llega a tener handle: no recibe ni el Ctrl+C.
func (s *sistema) Open(id reaper.ID) (reaper.Handle, error) {
	h, err := win.OpenForKill(id.PID, id.Created)
	if err != nil {
		return nil, traducirOpen(err)
	}
	a := abierto{puedeTerminar: h.CanTerminate}
	a.critico, a.seSupoCritico = h.Critical()
	if !a.puedeTerminar {
		a.protegido = h.Protected()
	}
	if err := a.negativa(); err != nil {
		h.Close()
		return nil, err
	}
	// Guarda la identidad CANÓNICA que pidió el motor: el win.Handle trae la
	// hora de creación en zona local (FILETIME→time.Unix), y el motor usa las
	// identidades como clave de mapa comparando con ==. Devolver la forma
	// canónica (la misma que salió del bosque) es lo que hace que WaitAny
	// reencuentre a la víctima en su tabla; reconstruirla del handle la
	// convertiría en otra clave y la espera nunca terminaría.
	return &mango{h: h, id: id.Canon()}, nil
}

// OpenWait abre un proceso SOLO para esperar su salida (sin permiso de
// terminarlo), con identidad: es lo que usa --wait-free para dormir hasta que
// el dueño de un puerto termine.
func (s *sistema) OpenWait(id reaper.ID) (reaper.Handle, error) {
	h, err := win.OpenForWait(id.PID, id.Created)
	if err != nil {
		return nil, traducirOpen(err)
	}
	return &mango{h: h, id: id.Canon()}, nil
}

// Wait espera sin polling la primera salida de hs, hasta deadline o cancelación.
func (s *sistema) Wait(ctx context.Context, hs []reaper.Handle, deadline time.Time) (reaper.Handle, reaper.ExitInfo, reaper.WaitOutcome) {
	raw := make([]*win.Handle, len(hs))
	for i, h := range hs {
		raw[i] = h.(*mango).h
	}
	i, res, _ := win.WaitAny(ctx, raw, deadline)
	switch res {
	case win.WaitExited:
		h := hs[i].(*mango).h
		code, _ := h.ExitCode()
		at, _ := h.ExitTime()
		return hs[i], reaper.ExitInfo{Code: code, At: at}, reaper.WaitExited
	case win.WaitCanceled:
		return nil, reaper.ExitInfo{}, reaper.WaitCanceled
	default:
		return nil, reaper.ExitInfo{}, reaper.WaitDeadline
	}
}

func (s *sistema) CloseWindows(id reaper.ID) (int, error) {
	return win.CloseWindows(id.PID)
}

// ConsolePIDs pregunta, por el ayudante, qué procesos comparten la consola del
// objetivo. ErrNoConsole si no tiene (proceso de ventanas, de servicio, o ya
// cerró la suya).
func (s *sistema) ConsolePIDs(id reaper.ID) ([]uint32, error) {
	pedir := s.pedirConsola
	if s.consolaDe != nil {
		pedir = s.consolaDe
	}
	pids, hay, err := pedir(id.PID)
	if err != nil {
		return nil, err
	}
	if !hay {
		return nil, reaper.ErrNoConsole
	}
	return pids, nil
}

// CtrlC manda, por el ayudante, UN Ctrl+C a la consola del objetivo, solo si
// todos sus miembros están en allow (por identidad). El ayudante hace la
// comprobación pegada al envío; acá solo se traduce su veredicto.
func (s *sistema) CtrlC(id reaper.ID, allow []reaper.ID) error {
	return s.mandarCtrlC(id.PID, allow)
}

// StopService detiene un servicio y espera STOPPED, traduciendo los errores.
func (s *sistema) StopService(ctx context.Context, name string, deadline time.Time) error {
	err := win.StopService(ctx, name, deadline)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, win.ErrAccessDenied):
		return reaper.ErrServiceAccessDenied
	case errors.Is(err, win.ErrServiceNotFound):
		return reaper.ErrGone
	case errors.Is(err, win.ErrServiceTimeout):
		return reaper.ErrServiceTimeout
	}
	var dep *win.DependentsError
	if errors.As(err, &dep) {
		return &reaper.ServiceDependentsError{Names: dep.Names}
	}
	return err
}

// ProbeBind sondea un bind sin listen para distinguir libre de reservado. Si
// las pruebas fijaron un sondeo (sondeo), es ese: ninguna prueba depende de
// qué puertos tiene tomados la máquina.
func (s *sistema) ProbeBind(proto ports.Proto, fam ports.Family, port uint16) ports.BindResult {
	if s.sondeo != nil {
		return s.sondeo(proto, fam, port)
	}
	st, err := win.ProbeBind(port, proto == ports.TCP, fam == ports.IPv6)
	if err != nil {
		return ports.BindError
	}
	switch st {
	case win.BindInUse:
		return ports.BindInUse
	case win.BindForbidden:
		return ports.BindForbidden
	default:
		return ports.BindFree
	}
}

// StopExternal detiene los objetivos que no son procesos de Windows, todos a
// la vez (una goroutine por externo: un docker stop con t=3 no hace esperar
// al cierre de un proceso Linux), y le pasa cada veredicto al motor desde
// ESTA goroutine, apenas llega. Cada uno se detiene con la operación de su
// dominio (ver externo.go): acá no se sabe qué clase de cosa es. Si el
// usuario cancela, vuelve al instante: lo que estaba en vuelo termina solo
// (las peticiones y los wsl.exe llevan el mismo contexto y el ejecutor
// acotado mata lo que quede).
func (s *sistema) StopExternal(ctx context.Context, xs []reaper.External, deadline time.Time, force bool, done func(int, reaper.ExternalStop)) {
	type fin struct {
		i  int
		r  reaper.ExternalStop
		ok bool
	}
	ch := make(chan fin, len(xs)) // con lugar para todos: nadie queda trabado al mandar si ya no se lo espera
	for i, x := range xs {
		go func() {
			r, ok := detenerExterno(ctx, s, x, deadline, force)
			ch <- fin{i, r, ok}
		}()
	}
	for range xs {
		select {
		case f := <-ch:
			if f.ok {
				done(f.i, f.r)
			}
		case <-ctx.Done():
			return
		}
	}
}

// abierto es lo que se sabe de un proceso recién abierto, antes de
// entregárselo al motor.
type abierto struct {
	critico, seSupoCritico bool // la marca ProcessBreakOnTermination, y si se la pudo leer
	puedeTerminar          bool // el handle tiene PROCESS_TERMINATE
	protegido              bool // es un proceso protegido (PPL)
}

// negativa dice por qué ese proceso no se le entrega al motor, o nil. Primero
// la marca de crítico, que manda sobre todo lo demás: a un proceso marcado no
// se le manda ni un Ctrl+C, tenga el nombre y los permisos que tenga. Si la
// marca no se pudo leer no se inventa: el proceso sigue su camino (lo que de
// verdad es de Windows ya lo negó la planificación).
func (a abierto) negativa() error {
	switch {
	case a.seSupoCritico && a.critico:
		return reaper.ErrCritical
	case !a.puedeTerminar && a.protegido:
		return reaper.ErrProtected
	}
	return nil
}

// mango envuelve un *win.Handle como reaper.Handle.
type mango struct {
	h  *win.Handle
	id reaper.ID // identidad canónica con la que se abrió (la clave del motor)
}

func (m *mango) ID() reaper.ID      { return m.id }
func (m *mango) CanTerminate() bool { return m.h.CanTerminate }
func (m *mango) Close()             { m.h.Close() }
func (m *mango) Terminate() error {
	// Código de salida 1: la convención de un proceso terminado desde afuera.
	if err := m.h.Terminate(1); err != nil {
		if errors.Is(err, win.ErrGone) {
			return reaper.ErrGone
		}
		if errors.Is(err, win.ErrAccessDenied) {
			return reaper.ErrAccessDenied
		}
		return err
	}
	return nil
}

// traducirOpen mapea los errores de OpenForKill a los centinelas del motor.
func traducirOpen(err error) error {
	switch {
	case errors.Is(err, win.ErrGone):
		return reaper.ErrGone
	case errors.Is(err, win.ErrRecycled):
		return reaper.ErrRecycled
	case errors.Is(err, win.ErrAccessDenied):
		return reaper.ErrAccessDenied
	}
	return err
}

func protoDe(p win.Proto) ports.Proto {
	if p == win.UDP {
		return ports.UDP
	}
	return ports.TCP
}
