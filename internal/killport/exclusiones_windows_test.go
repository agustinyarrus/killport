//go:build windows

package killport

import (
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/win"
)

// sidDeServicio calcula el SID de un servicio como Windows: S-1-5-80 seguido
// del SHA-1 del nombre en mayúsculas (UTF-16LE), en cinco enteros de 32 bits.
// Es lo que hace que el SDDL de una reserva de URL nombre al servicio en
// cualquier idioma.
func sidDeServicio(nombre string) string {
	u := utf16.Encode([]rune(strings.ToUpper(nombre)))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	h := sha1.Sum(b)
	partes := []string{"S-1-5-80"}
	for i := 0; i < 5; i++ {
		partes = append(partes, strconv.FormatUint(uint64(binary.LittleEndian.Uint32(h[4*i:])), 10))
	}
	return strings.Join(partes, "-")
}

// El SID de la reserva de WinRM de la muestra real es el de su nombre, y
// LookupAccountSid lo devuelve a su nombre: el que usa la tabla de Windows.
func TestServicioDeSIDReal(t *testing.T) {
	if got := sidDeServicio("WinRM"); got != "S-1-5-80-569256582-2953403351-2909559716-1301513147-412116970" {
		t.Fatalf("el SID de WinRM = %s", got)
	}
	for _, nombre := range []string{"WinRM", "Wecsvc"} {
		if got := servicioDeSID(sidDeServicio(nombre)); !strings.EqualFold(got, nombre) {
			t.Errorf("servicioDeSID(SID de %s) = %q", nombre, got)
		}
	}
	for _, sid := range []string{"LS", "S-1-5-19", "S-1-5-21-1111111111-2222222222-3333333333-1001", "S-1-5-80-x", ""} {
		if got := servicioDeSID(sid); got != "" {
			t.Errorf("servicioDeSID(%q) = %q: solo los SID de servicio se resuelven", sid, got)
		}
	}
}

// El adaptador, con netsh, los sockets y los servicios falsos pero las
// salidas de netsh REALES (SSDP atendiendo el 2869): la fila del 2869 queda a
// nombre de SSDP, los bloques de WinNAT como estaban, y las consultas de más
// (colas y reservas de URL) corren una vez, y solo porque hay una fila que
// puede ser de http.sys.
func TestAdaptadorAtribuyeExclusiones(t *testing.T) {
	var mu sync.Mutex
	pedidos := map[string]int{}
	tablas := map[string]string{}
	for _, f := range []string{"ipv4", "ipv6"} {
		tablas["interface "+f+" show excludedportrange protocol=tcp"] = leerNetsh(t, "netsh_es_"+f+"_tcp_ssdp.txt")
		tablas["interface "+f+" show excludedportrange protocol=udp"] = leerNetsh(t, "netsh_es_"+f+"_udp.txt")
	}
	tablas["http show servicestate view=requestq verbose=no"] = leerNetsh(t, "netsh_es_requestq_ssdp.txt")
	tablas["http show urlacl"] = leerNetsh(t, "netsh_es_urlacl.txt")
	netsh := func(_ context.Context, args ...string) (string, error) {
		pedido := strings.Join(args, " ")
		mu.Lock()
		pedidos[pedido]++
		mu.Unlock()
		if out, ok := tablas[pedido]; ok {
			return out, nil
		}
		if strings.Contains(pedido, "dynamicport") {
			return "\nIntervalo\n------\nPuerto de inicio    : 49152\nNúmero de puertos   : 16384\n", nil
		}
		return "", errors.New("pedido inesperado: " + pedido)
	}
	escucha := func(p uint16) ports.Socket {
		return ports.Socket{Proto: ports.TCP, Local: netip.AddrPortFrom(netip.IPv4Unspecified(), p), State: ports.Listen, PID: 4}
	}
	s := &sistema{red: nuevaRed(netsh), http: nuevoHTTPSys(netsh),
		escuchando: func(context.Context) ([]ports.Socket, error) {
			return []ports.Socket{escucha(2869), escucha(445)}, nil
		},
		listaServicios: func() ([]win.Service, error) {
			return []win.Service{{Name: "SSDPSRV", PID: 6528}, {Name: "Spooler", PID: 3100}}, nil
		},
	}
	for vez := 0; vez < 2; vez++ {
		xs, err := s.Excluded()
		if err != nil {
			t.Fatal(err)
		}
		for _, fam := range []ports.Family{ports.IPv4, ports.IPv6} {
			x, ok := xs.Find(ports.TCP, fam, 2869)
			want := ports.HTTPHold{Held: true, URL: "http://*:2869/UPNP/EVENTING/", Service: "SSDPSRV", What: "SSDP (UPnP), el descubrimiento de dispositivos de la red"}
			if !ok || x.HTTP != want || x.Origin() != ports.OriginHTTP {
				t.Fatalf("el 2869 %v = %+v (%v)", fam, x, x.Origin())
			}
			if y, _ := xs.Find(ports.TCP, fam, 55200); y.Origin() != ports.OriginWinNAT || y.HTTP != (ports.HTTPHold{}) {
				t.Fatalf("el bloque de WinNAT quedó %+v", y)
			}
		}
	}
	if pedidos["http show urlacl"] != 1 || pedidos["http show servicestate view=requestq verbose=no"] != 1 {
		t.Fatalf("consultas a http.sys = %v: una de cada una, memoizadas", pedidos)
	}

	// Sin filas que puedan ser de http.sys (solo bloques de WinNAT y la
	// administrada de UDP), no se le pregunta nada.
	pedidos = map[string]int{}
	for _, f := range []string{"ipv4", "ipv6"} {
		tablas["interface "+f+" show excludedportrange protocol=tcp"] = strings.ReplaceAll(leerNetsh(t, "netsh_es_"+f+"_tcp_ssdp.txt"), "      2869        2869      \r\n", "")
	}
	s2 := &sistema{red: nuevaRed(netsh), http: nuevoHTTPSys(netsh)}
	xs, err := s2.Excluded()
	if err != nil || len(xs) != 14 {
		t.Fatalf("exclusiones = %v, %v", xs, err)
	}
	if pedidos["http show urlacl"] != 0 || pedidos["http show servicestate view=requestq verbose=no"] != 0 {
		t.Fatalf("le preguntó a http.sys sin que hiciera falta: %v", pedidos)
	}
}

// La fila de una exclusión en el JSON dice de quién es y, si es de http.sys,
// la URL y el servicio.
func TestRangoJSON(t *testing.T) {
	x := ports.ExcludedRange{Proto: ports.TCP, Family: ports.IPv4, Range: ports.R(2869, 2869),
		HTTP: ports.HTTPHold{Held: true, URL: "http://*:2869/UPNP/EVENTING/", Service: "SSDPSRV"}}
	if j := rangoJSON(x); *j != (jsonRango{Inicio: 2869, Fin: 2869, Origen: "httpsys", URL: "http://*:2869/UPNP/EVENTING/", Servicio: "SSDPSRV"}) {
		t.Errorf("rangoJSON = %+v", j)
	}
	w := ports.ExcludedRange{Proto: ports.TCP, Family: ports.IPv4, Range: ports.R(55150, 55249)}
	if j := rangoJSON(w); *j != (jsonRango{Inicio: 55150, Fin: 55249, Origen: "winnat"}) {
		t.Errorf("rangoJSON = %+v", j)
	}
}
