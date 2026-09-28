//go:build windows

package killport

// wsl.go: mirar DENTRO de WSL a través de wslrelay. Cuando la tabla dice que
// wslrelay.exe escucha el 3000, el que escucha de verdad es un proceso Linux
// en alguna distro: acá se lo encuentra (sin arrancar ninguna distro), se lo
// muestra con su identidad y se lo cierra con la escalera de Linux (SIGINT,
// SIGTERM, SIGKILL), verificando después del lado de Windows que el relay
// soltó el puerto. Si adentro no escucha nadie, el relay quedó colgado (el
// bug de WSL#10601) y se dice.
//
// Toda llamada a wsl.exe pasa por el ejecutor acotado (win.RunBounded): sin
// ventana, con plazo y dentro de un Job que no deja nada colgado. wsl.exe
// tarda de 200 a 500 ms por llamada, así que se lo consulta solo si un relay
// de WSL tiene un puerto pedido, o con --wsl; y las distros se sondean en
// paralelo.
//
// Nunca arranca una distro: solo se nombran las que `wsl -l --running`
// informa corriendo. wsl.exe no tiene un "ejecutá solo si ya corre", así que
// queda una ventana entre listar y ejecutar (~ lo que tarda wsl.exe); una
// distro con un proceso vivo no se detiene sola, y antes de cerrar se vuelve
// a listar (el usuario pudo tardar en confirmar y la distro apagarse).

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

const (
	wslListTimeout  = 10 * time.Second
	wslProbeTimeout = 20 * time.Second
	wslMaxOutput    = 4 << 20
)

// wsl es el cliente de WSL de una corrida (memoiza lo que vio).
type wsl struct {
	run func(ctx context.Context, args []string, stdin []byte, timeout time.Duration) (out []byte, code uint32, err error)

	once     sync.Once
	errLista error    // no se pudo saber qué distros corren
	distros  []string // las que corrían al mirar
	sondeos  []sondeo // lo visto en cada una (las que contestaron)
	fallas   map[string]error
}

// wslExe es wsl.exe por ruta absoluta (el de System32 reenvía al de la
// Tienda si está). Solo el binario de las pruebas de punta a punta
// (etiqueta killport_e2e, ver wsl_e2e.go) lo puede reemplazar por un doble.
var wslExe = win.SystemExe("wsl.exe")

func nuevoWSL() *wsl {
	exe := wslExe
	return &wsl{run: func(ctx context.Context, args []string, stdin []byte, timeout time.Duration) ([]byte, uint32, error) {
		// WSL_UTF8=1: los mensajes propios de wsl.exe (la lista de distros)
		// en UTF-8; la salida de los comandos Linux pasa tal cual.
		res, err := win.RunBounded(ctx, exe, args, win.Bounded{Timeout: timeout, MaxOutput: wslMaxOutput, Env: []string{"WSL_UTF8=1"}, Stdin: stdin})
		return res.Stdout, res.ExitCode, err
	}}
}

// corriendo pregunta qué distros están corriendo AHORA.
func (w *wsl) corriendo(ctx context.Context) ([]string, error) {
	out, code, err := w.run(ctx, []string{"-l", "--running", "-q"}, nil, wslListTimeout)
	switch {
	case errors.Is(err, context.Canceled):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("no pude preguntarle a WSL: %w", err)
	case code != 0:
		// Con -q y sin distros, wsl.exe sale 0 y no escribe nada; distinto de
		// cero es que WSL no está instalado o no responde.
		msg := strings.TrimSpace(decodificarWSL(out))
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		return nil, fmt.Errorf("WSL no respondió (código %d): %s", int32(code), msg)
	}
	return distrosCorriendo(out), nil
}

// execRoot corre un script en una distro, como root, por la entrada estándar.
func (w *wsl) execRoot(ctx context.Context, distro, script string, timeout time.Duration) (string, error) {
	out, code, err := w.run(ctx, []string{"-d", distro, "-u", "root", "-e", "/bin/sh", "-s"}, []byte(script), timeout)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return string(out), fmt.Errorf("%w (código %d)", ErrSondeo, int32(code))
	}
	return string(out), nil
}

// sondear mira una distro: una ida con ss, dos con el respaldo /proc.
func (w *wsl) sondear(ctx context.Context, distro string) (sondeo, error) {
	out, err := w.execRoot(ctx, distro, scriptSondeo, wslProbeTimeout)
	if err != nil {
		return sondeo{Distro: distro}, err
	}
	sd, faltan, err := parseSondeo(distro, out)
	if err != nil || len(faltan) == 0 {
		return sd, err
	}
	out, err = w.execRoot(ctx, distro, scriptProcesos(faltan), wslProbeTimeout)
	if err != nil {
		return sd, err
	}
	return completarSondeo(sd, out)
}

// consultar lista las distros corriendo y sondea cada una en paralelo, una
// sola vez por corrida.
func (w *wsl) consultar(ctx context.Context) {
	w.once.Do(func() {
		w.fallas = map[string]error{}
		w.distros, w.errLista = w.corriendo(ctx)
		if w.errLista != nil {
			return
		}
		res := make([]sondeo, len(w.distros))
		errs := make([]error, len(w.distros))
		var wg sync.WaitGroup
		for i, d := range w.distros {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res[i], errs[i] = w.sondear(ctx, d)
			}()
		}
		wg.Wait()
		for i, d := range w.distros {
			if errs[i] != nil {
				w.fallas[d] = errs[i]
				continue
			}
			w.sondeos = append(w.sondeos, res[i])
		}
	})
}

// miradaWSL es el resumen de una mirada dentro de WSL: qué distros corrían y
// cómo le fue a la consulta. Es lo que --wsl le informa al usuario (y al JSON)
// además de las filas.
type miradaWSL struct {
	Distros []string          `json:"distros"`          // las que corrían al mirar
	Error   string            `json:"error,omitempty"`  // no se pudo preguntar qué distros corren
	Fallas  map[string]string `json:"fallas,omitempty"` // distro → por qué no se pudo mirar adentro
}

// mirada resume la consulta ya hecha (consultar). O(distros).
func (w *wsl) mirada() *miradaWSL {
	m := &miradaWSL{Distros: append([]string{}, w.distros...)}
	if w.errLista != nil {
		m.Error = w.errLista.Error()
	}
	for d, err := range w.fallas {
		if m.Fallas == nil {
			m.Fallas = map[string]string{}
		}
		m.Fallas[d] = err.Error()
	}
	return m
}

// escuchaEn dice si en el espacio de red de WSL hay un socket reenviable en
// port/proto, y quiénes (de todas las distros) son sus dueños.
func (w *wsl) escuchaEn(port uint16, proto ports.Proto) (hay bool, duenos []procesoLinux) {
	seen := map[string]bool{}
	for _, sd := range w.sondeos {
		for _, s := range sd.Sockets {
			if s.Port() == port && s.Proto == proto && linuxForwarded(s) {
				hay = true
			}
		}
		for _, p := range sd.Procesos {
			if seen[p.clave()] {
				continue
			}
			if slices.ContainsFunc(p.Sockets, func(s ports.LinuxSocket) bool {
				return s.Port() == port && s.Proto == proto && linuxForwarded(s)
			}) {
				seen[p.clave()] = true
				duenos = append(duenos, p)
			}
		}
	}
	return hay, duenos
}

// desenmascarar contesta, para cada puerto que sostiene un relay de WSL, qué
// proceso Linux lo escucha (Found), que no escucha nadie (Empty: relay
// colgado, cerrable si no reenvía nada vivo) o que no se pudo mirar. Si el
// dueño es un reenvío de contenedores dentro de la distro (docker-proxy) o la
// distro de Docker Desktop, se deriva al motor de contenedores.
func (w *wsl) desenmascarar(ctx context.Context, masked []reaper.MaskedPort, socks []ports.Socket, forest *reaper.Forest, dk *docker, noDocker bool) []reaper.Unmasking {
	w.consultar(ctx)
	out := make([]reaper.Unmasking, 0, len(masked))
	for _, m := range masked {
		u := reaper.Unmasking{Via: m.Via, Port: m.Port, Proto: m.Proto}
		if w.errLista != nil {
			u.Verdict = reaper.UnmaskUnavailable
			u.Reason = w.errLista.Error()
			u.Advice = w.consejoApagar()
			out = append(out, u)
			continue
		}
		hay, duenos := w.escuchaEn(m.Port, m.Proto)
		switch {
		case len(duenos) > 0:
			u = w.encontrado(ctx, u, duenos, forest, dk, noDocker)
		case hay:
			// Hay un socket pero ninguna distro corriendo dice de quién es:
			// otro espacio de PID (la distro de Docker Desktop, un contenedor).
			u = w.sinDueno(ctx, u, forest, dk, noDocker)
		case len(w.fallas) > 0:
			u.Verdict = reaper.UnmaskUnavailable
			u.Reason = "no pude mirar dentro de " + strings.Join(clavesOrdenadas(w.fallas), ", ") + ": " + primerError(w.fallas).Error()
			u.Advice = w.consejoApagar()
		default:
			u = w.zombi(u, m, socks)
		}
		out = append(out, u)
	}
	return out
}

// encontrado arma el Found con los procesos Linux dueños (o los deriva al
// motor de contenedores si son su reenvío).
func (w *wsl) encontrado(ctx context.Context, u reaper.Unmasking, duenos []procesoLinux, forest *reaper.Forest, dk *docker, noDocker bool) reaper.Unmasking {
	// El PID 1 de una distro (systemd, o el init de WSL) no se toca nunca, y
	// si figura entre los dueños no se toca a ninguno: con la activación por
	// sockets (el ssh.socket de Ubuntu) systemd tiene el socket y le pasa una
	// copia al servicio que lanza; cerrar el servicio no libera el puerto, y
	// un SIGINT a systemd arranca ctrl-alt-del.target: reinicia la distro.
	for _, p := range duenos {
		if p.PID == 1 {
			u.Verdict = reaper.UnmaskUnavailable
			u.Reason, u.Advice = w.delInit(p, u.Port)
			return u
		}
	}
	for _, p := range duenos {
		if p.esDelMotor() {
			if c := w.contenedores(ctx, u, forest, dk, noDocker); c.Verdict == reaper.UnmaskFound {
				return c
			}
			u.Verdict = reaper.UnmaskUnavailable
			u.Reason = "lo publica un contenedor dentro de WSL (" + p.Comm + " en " + p.Distro + "): " + p.Comm + " es su reenvío"
			u.Advice = "`wsl -d " + p.Distro + " docker ps` muestra qué contenedor publica el " + strconv.Itoa(int(u.Port)) + "; detenelo con `docker stop`"
			return u
		}
	}
	u.Verdict = reaper.UnmaskFound
	for _, p := range duenos {
		cp := p
		u.Behind = append(u.Behind, reaper.External{
			Kind: reaper.ExtLinux, Where: p.Distro, Key: p.clave(), Name: p.Comm,
			Ports: p.puertos(), Ref: &cp,
		})
	}
	return u
}

// delInit explica un puerto que tiene el PID 1 de una distro, y cómo se
// libera.
func (w *wsl) delInit(p procesoLinux, port uint16) (reason, advice string) {
	if p.Comm == "systemd" {
		return "lo tiene systemd (el PID 1 de " + p.Distro + ") por activación de sockets: escucha él y lanza el servicio cuando llega una conexión; cerrar el servicio no libera el puerto",
			"`wsl -d " + p.Distro + " -u root systemctl list-sockets` dice qué unidad escucha el " + strconv.Itoa(int(port)) +
				", y `wsl -d " + p.Distro + " -u root systemctl stop UNIDAD.socket` la detiene"
	}
	return "lo tiene " + p.Comm + ", el PID 1 de " + p.Distro + ": es el proceso que sostiene la distro", w.consejoApagar()
}

// sinDueno: hay un socket en WSL que ninguna distro atribuye. Si corre la
// distro de Docker Desktop, se le pregunta al motor.
func (w *wsl) sinDueno(ctx context.Context, u reaper.Unmasking, forest *reaper.Forest, dk *docker, noDocker bool) reaper.Unmasking {
	if w.motorCorriendo() != "" {
		if c := w.contenedores(ctx, u, forest, dk, noDocker); c.Verdict == reaper.UnmaskFound {
			return c
		}
	}
	u.Verdict = reaper.UnmaskUnavailable
	u.Reason = "algo escucha el " + strconv.Itoa(int(u.Port)) + " dentro de WSL, pero ninguna distro corriendo dice qué proceso es"
	u.Advice = w.consejoApagar()
	return u
}

// motorCorriendo es la distro de un motor de contenedores que está corriendo
// ("" si ninguna): mientras corra, la VM de WSL también es del motor.
func (w *wsl) motorCorriendo() string {
	for _, d := range w.distros {
		if distroDeMotor(d) {
			return d
		}
	}
	return ""
}

// consejoApagar es el último recurso, con lo que cuesta: `wsl --shutdown`
// apaga la VM entera, y con ella todo lo que corre en WSL —también el motor
// de contenedores, si usa esa VM—. Si no se pudo saber qué distros corren, no
// se puede descartar que Docker Desktop esté entre ellas: se avisa igual.
func (w *wsl) consejoApagar() string {
	if m := w.motorCorriendo(); m != "" {
		return "`wsl --shutdown` corta todos los reenvíos, pero apaga TODO lo que corre en WSL: también el motor de contenedores (" + m + ") y sus contenedores"
	}
	if w.errLista != nil {
		return "`wsl --shutdown` corta todos los reenvíos, pero apaga TODO lo que corre en WSL: también Docker Desktop o Podman, si los usás"
	}
	return "`wsl --shutdown` corta todos los reenvíos (y cierra todo lo que corre en WSL)"
}

// contenedores deriva un puerto de WSL al motor de contenedores.
func (w *wsl) contenedores(ctx context.Context, u reaper.Unmasking, forest *reaper.Forest, dk *docker, noDocker bool) reaper.Unmasking {
	if dk == nil || noDocker {
		return u
	}
	res := dk.desenmascarar(ctx, []reaper.MaskedPort{{Via: u.Via, Port: u.Port, Proto: u.Proto}}, forest)
	if len(res) == 1 && res[0].Verdict == reaper.UnmaskFound {
		return res[0]
	}
	return u
}

// zombi: nadie escucha adentro. El relay es cerrable solo si no reenvía
// NADA vivo (hay uno por VM: cerrarlo corta todos sus reenvíos).
func (w *wsl) zombi(u reaper.Unmasking, m reaper.MaskedPort, socks []ports.Socket) reaper.Unmasking {
	u.Verdict = reaper.UnmaskEmpty
	u.Reason = "relay colgado: " + m.Name + " escucha el " + strconv.Itoa(int(m.Port)) + ", pero en ninguna distro de WSL escucha nadie"
	var vivos []uint16
	for _, s := range socks {
		if s.PID != m.Via.PID || !(s.Proto == ports.UDP || s.State == ports.Listen) || s.Port() == m.Port {
			continue
		}
		if hay, _ := w.escuchaEn(s.Port(), s.Proto); hay && !slices.Contains(vivos, s.Port()) {
			vivos = append(vivos, s.Port())
		}
	}
	// El par partido (repartirWSL): en el mismo puerto el relay tiene también
	// el socket de la publicación de un contenedor, que está trabajando.
	publica := false
	for _, s := range socks {
		if s.PID == m.Via.PID && s.Proto == m.Proto && s.Port() == m.Port && m.Sockets != nil &&
			!slices.ContainsFunc(m.Sockets, func(x ports.Socket) bool { return x.Local == s.Local }) {
			publica = true
		}
	}
	switch {
	case len(vivos) > 0:
		slices.Sort(vivos)
		u.Advice = "no lo cierro: el mismo relay reenvía también " + ports.Of(vivos...).String() + ", que están vivos; " + w.consejoApagar()
	case publica:
		u.Advice = "no lo cierro: en ese mismo puerto el relay reenvía también la publicación de un contenedor; " + w.consejoApagar()
	case w.motorCorriendo() != "":
		// El motor de contenedores publica por este mismo relay (el [::1] de
		// cada puerto): cerrarlo le cortaría los reenvíos a contenedores que
		// todavía no publicaron nada. Un relay del motor no se toca.
		u.Advice = "no lo cierro: el motor de contenedores (" + w.motorCorriendo() + ") usa ese mismo relay; reiniciá el motor, o " + w.consejoApagar()
	default:
		u.KillVia = true
		u.Advice = "cerrarlo corta todos los reenvíos de esa VM de WSL (WSL los rearma al volver a arrancar); `wsl --shutdown` también lo arregla"
	}
	return u
}

// detener cierra un proceso Linux con la escalera, verificando la identidad
// adentro. ok=false si se canceló (el motor lo deja pendiente).
func (w *wsl) detener(ctx context.Context, p *procesoLinux, deadline time.Time, force bool) (reaper.ExternalStop, bool) {
	// Antes de ejecutar nada en la distro: ¿sigue corriendo? Si se apagó (el
	// usuario tardó en confirmar), el proceso ya no existe, y nombrarla la
	// arrancaría.
	distros, err := w.corriendo(ctx)
	if ctx.Err() != nil {
		return reaper.ExternalStop{}, false
	}
	if err != nil {
		return reaper.ExternalStop{Outcome: reaper.OutcomeError, Err: err}, true
	}
	if !slices.Contains(distros, p.Distro) {
		return reaper.ExternalStop{Outcome: reaper.OutcomeGone}, true
	}
	timeout := max(time.Until(deadline), 0)
	out, err := w.execRoot(ctx, p.Distro, scriptCierre(*p, timeout, force), plazoCierre(timeout))
	if ctx.Err() != nil {
		return reaper.ExternalStop{}, false
	}
	if err != nil && out == "" {
		return reaper.ExternalStop{Outcome: reaper.OutcomeError, Err: err}, true
	}
	fin, perr := resultadoCierre(out)
	if perr != nil {
		if err == nil {
			err = perr
		}
		return reaper.ExternalStop{Outcome: reaper.OutcomeError, Err: err}, true
	}
	return veredictoCierre(fin), true
}

// --- el proceso Linux como objetivo externo (ver externo.go) -------------------

// detener: la escalera de señales dentro de su distro.
func (p *procesoLinux) detener(ctx context.Context, s *sistema, deadline time.Time, force bool) (reaper.ExternalStop, bool) {
	return s.wsl.detener(ctx, p, deadline, force)
}

// presentar: nombre, PID y distro en el título (y desde cuándo corre); su
// línea de comandos y su usuario, que son los de ADENTRO de la distro.
func (p *procesoLinux) presentar(t *tui.Term, _ reaper.Target, now time.Time) (string, []string) {
	titulo := t.Paint(tui.Text, p.Comm) + t.Paint(tui.Faint, "  PID ") + t.Paint(tui.Subtle, u32(p.PID)) +
		t.Paint(tui.Faint, " en ") + t.Paint(tui.Subtle, p.Distro)
	if !p.Desde.IsZero() {
		titulo += t.Paint(tui.Faint, " · ") + t.Paint(tui.Subtle, tui.Ago(p.Desde, now))
	}
	var lineas []string
	if p.Cmdline != "" {
		lineas = append(lineas, t.Paint(tui.Faint, comandoCorto(p.Cmdline, t.Width()-8)))
	}
	if p.Usuario != "" {
		lineas = append(lineas, t.Paint(tui.Faint, "usuario: "+p.Usuario+" (dentro de WSL)"))
	}
	return titulo, lineas
}

// anotar: el objeto "wsl" del JSON.
func (p *procesoLinux) anotar(o *jsonObjetivo) { o.WSL = linuxJSON(p) }

// veredictoCierre traduce la marca del script al veredicto del motor.
func veredictoCierre(fin string) reaper.ExternalStop {
	switch fin {
	case "GONE":
		return reaper.ExternalStop{Outcome: reaper.OutcomeGone}
	case "INT":
		return reaper.ExternalStop{Outcome: reaper.OutcomeClosed, Method: reaper.MethodSigInt}
	case "TERM":
		return reaper.ExternalStop{Outcome: reaper.OutcomeClosed, Method: reaper.MethodSigTerm}
	case "KILL":
		return reaper.ExternalStop{Outcome: reaper.OutcomeTerminated, Method: reaper.MethodTerminate}
	case "DENIED":
		return reaper.ExternalStop{Outcome: reaper.OutcomeDenied, Err: errors.New("no pude mandarle la señal (¿la distro no deja usar root?)")}
	}
	return reaper.ExternalStop{Outcome: reaper.OutcomeError, Err: errors.New("no murió ni con SIGKILL (¿está trabado en el kernel?)")}
}

func clavesOrdenadas(m map[string]error) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func primerError(m map[string]error) error {
	return m[clavesOrdenadas(m)[0]]
}
