package killport

// wsl_parse.go: lo PURO de mirar dentro de WSL —qué distros corren, qué
// escucha adentro y quién, y cómo se cierra un proceso Linux—, sin tocar el
// sistema. El lado que corre wsl.exe está en wsl.go.
//
// Una sola ida por distro. wsl.exe tarda de 200 a 500 ms por llamada aunque
// la VM ya esté arriba, así que todo lo que hace falta sale de UN script (que
// viaja por la entrada estándar, no por la línea de comandos: wsl.exe parte
// los argumentos con reglas propias y un script con comillas llegaría roto):
// los sockets que escuchan (ss, o /proc/net si no hay iproute2) y, para cada
// proceso dueño, su identidad (PID + hora de arranque), su nombre, su
// usuario y su línea de comandos. La salida es un protocolo de líneas con
// marcas "@…" que no pueden aparecer en los datos (ninguna fila de ss, de
// /proc/net ni de ls empieza con "@", y lo demás va prefijado).
//
// Trampa de WSL2: todas las distros de la VM comparten el MISMO espacio de
// red, pero cada una tiene su espacio de PID. Así que `ss` en Ubuntu ve el
// socket de un node que corre en Debian, pero sin dueño (no ve su /proc). El
// dueño de un socket es la distro que SÍ lo resuelve a un proceso.

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/agustinyarrus/killport/internal/ports"
)

// procesoLinux es un proceso de una distro de WSL, con identidad.
type procesoLinux struct {
	Distro   string
	PID      uint32
	Arranque uint64 // starttime: ticks desde el arranque del kernel (campo 22 de /proc/PID/stat)
	Comm     string
	Cmdline  string
	UID      uint32
	Usuario  string
	Desde    time.Time // cero si no se pudo calcular
	Sockets  []ports.LinuxSocket
	// inodos son los de los sockets que tiene abiertos (sus fd): con eso se
	// confirma que sostiene un socket, sin creerle a lo que imprime ss.
	inodos []uint64
}

// clave es su identidad en el dominio: "distro/PID@arranque". Un PID Linux se
// recicla igual que uno de Windows; con la hora de arranque no.
func (p procesoLinux) clave() string {
	return p.Distro + "/" + strconv.FormatUint(uint64(p.PID), 10) + "@" + strconv.FormatUint(p.Arranque, 10)
}

// puertos son los puertos en los que escucha.
func (p procesoLinux) puertos() ports.Set {
	ps := make([]uint16, len(p.Sockets))
	for i, s := range p.Sockets {
		ps[i] = s.Port()
	}
	return ports.Of(ps...)
}

// sondeo es lo que se vio dentro de una distro.
type sondeo struct {
	Distro   string
	Fuente   string              // "ss" o "/proc"
	Sockets  []ports.LinuxSocket // TODO lo que escucha en el espacio de red (compartido entre distros)
	Procesos []procesoLinux      // los dueños que ESTA distro resuelve, con sus sockets
	inodes   map[uint64][]uint32 // respaldo /proc: inodo → PID, para la segunda ida
}

// distrosCorriendo lee `wsl.exe -l --running -q`: una distro por línea, vacío
// si no corre ninguna. wsl.exe escribe UTF-16LE (salvo con WSL_UTF8=1, que se
// pide igual): se detecta y se decodifica. Un nombre raro (vacío, con control
// o que empieza con '-', que wsl.exe tomaría como opción) se descarta.
func distrosCorriendo(out []byte) []string {
	var names []string
	for _, line := range strings.Split(decodificarWSL(out), "\n") {
		name := strings.TrimSpace(strings.TrimPrefix(line, string(rune(0xFEFF))))
		if name == "" || strings.HasPrefix(name, "-") || strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r == 0x7f }) {
			continue
		}
		names = append(names, name)
	}
	return names
}

// decodificarWSL devuelve la salida de wsl.exe como texto. wsl.exe escribe
// UTF-16LE (con BOM o sin él) salvo cuando WSL_UTF8=1 le pide UTF-8. El texto
// en UTF-8 no tiene bytes cero; el ASCII en UTF-16LE tiene uno en cada
// posición impar: si al menos la mitad de las impares son cero, es UTF-16LE.
func decodificarWSL(b []byte) string {
	bom := len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE
	zeros := 0
	for i := 1; i < len(b); i += 2 {
		if b[i] == 0 {
			zeros++
		}
	}
	if !bom && (zeros == 0 || zeros*2 < len(b)/2) {
		return strings.ReplaceAll(string(b), "\r", "")
	}
	if bom {
		b = b[2:]
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return strings.ReplaceAll(string(utf16.Decode(u)), "\r", "")
}

// Los scripts corren dentro de cada distro como root (sin root, ss no puede
// nombrar los procesos de otros usuarios), leídos por `sh -s` desde la
// entrada estándar. POSIX sh puro: se prueban de verdad con dash (el /bin/sh
// de Debian y Ubuntu). Regla: ningún comando del script lee la entrada
// estándar (todos tienen archivo o tubería), porque es de donde sh está
// leyendo el script mismo. Terminan solos; no dejan nada.

// scriptEncabezado fija un entorno predecible: C para que ss y awk no
// traduzcan nada, y las carpetas del sistema primero.
const scriptEncabezado = "export LC_ALL=C PATH=/usr/sbin:/usr/bin:/sbin:/bin:$PATH\necho '@navaja 1'\n"

// scriptDescribir describe los procesos de $pids: identidad (stat), usuario,
// línea de comandos y los inodos de los sockets que tiene abiertos (F: con
// eso se confirma quién sostiene cada socket, ver atribuir), más el reloj del
// kernel para fecharlos.
//
// Los datos van con `printf '%s\n'` y nunca con echo: el echo de dash (el
// /bin/sh de Ubuntu y Debian) interpreta las barras invertidas, y un "\c" en
// una línea de comandos (`node C:\code\app.js`) corta la salida sin salto de
// línea: la marca @pid del proceso siguiente quedaba pegada a la línea C, y
// sus datos pisaban los del primero (o se perdía la distro entera si el de la
// barra era el último, con @fin pegado).
const scriptDescribir = `echo "@clock $(awk '/^btime/{print $2}' /proc/stat 2>/dev/null) $(getconf CLK_TCK 2>/dev/null || echo 100)"
for p in $pids; do
  s=$(tr '\n' ' ' < /proc/$p/stat 2>/dev/null) || continue
  u=$(awk '/^Uid:/{print $2}' /proc/$p/status 2>/dev/null)
  n=$(awk -F: -v u="$u" '$3==u{print $1; exit}' /etc/passwd 2>/dev/null)
  echo "@pid $p"
  printf 'S %s\n' "$s"
  printf 'U %s %s\n' "$u" "$n"
  printf 'C %s\n' "$(tr '\0\n' '  ' < /proc/$p/cmdline 2>/dev/null)"
  printf 'F %s\n' "$(ls -l /proc/$p/fd 2>/dev/null | grep -o 'socket:\[[0-9]*\]' | tr -dc '0-9\n' | tr '\n' ' ')"
done
echo '@fin'
`

// scriptSondeo es la primera (y casi siempre única) ida: con ss, los sockets
// que escuchan y los procesos que ss nombra, las tablas de /proc/net (con el
// inodo de cada socket, que da el kernel y no se puede falsificar) y la
// descripción de esos procesos con los inodos que tienen abiertos.
// Sin ss (Alpine y otras mínimas), las tablas crudas de /proc/net y el mapa
// inodo → PID de `ls -l /proc/*/fd/`; los dueños se conocen recién al unir
// las dos cosas, así que ahí hay una segunda ida (scriptProcesos).
const scriptSondeo = scriptEncabezado + `if command -v ss >/dev/null 2>&1 && o=$(ss -Hltnup 2>/dev/null || ss -ltnup 2>/dev/null); then
  echo '@ss'
  printf '%s\n' "$o"
  for f in tcp tcp6 udp udp6; do echo "@net $f"; cat /proc/net/$f 2>/dev/null; done
  pids=$(printf '%s\n' "$o" | grep -o 'pid=[0-9]*' | cut -c5- | sort -un)
else
  echo '@proc'
  for f in tcp tcp6 udp udp6; do echo "@net $f"; cat /proc/net/$f 2>/dev/null; done
  echo '@fd'
  ls -l /proc/[0-9]*/fd/ 2>/dev/null | grep -e '^/proc/' -e 'socket:\['
  pids=
fi
` + scriptDescribir

// scriptProcesos es la segunda ida del respaldo /proc: describe los PID dados
// (enteros; no hay texto de afuera en el script).
func scriptProcesos(pids []uint32) string {
	var b strings.Builder
	b.WriteString(scriptEncabezado + "pids='")
	for i, p := range pids {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.FormatUint(uint64(p), 10))
	}
	b.WriteString("'\n" + scriptDescribir)
	return b.String()
}

// ErrSondeo: la salida del script no tiene la forma esperada.
var ErrSondeo = errors.New("la distro devolvió algo inesperado")

// salida es la salida de un script, separada en secciones.
type salida struct {
	fuente string              // "ss", "/proc" o "" (solo procesos: la segunda ida)
	ss     string              // la salida de ss tal cual
	nets   map[string]string   // "tcp", "tcp6", "udp", "udp6" → /proc/net/…
	inodes map[uint64][]uint32 // inodo de socket → PID que lo tienen abierto (respaldo /proc)
	procs  []procesoLinux      // los procesos descriptos, con identidad y fecha
}

// leerSalida separa la salida de un script en sus secciones. Lo que wsl.exe
// escriba antes de la marca de inicio (avisos propios) se ignora; si la marca
// no aparece, o falta la de fin (se cortó), es error.
func leerSalida(distro, out string) (salida, error) {
	s := salida{nets: map[string]string{}, inodes: map[uint64][]uint32{}}
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.TrimSpace(l) == "@navaja 1" })
	if start < 0 {
		return s, fmt.Errorf("%w: falta la marca de inicio (¿la distro no tiene /bin/sh?)", ErrSondeo)
	}
	var (
		section string
		ssText  strings.Builder
		net     strings.Builder
		netName string
		fdPID   uint32
		cur     = -1 // índice del proceso que se está leyendo (no un puntero: append mueve el arreglo)
		btime   int64
		hz      int64 = 100
		fin     bool
	)
	cerrarNet := func() {
		if netName != "" {
			s.nets[netName] = net.String()
			net.Reset()
			netName = ""
		}
	}
	for _, line := range lines[start+1:] {
		if strings.HasPrefix(line, "@") {
			cerrarNet()
			f := strings.Fields(line)
			switch f[0] {
			case "@ss":
				section, s.fuente = "ss", "ss"
			case "@proc":
				section, s.fuente = "", "/proc"
			case "@net":
				section = "net"
				if len(f) > 1 {
					netName = f[1]
				}
			case "@fd":
				section = "fd"
			case "@clock":
				section = ""
				if len(f) > 1 {
					btime, _ = strconv.ParseInt(f[1], 10, 64)
				}
				if len(f) > 2 {
					if v, err := strconv.ParseInt(f[2], 10, 64); err == nil && v > 0 {
						hz = v
					}
				}
			case "@pid":
				section = "pid"
				s.procs = append(s.procs, procesoLinux{Distro: distro})
				cur = len(s.procs) - 1
				if len(f) > 1 {
					v, _ := strconv.ParseUint(f[1], 10, 32)
					s.procs[cur].PID = uint32(v)
				}
			case "@fin":
				fin = true
				section = ""
			}
			continue
		}
		switch section {
		case "ss":
			ssText.WriteString(line + "\n")
		case "net":
			net.WriteString(line + "\n")
		case "fd":
			// "/proc/812/fd:" abre la lista de un PID; las líneas de ls -l que
			// siguen terminan en "-> socket:[34567]".
			if rest, ok := strings.CutPrefix(line, "/proc/"); ok {
				v, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(rest), ":"), "/fd"), 10, 32)
				fdPID = uint32(v)
				continue
			}
			if i := strings.Index(line, "socket:["); i >= 0 && fdPID != 0 {
				num := strings.TrimSuffix(strings.TrimSpace(line[i+len("socket:["):]), "]")
				if ino, err := strconv.ParseUint(num, 10, 64); err == nil {
					s.inodes[ino] = appendUnique(s.inodes[ino], fdPID)
				}
			}
		case "pid":
			if cur < 0 {
				continue
			}
			p := &s.procs[cur]
			switch {
			case strings.HasPrefix(line, "S "):
				if comm, start, ok := parseStat(line[2:]); ok {
					p.Comm, p.Arranque = comm, start
				}
			case strings.HasPrefix(line, "U "):
				f := strings.Fields(line[2:])
				if len(f) > 0 {
					v, _ := strconv.ParseUint(f[0], 10, 32)
					p.UID = uint32(v)
				}
				if len(f) > 1 {
					p.Usuario = f[1]
				}
			case strings.HasPrefix(line, "C "):
				p.Cmdline = strings.TrimSpace(line[2:])
			case strings.HasPrefix(line, "F "), line == "F":
				for _, f := range strings.Fields(strings.TrimPrefix(line, "F")) {
					if ino, err := strconv.ParseUint(f, 10, 64); err == nil && ino != 0 {
						p.inodos = append(p.inodos, ino)
					}
				}
			}
		}
	}
	cerrarNet()
	if !fin {
		return s, fmt.Errorf("%w: la salida se cortó antes de terminar", ErrSondeo)
	}
	s.ss = ssText.String()
	// La fecha de arranque: btime (segundos desde 1970 en que arrancó el
	// kernel) + starttime / CLK_TCK. Sin btime, queda sin fecha.
	for i := range s.procs {
		if p := &s.procs[i]; btime > 0 && p.Arranque > 0 {
			p.Desde = time.Unix(btime, 0).Add(time.Duration(p.Arranque) * time.Second / time.Duration(hz))
		}
	}
	return s, nil
}

// sockets lee los sockets que escuchan: de ss (con dueños) o de /proc/net
// (sin dueños: se unen por inodo con atribuir).
func (s salida) sockets() ([]ports.LinuxSocket, error) {
	switch s.fuente {
	case "ss":
		return ports.ParseSS(s.ss)
	case "/proc":
		out, _, err := s.tablasNet()
		return out, err
	}
	return nil, fmt.Errorf("%w: no dice de dónde leyó los sockets", ErrSondeo)
}

// tablasNet lee las tablas de /proc/net que vinieron en la salida. hay dice
// si vino alguna: en Linux están siempre (aunque vacías, con su cabecera); no
// vienen en el /proc de MSYS con el que se prueban los scripts.
func (s salida) tablasNet() (out []ports.LinuxSocket, hay bool, err error) {
	for _, t := range []struct {
		name  string
		proto ports.Proto
		v6    bool
	}{{"tcp", ports.TCP, false}, {"tcp6", ports.TCP, true}, {"udp", ports.UDP, false}, {"udp6", ports.UDP, true}} {
		text := s.nets[t.name]
		if strings.TrimSpace(text) == "" {
			continue // la tabla no existe (IPv6 apagado, por ejemplo)
		}
		hay = true
		socks, err := ports.ParseProcNet(text, t.proto, t.v6)
		if err != nil {
			return nil, hay, err
		}
		out = append(out, socks...)
	}
	return out, hay, nil
}

// parseSondeo arma lo visto en una distro con la salida de la primera ida
// (y, en el respaldo /proc, la de la segunda: procs2). Devuelve también los
// PID que falta describir si el respaldo necesita esa segunda ida.
func parseSondeo(distro, out string) (sondeo, []uint32, error) {
	s, err := leerSalida(distro, out)
	if err != nil {
		return sondeo{Distro: distro}, nil, err
	}
	socks, err := s.sockets()
	if err != nil {
		return sondeo{Distro: distro}, nil, err
	}
	sd := sondeo{Distro: distro, Fuente: s.fuente, Sockets: socks, inodes: s.inodes}
	if s.fuente == "/proc" {
		return sd, pidsDeInodos(socks, s.inodes), nil
	}
	net, hay, err := s.tablasNet()
	if err != nil {
		return sondeo{Distro: distro}, nil, err
	}
	if hay {
		sd.Procesos = atribuirPorInodo(socks, net, s.procs)
	} else {
		sd.Procesos = atribuir(socks, s.procs, nil)
	}
	return sd, nil, nil
}

// completarSondeo termina el respaldo /proc con la salida de la segunda ida.
func completarSondeo(sd sondeo, out string) (sondeo, error) {
	s, err := leerSalida(sd.Distro, out)
	if err != nil {
		return sd, err
	}
	sd.Procesos = atribuir(sd.Sockets, s.procs, sd.inodes)
	return sd, nil
}

// atribuirPorInodo le asigna a cada proceso los sockets que de verdad tiene
// abiertos: los que ss lista cuyo inodo (el de la tabla de /proc/net, que da
// el kernel) aparece entre los fd del proceso. Lo que ss imprime como dueño
// NO se cree: el nombre de un proceso lo elige él mismo (prctl), ss no lo
// escapa, y un programa sin privilegios que se llame `a",pid=1,fd=3` hace que
// la fila diga que el dueño es el PID 1 —y killport le manda señales como
// root—. ss solo sirvió para elegir a quién describir. El inodo se busca por
// protocolo, dirección y puerto; si la dirección no cuadra (ss y /proc/net la
// escriben distinto en algún caso raro), por protocolo y puerto: igual es un
// socket de ese puerto que el proceso tiene abierto. O(s · n).
func atribuirPorInodo(socks []ports.LinuxSocket, net []ports.LinuxSocket, procs []procesoLinux) []procesoLinux {
	type pp struct {
		proto ports.Proto
		port  uint16
	}
	type exacta struct {
		pp
		addr netip.Addr
	}
	porDir := map[exacta][]uint64{}
	porPuerto := map[pp][]uint64{}
	for _, n := range net {
		k := pp{n.Proto, n.Port()}
		porPuerto[k] = append(porPuerto[k], n.Inode)
		e := exacta{k, n.Local.Addr().WithZone("")}
		porDir[e] = append(porDir[e], n.Inode)
	}
	for i := range procs {
		procs[i].Sockets = nil
	}
	for _, s := range socks {
		k := pp{s.Proto, s.Port()}
		inos := porDir[exacta{k, s.Local.Addr().WithZone("")}]
		if len(inos) == 0 {
			inos = porPuerto[k]
		}
		for i := range procs {
			if p := &procs[i]; p.PID != 0 && p.Arranque != 0 && tieneAlguno(p.inodos, inos) {
				p.Sockets = append(p.Sockets, s)
			}
		}
	}
	var out []procesoLinux
	visto := map[uint32]bool{}
	for _, p := range procs {
		if len(p.Sockets) > 0 && !visto[p.PID] {
			visto[p.PID] = true
			out = append(out, p)
		}
	}
	return out
}

func tieneAlguno(xs, ys []uint64) bool {
	for _, x := range xs {
		if slices.Contains(ys, x) {
			return true
		}
	}
	return false
}

// atribuir le asigna a cada proceso sus sockets: por los dueños que trae ss o
// por el mapa inodo → PID del respaldo. Lo que dice ss solo se usa si no hay
// tablas de /proc/net con qué confirmarlo (ver atribuirPorInodo). Solo quedan los procesos con al menos
// un socket y con identidad completa (sin hora de arranque no se lo puede
// cerrar sin riesgo de pegarle a un PID reciclado).
func atribuir(socks []ports.LinuxSocket, procs []procesoLinux, inodes map[uint64][]uint32) []procesoLinux {
	idx := map[uint32]int{}
	for i, p := range procs {
		if p.PID != 0 && p.Arranque != 0 {
			idx[p.PID] = i
		}
	}
	for _, s := range socks {
		var pids []uint32
		for _, u := range s.Users {
			pids = appendUnique(pids, u.PID)
		}
		if s.Inode != 0 {
			for _, pid := range inodes[s.Inode] {
				pids = appendUnique(pids, pid)
			}
		}
		for _, pid := range pids {
			if i, ok := idx[pid]; ok {
				procs[i].Sockets = append(procs[i].Sockets, s)
			}
		}
	}
	var out []procesoLinux
	for i, p := range procs {
		if j, ok := idx[p.PID]; ok && j == i && len(p.Sockets) > 0 {
			out = append(out, p)
		}
	}
	return out
}

// pidsDeInodos son los PID dueños de los sockets dados, según el mapa.
func pidsDeInodos(socks []ports.LinuxSocket, inodes map[uint64][]uint32) []uint32 {
	var out []uint32
	for _, s := range socks {
		for _, p := range inodes[s.Inode] {
			out = appendUnique(out, p)
		}
	}
	return out
}

// parseStat lee /proc/PID/stat: "812 (node) S 1 812 … starttime …". El
// nombre va entre el PRIMER '(' y el ÚLTIMO ')' (puede tener espacios y
// paréntesis); después vienen los campos desde el 3 (estado), así que el 22
// (starttime) es el índice 19 de lo que sigue.
func parseStat(s string) (comm string, start uint64, ok bool) {
	i, j := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if i < 0 || j < i {
		return "", 0, false
	}
	comm = s[i+1 : j]
	f := strings.Fields(s[j+1:])
	if len(f) < 20 {
		return "", 0, false
	}
	start, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil || start == 0 {
		return "", 0, false
	}
	return comm, start, true
}

func appendUnique(xs []uint32, v uint32) []uint32 {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

// --- cerrar un proceso Linux ------------------------------------------------

// Pasos de la espera dentro de Linux. No hay forma de que sh espere la
// muerte de un proceso que no es su hijo sin sondear (pidfd_open no está al
// alcance de un script), así que se mira /proc cada 50 ms: la latencia de
// darse cuenta es ≤ 50 ms y el costo, un `tr` por paso. La espera total la
// fija --timeout y además la acota el ejecutor del lado de Windows.
const (
	pasoKill     = 50 * time.Millisecond
	graciaTerm   = time.Second     // tras SIGINT, cuánto se le da a SIGTERM
	graciaKill   = 2 * time.Second // tras SIGKILL, cuánto se espera a que el kernel lo baje
	margenScript = 10 * time.Second
)

// scriptCierre arma el script que cierra UN proceso Linux con la escalera de
// siempre —SIGINT (el Ctrl+C de Linux), SIGTERM, SIGKILL— verificando antes de
// cada señal que el PID siga siendo EL MISMO proceso (misma hora de arranque):
// si se recicló, no se toca. Con force va directo a SIGKILL. Termina solo y
// dice cómo terminó: "@fin GONE|DENIED|INT|TERM|KILL|STUCK".
func scriptCierre(p procesoLinux, timeout time.Duration, force bool) string {
	ticks := func(d time.Duration) int { return max(int((d+pasoKill-1)/pasoKill), 1) }
	f := 0
	if force {
		f = 1
	}
	return fmt.Sprintf(`export LC_ALL=C PATH=/usr/sbin:/usr/bin:/sbin:/bin:$PATH
P=%d; T=%d; N=%d; M=%d; K=%d; F=%d
st() { s=$(tr '\n' ' ' < /proc/$P/stat 2>/dev/null) || return 1; s="${s##*) }"; set -- $s; echo "${20}"; }
vive() { [ "$(st)" = "$T" ]; }
espera() { i=0; while vive && [ $i -lt $1 ]; do sleep 0.05 2>/dev/null || sleep 1; i=$((i+1)); done; }
vive || { echo '@fin GONE'; exit 0; }
if [ $F = 0 ]; then
  if ! kill -INT $P 2>/dev/null; then vive && echo '@fin DENIED' || echo '@fin GONE'; exit 0; fi
  espera $N; vive || { echo '@fin INT'; exit 0; }
  kill -TERM $P 2>/dev/null; espera $M; vive || { echo '@fin TERM'; exit 0; }
fi
vive || { echo '@fin GONE'; exit 0; }
kill -KILL $P 2>/dev/null || { echo '@fin DENIED'; exit 0; }
espera $K; vive && echo '@fin STUCK' || echo '@fin KILL'
`, p.PID, p.Arranque, ticks(timeout), ticks(graciaTerm), ticks(graciaKill), f)
}

// plazoCierre es el tope del lado de Windows para el script de cierre: lo que
// puede tardar la escalera entera más el arranque de wsl.exe.
func plazoCierre(timeout time.Duration) time.Duration {
	return timeout + graciaTerm + graciaKill + margenScript
}

// resultadoCierre traduce la última marca del script al veredicto del motor.
func resultadoCierre(out string) (outcome string, err error) {
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "@fin "); ok {
			switch v {
			case "GONE", "DENIED", "INT", "TERM", "KILL", "STUCK":
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("%w: el cierre no informó cómo terminó", ErrSondeo)
}

// linuxForwarded dice si un socket de Linux es de los que el relay de WSL
// reenvía a localhost de Windows: los que escuchan en todas las interfaces o
// en loopback. Uno atado a la IP de eth0 no se reenvía.
func linuxForwarded(s ports.LinuxSocket) bool {
	a := s.Local.Addr()
	return s.Iface == "" && (a.IsUnspecified() || a.IsLoopback() || (a.Is4In6() && (a.Unmap().IsUnspecified() || a.Unmap().IsLoopback())))
}

// distrosDeMotor son los prefijos de las distros de WSL que NO son del
// usuario: las crea y las maneja un motor de contenedores para correr
// adentro (Docker Desktop, Rancher Desktop, la máquina de Podman). Lo que
// escucha ahí es del motor o de sus contenedores.
var distrosDeMotor = []string{"docker-desktop", "rancher-desktop", "podman-machine", "podman-"}

// distroDeMotor dice si la distro es de un motor de contenedores.
func distroDeMotor(name string) bool {
	n := strings.ToLower(name)
	return slices.ContainsFunc(distrosDeMotor, func(p string) bool { return strings.HasPrefix(n, p) })
}

// esDelMotor dice si el proceso Linux es parte de un motor de contenedores:
// su reenvío de puertos dentro de una distro del usuario, o cualquier cosa
// dentro de la distro del motor. No se cierra: el objetivo es el contenedor.
func (p procesoLinux) esDelMotor() bool {
	return relaysDeLinux[p.Comm] || distroDeMotor(p.Distro)
}

// relaysDeLinux son los nombres de procesos Linux que reenvían puertos de
// contenedores DENTRO de la distro (Docker o Podman instalados en la distro):
// el mismo error que en Windows, un nivel más abajo. No se cierran.
var relaysDeLinux = map[string]bool{
	"docker-proxy": true, "rootlessport": true, "rootlesskit": true, "slirp4netns": true,
	"pasta": true, "pasta.avx2": true, "conmon": true, "containerd-shim": true, "gvproxy": true,
}
