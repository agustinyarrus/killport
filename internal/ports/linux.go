package ports

// linux.go: lo que escucha DENTRO de una distro de WSL, leído de las dos
// fuentes que hay en cualquier Linux: la salida de `ss -Hltnup` (iproute2) y,
// donde no hay ss (Alpine y otras mínimas), las tablas crudas /proc/net/tcp,
// tcp6, udp y udp6. Puro y sin build tag: se prueba con salidas grabadas.
//
// Las dos fuentes se normalizan al mismo LinuxSocket. ss trae los procesos
// dueños ya resueltos (users:(("node",pid=812,fd=20))); /proc/net trae el
// inodo del socket, que se une con el mapa inodo → PID de /proc/*/fd.

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// LinuxSocket es un socket que ocupa un puerto dentro de Linux: TCP en LISTEN
// o UDP sin conectar (lo que ss -l llama UNCONN).
type LinuxSocket struct {
	Proto Proto
	Local netip.AddrPort // IPv6 con zona si vino con %iface; "*" de ss es [::] (doble pila)
	Iface string         // interfaz a la que está atado (SO_BINDTODEVICE, el "%lo" de ss); "" si ninguna
	Inode uint64         // solo /proc/net: el inodo del socket (0 si vino de ss)
	UID   uint32         // solo /proc/net: el dueño del socket
	Users []LinuxUser    // solo ss: los procesos que lo tienen abierto (varios tras un fork)
}

// LinuxUser es un proceso Linux que tiene un socket abierto.
type LinuxUser struct {
	Name string // comm del proceso (hasta 15 caracteres; puede tener espacios)
	PID  uint32
	FD   int
}

// Port es el puerto local.
func (s LinuxSocket) Port() uint16 { return s.Local.Port() }

// ErrLinuxFormat: la salida no tiene la forma esperada (no es ss ni /proc/net).
var ErrLinuxFormat = errors.New("salida de Linux con un formato inesperado")

// ParseSS lee la salida de `ss -Hltnup` (o `ss -ltnup`, con cabecera). Cada
// fila es: [Netid] State Recv-Q Send-Q Local Peer [Process]. Netid aparece
// cuando se piden TCP y UDP juntos (-t -u); si falta, el protocolo sale del
// estado (LISTEN es TCP, UNCONN es UDP). Se quedan solo las filas que ocupan
// un puerto: TCP LISTEN y UDP UNCONN. Una fila ilegible es error (mejor decir
// "no pude leer" que inventar un dueño); las cabeceras y líneas vacías se
// saltan. O(largo de la salida).
func ParseSS(out string) ([]LinuxSocket, error) {
	var socks []LinuxSocket
	for n, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		f := fieldsN(line, 6)
		if len(f) == 0 || f[0] == "Netid" || f[0] == "State" {
			continue // vacía o cabecera (ss sin -H)
		}
		proto, state, rest, ok := ssHead(f)
		if !ok {
			return nil, fmt.Errorf("%w: línea %d de ss: %q", ErrLinuxFormat, n+1, line)
		}
		if !(proto == TCP && state == "LISTEN") && !(proto == UDP && state == "UNCONN") {
			continue
		}
		// rest = Recv-Q Send-Q Local Peer [Process]
		rf := fieldsN(rest, 5)
		if len(rf) < 4 {
			return nil, fmt.Errorf("%w: línea %d de ss sin dirección: %q", ErrLinuxFormat, n+1, line)
		}
		local, iface, err := parseSSAddr(rf[2])
		if err != nil {
			return nil, fmt.Errorf("%w: línea %d de ss: %v", ErrLinuxFormat, n+1, err)
		}
		s := LinuxSocket{Proto: proto, Local: local, Iface: iface}
		if len(rf) == 5 {
			s.Users = parseSSUsers(rf[4])
		}
		socks = append(socks, s)
	}
	return socks, nil
}

// ssHead separa el protocolo y el estado del principio de una fila de ss y
// devuelve el resto de la línea desde Recv-Q.
func ssHead(f []string) (Proto, string, string, bool) {
	netid := strings.ToLower(f[0])
	switch {
	case strings.HasPrefix(netid, "tcp") && len(f) >= 3:
		return TCP, f[1], strings.Join(f[2:], " "), true
	case strings.HasPrefix(netid, "udp") && len(f) >= 3:
		return UDP, f[1], strings.Join(f[2:], " "), true
	case netid == "listen" && len(f) >= 2:
		return TCP, "LISTEN", strings.Join(f[1:], " "), true
	case netid == "unconn" && len(f) >= 2:
		return UDP, "UNCONN", strings.Join(f[1:], " "), true
	}
	return 0, "", "", false
}

// fieldsN parte por espacios en a lo sumo n campos: el último se lleva el
// resto de la línea tal cual (la columna Process puede tener espacios dentro
// de un nombre entre comillas: ("Web Content",pid=…)).
func fieldsN(s string, n int) []string {
	var out []string
	s = strings.TrimLeft(s, " \t")
	for s != "" && len(out) < n-1 {
		i := strings.IndexAny(s, " \t")
		if i < 0 {
			break
		}
		out = append(out, s[:i])
		s = strings.TrimLeft(s[i:], " \t")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// parseSSAddr lee una dirección local de ss: "0.0.0.0:3000",
// "127.0.0.53%lo:53", "[::]:3000", "[fe80::1%eth0]:546", "*:22". El puerto
// es lo que sigue al último ':'.
func parseSSAddr(s string) (netip.AddrPort, string, error) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return netip.AddrPort{}, "", fmt.Errorf("dirección sin puerto %q", s)
	}
	host, portStr := s[:i], s[i+1:]
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return netip.AddrPort{}, "", fmt.Errorf("puerto inválido en %q", s)
	}
	var iface string
	switch {
	case host == "*":
		return netip.AddrPortFrom(netip.IPv6Unspecified(), uint16(port)), "", nil
	case strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]"):
		host = host[1 : len(host)-1]
	}
	if j := strings.IndexByte(host, '%'); j >= 0 {
		iface = host[j+1:]
		host = host[:j]
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.AddrPort{}, "", fmt.Errorf("dirección inválida %q", s)
	}
	if a.Is6() && iface != "" && a.IsLinkLocalUnicast() {
		a = a.WithZone(iface) // una link-local sin zona es ambigua; con zona es exacta
	}
	return netip.AddrPortFrom(a, uint16(port)), iface, nil
}

// parseSSUsers lee users:(("node",pid=812,fd=20),("node",pid=813,fd=20)). El
// nombre va entre comillas y ss no lo escapa, así que se corta en `",pid=` y
// no en la primera comilla. Lo que no cuadra se ignora: la columna es
// informativa y el socket igual existe.
//
// OJO: informativa y nada más. El comm lo elige el propio proceso (prctl),
// y uno que se llame `a",pid=1,fd=3` hace que esto lea el PID 1 y pierda al
// dueño de verdad: la lectura es ambigua por construcción. Quien decide a
// qué proceso mandarle señales no le puede creer a esto; la herramienta
// confirma cada dueño por el inodo del socket (internal/killport/wsl_parse.go,
// atribuirPorInodo).
func parseSSUsers(s string) []LinuxUser {
	_, s, ok := strings.Cut(s, "users:(")
	if !ok {
		return nil
	}
	var out []LinuxUser
	for {
		start := strings.Index(s, `("`)
		if start < 0 {
			return out
		}
		s = s[start+2:]
		end := strings.Index(s, `",pid=`)
		if end < 0 {
			return out
		}
		name := s[:end]
		s = s[end+len(`",pid=`):]
		pidStr, rest, _ := strings.Cut(s, ",")
		pid, err := strconv.ParseUint(pidStr, 10, 32)
		if err != nil {
			return out
		}
		fd := -1
		if v, ok := strings.CutPrefix(rest, "fd="); ok {
			num := v
			if k := strings.IndexByte(v, ')'); k >= 0 {
				num = v[:k]
			}
			if n, err := strconv.Atoi(num); err == nil {
				fd = n
			}
		}
		out = append(out, LinuxUser{Name: name, PID: uint32(pid), FD: fd})
		s = rest
	}
}

// Estados de /proc/net/tcp (include/net/tcp_states.h): solo importa LISTEN;
// en /proc/net/udp, 07 (TCP_CLOSE) es un socket sin conectar = servidor.
const (
	linuxTCPListen = 0x0A
	linuxUDPUnconn = 0x07
)

// ParseProcNet lee /proc/net/tcp, tcp6, udp o udp6 y devuelve los sockets que
// ocupan un puerto (TCP en LISTEN, UDP sin conectar). Formato de cada fila:
//
//	sl  local_address rem_address   st tx_queue:rx_queue tr:tm->when retrnsmt  uid  timeout inode …
//	0: 0100007F:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000  0 23456 …
//
// Trampa: la dirección es el entero de 32 bits en el ORDEN DE LA MÁQUINA
// (little-endian en x86-64 y arm64, que es donde corre WSL) impreso en hex:
// 127.0.0.1 sale "0100007F". En IPv6 son cuatro palabras así. El puerto, en
// cambio, ya es el número (0BB8 = 3000). O(filas).
func ParseProcNet(out string, proto Proto, v6 bool) ([]LinuxSocket, error) {
	var socks []LinuxSocket
	header := false
	for n, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "sl" {
			header = true
			continue
		}
		if len(f) < 10 || !strings.HasSuffix(f[0], ":") {
			return nil, fmt.Errorf("%w: línea %d de /proc/net: %q", ErrLinuxFormat, n+1, line)
		}
		st, err := strconv.ParseUint(f[3], 16, 8)
		if err != nil {
			return nil, fmt.Errorf("%w: estado %q en la línea %d", ErrLinuxFormat, f[3], n+1)
		}
		if (proto == TCP && st != linuxTCPListen) || (proto == UDP && st != linuxUDPUnconn) {
			continue
		}
		local, err := parseProcAddr(f[1], v6)
		if err != nil {
			return nil, fmt.Errorf("%w: línea %d: %v", ErrLinuxFormat, n+1, err)
		}
		uid, err1 := strconv.ParseUint(f[7], 10, 32)
		inode, err2 := strconv.ParseUint(f[9], 10, 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("%w: uid/inodo ilegibles en la línea %d", ErrLinuxFormat, n+1)
		}
		socks = append(socks, LinuxSocket{Proto: proto, Local: local, UID: uint32(uid), Inode: inode})
	}
	if !header && len(socks) == 0 && strings.TrimSpace(out) != "" {
		return nil, fmt.Errorf("%w: no es una tabla de /proc/net", ErrLinuxFormat)
	}
	return socks, nil
}

// parseProcAddr decodifica "0100007F:0BB8" (v4) o la versión de 32 dígitos (v6).
func parseProcAddr(s string, v6 bool) (netip.AddrPort, error) {
	hexAddr, hexPort, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("dirección sin puerto %q", s)
	}
	port, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("puerto inválido %q", s)
	}
	words := 1
	if v6 {
		words = 4
	}
	if len(hexAddr) != 8*words {
		return netip.AddrPort{}, fmt.Errorf("dirección de largo inesperado %q", s)
	}
	var b [16]byte
	for w := 0; w < words; w++ {
		v, err := strconv.ParseUint(hexAddr[8*w:8*w+8], 16, 32)
		if err != nil {
			return netip.AddrPort{}, fmt.Errorf("dirección inválida %q", s)
		}
		// La palabra está en el orden de la máquina (little-endian): el byte
		// menos significativo es el primero en memoria.
		b[4*w], b[4*w+1], b[4*w+2], b[4*w+3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
	}
	var a netip.Addr
	if v6 {
		a = netip.AddrFrom16(b)
	} else {
		a = netip.AddrFrom4([4]byte{b[0], b[1], b[2], b[3]})
	}
	return netip.AddrPortFrom(a, uint16(port)), nil
}
