package ports

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/netip"
	"os"
	"strings"
	"testing"
)

func leer(t *testing.T, nombre string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + nombre)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ss -Hltnup de un Ubuntu típico: resolved atado a lo (%lo), doble pila con
// "*", un nombre con espacio, varios dueños por fork, link-local con zona.
func TestParseSSUbuntu(t *testing.T) {
	socks, err := ParseSS(leer(t, "wsl_ss_ubuntu.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(socks) != 13 {
		t.Fatalf("esperaba 13 sockets, hubo %d", len(socks))
	}
	find := func(proto Proto, local string) LinuxSocket {
		t.Helper()
		for _, s := range socks {
			if s.Proto == proto && s.Local.String() == local {
				return s
			}
		}
		t.Fatalf("no está %v %s", proto, local)
		return LinuxSocket{}
	}
	if s := find(TCP, "0.0.0.0:3000"); len(s.Users) != 1 || s.Users[0] != (LinuxUser{Name: "node", PID: 812, FD: 20}) {
		t.Fatalf("node 3000 = %+v", s.Users)
	}
	if s := find(TCP, "[::]:3000"); s.Users[0].FD != 21 {
		t.Fatalf("node [::]:3000 = %+v", s.Users)
	}
	if s := find(TCP, "127.0.0.53:53"); s.Iface != "lo" || s.Users[0].Name != "systemd-resolve" {
		t.Fatalf("resolved = %+v", s)
	}
	if s := find(UDP, "127.0.0.53:53"); s.Iface != "lo" {
		t.Fatalf("resolved udp = %+v", s)
	}
	if s := find(TCP, "[::]:22"); len(s.Users) != 2 || s.Users[1] != (LinuxUser{Name: "sshd", PID: 900, FD: 3}) {
		t.Fatalf("'*:22' es doble pila con dos dueños: %+v", s)
	}
	if s := find(TCP, "[fe80::215:5dff:fe12:3456%eth0]:9090"); s.Users[0].Name != "Web Content" || s.Iface != "eth0" {
		t.Fatalf("link-local = %+v", s)
	}
	if s := find(TCP, "0.0.0.0:5173"); len(s.Users) != 2 || s.Users[1].PID != 1002 {
		t.Fatalf("fork = %+v", s)
	}
	if s := find(UDP, "0.0.0.0:5353"); s.Users[0].Name != "avahi-daemon" {
		t.Fatalf("mdns = %+v", s)
	}
}

// Sin -H viene la cabecera; con CRLF (pasó por Windows) también se lee; y las
// conexiones establecidas no ocupan el puerto.
func TestParseSSCabeceraYCRLF(t *testing.T) {
	socks, err := ParseSS(leer(t, "wsl_ss_cabecera_crlf.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(socks) != 1 || socks[0].Port() != 3000 || socks[0].Users[0].PID != 812 {
		t.Fatalf("sockets = %+v", socks)
	}
}

// Sin root, ss no puede ver los procesos: el socket existe igual, sin dueños.
func TestParseSSSinRoot(t *testing.T) {
	socks, err := ParseSS(leer(t, "wsl_ss_sin_root.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(socks) != 2 || socks[0].Users != nil || socks[1].Iface != "lo" {
		t.Fatalf("sockets = %+v", socks)
	}
}

// Un solo protocolo (sin columna Netid): el protocolo sale del estado.
func TestParseSSSinNetid(t *testing.T) {
	socks, err := ParseSS("LISTEN 0      511          0.0.0.0:3000       0.0.0.0:*    users:((\"node\",pid=812,fd=20))\nUNCONN 0 0 0.0.0.0:5353 0.0.0.0:*\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(socks) != 2 || socks[0].Proto != TCP || socks[1].Proto != UDP {
		t.Fatalf("sockets = %+v", socks)
	}
}

func TestParseSSErrores(t *testing.T) {
	for _, in := range []string{
		"ss: invalid option -- 'H'",
		"tcp LISTEN 0 511 sin-puerto 0.0.0.0:*",
		"tcp LISTEN 0 511 999.1.1.1:80 0.0.0.0:*",
		"tcp LISTEN 0 511 0.0.0.0:99999 0.0.0.0:*",
	} {
		if _, err := ParseSS(in); !errors.Is(err, ErrLinuxFormat) {
			t.Errorf("%q: esperaba ErrLinuxFormat, dio %v", in, err)
		}
	}
	if socks, err := ParseSS(""); err != nil || len(socks) != 0 {
		t.Fatalf("vacío = %v/%v", socks, err)
	}
}

// users:(…) mal formado no rompe la fila: el socket existe igual.
func TestParseSSUsersRaros(t *testing.T) {
	for in, want := range map[string]int{
		`users:(("a",pid=1,fd=2))`:                    1,
		`users:(("a b",pid=1,fd=2),("c",pid=3,fd=4))`: 2,
		`users:(("a",pid=x,fd=2))`:                    0,
		`users:(("a"`:                                 0,
		`nada`:                                        0,
		`users:(("con,coma",pid=7,fd=1))`:             1,
	} {
		if got := parseSSUsers(in); len(got) != want {
			t.Errorf("%s: %d dueños, esperaba %d (%+v)", in, len(got), want, got)
		}
	}
}

func TestParseProcNet(t *testing.T) {
	tcp4, err := ParseProcNet(leer(t, "wsl_proc_net_tcp.txt"), TCP, false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range tcp4 {
		got = append(got, fmt.Sprintf("%s uid=%d inode=%d", s.Local, s.UID, s.Inode))
	}
	want := []string{"127.0.0.53:53 uid=101 inode=18731", "0.0.0.0:3000 uid=1000 inode=34567", "127.0.0.1:8000 uid=0 inode=35000"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("tcp4 =\n%s\nesperaba\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	tcp6, err := ParseProcNet(leer(t, "wsl_proc_net_tcp6.txt"), TCP, true)
	if err != nil {
		t.Fatal(err)
	}
	got = got[:0]
	for _, s := range tcp6 {
		got = append(got, s.Local.String())
	}
	if strings.Join(got, "|") != "[::]:3000|[::1]:631|[::ffff:127.0.0.1]:8080" {
		t.Fatalf("tcp6 = %v", got)
	}

	udp4, err := ParseProcNet(leer(t, "wsl_proc_net_udp.txt"), UDP, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(udp4) != 2 || udp4[0].Local.String() != "127.0.0.53:53" || udp4[1].Port() != 5353 {
		t.Fatalf("udp4 = %+v (la conectada no ocupa)", udp4)
	}
}

func TestParseProcNetErrores(t *testing.T) {
	for _, in := range []string{
		"no soy una tabla",
		"  sl local\n   0: ZZZZ:0BB8 00000000:0000 0A 0 0 0 0 0 1",
		"  sl local\n   0: 0100007F:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 x 1",
		"  sl local\n   0: 0100007F0:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 5 1",
	} {
		if _, err := ParseProcNet(in, TCP, false); !errors.Is(err, ErrLinuxFormat) {
			t.Errorf("%q: esperaba ErrLinuxFormat, dio %v", in, err)
		}
	}
	if s, err := ParseProcNet("  sl  local_address rem_address\n", TCP, false); err != nil || len(s) != 0 {
		t.Fatalf("solo cabecera = %v/%v", s, err)
	}
}

// Propiedad: /proc/net escrito con el formato EXACTO del kernel
// (seq_printf "%08X:%04X", palabras en el orden de la máquina) y leído de
// vuelta da las mismas direcciones y puertos, en v4 y v6, con los estados
// que no ocupan filtrados. El codificador es independiente del parser: sale
// de net/ipv4/tcp_ipv4.c (get_tcp4_sock) y net/ipv6/tcp_ipv6.c (get_tcp6_sock).
func TestParseProcNetPropiedad(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 8))
	le := func(b []byte) uint32 { return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24 }
	for iter := 0; iter < 300; iter++ {
		v6 := rng.IntN(2) == 0
		var sb strings.Builder
		sb.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
		var want []netip.AddrPort
		n := rng.IntN(40)
		for i := 0; i < n; i++ {
			var a netip.Addr
			var hex string
			if v6 {
				var b [16]byte
				for k := range b {
					b[k] = byte(rng.IntN(256))
				}
				a = netip.AddrFrom16(b)
				hex = fmt.Sprintf("%08X%08X%08X%08X", le(b[0:4]), le(b[4:8]), le(b[8:12]), le(b[12:16]))
			} else {
				b := [4]byte{byte(rng.IntN(256)), byte(rng.IntN(256)), byte(rng.IntN(256)), byte(rng.IntN(256))}
				a = netip.AddrFrom4(b)
				hex = fmt.Sprintf("%08X", le(b[:]))
			}
			port := uint16(rng.IntN(65536))
			st := 1 + rng.IntN(11)
			if rng.IntN(2) == 0 {
				st = linuxTCPListen
			}
			fmt.Fprintf(&sb, "%4d: %s:%04X %s:0000 %02X 00000000:00000000 00:00000000 00000000 %5d        0 %d 1 0000000000000000 100 0 0 10 0\n",
				i, hex, port, strings.Repeat("0", len(hex)), st, rng.IntN(70000), rng.IntN(1<<30))
			if st == linuxTCPListen {
				want = append(want, netip.AddrPortFrom(a, port))
			}
		}
		got, err := ParseProcNet(sb.String(), TCP, v6)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		if len(got) != len(want) {
			t.Fatalf("iter %d: %d sockets, esperaba %d", iter, len(got), len(want))
		}
		for i := range want {
			if got[i].Local != want[i] {
				t.Fatalf("iter %d fila %d: %v, esperaba %v", iter, i, got[i].Local, want[i])
			}
		}
	}
}
