package killport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
)

// Las salidas de netsh de estas pruebas son las REALES de ports/testdata
// (Windows 11 en español): la tabla con SSDP atendiendo el 2869, la mezcla
// de las tres clases, la cola de SSDPSRV y las reservas de URL de fábrica.

func leerNetsh(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "ports", "testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func exclusionesDe(t *testing.T, files ...string) ports.Exclusions {
	t.Helper()
	var out ports.Exclusions
	for _, f := range files {
		fam := ports.IPv4
		if strings.Contains(f, "ipv6") {
			fam = ports.IPv6
		}
		xs, err := ports.ParseExcluded(leerNetsh(t, f), ports.TCP, fam)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, xs...)
	}
	return out
}

func colasDe(t *testing.T, file string) []ports.RequestQueue {
	t.Helper()
	qs, err := ports.ParseRequestQueues(leerNetsh(t, file))
	if err != nil {
		t.Fatal(err)
	}
	return qs
}

func reservasDe(t *testing.T) []ports.URLReservation {
	t.Helper()
	rs, err := ports.ParseURLReservations(leerNetsh(t, "netsh_es_urlacl.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// sids resuelve los SID de servicio de la muestra como lo haría
// LookupAccountSid (ver TestServicioDeSIDReal).
func sids(sid string) string {
	return map[string]string{
		"S-1-5-80-569256582-2953403351-2909559716-1301513147-412116970":  "WinRM",
		"S-1-5-80-4059739203-877974739-1245631912-527174227-2996563517":  "Wecsvc",
		"S-1-5-80-2375682873-768044350-3534595160-1005545032-2873800392": "WMPNetworkSvc",
	}[sid]
}

// filaEn busca la fila IPv4 de ese puerto.
func filaEn(t *testing.T, xs ports.Exclusions, p uint16) ports.ExcludedRange {
	t.Helper()
	x, ok := xs.Find(ports.TCP, ports.IPv4, p)
	if !ok {
		t.Fatalf("no hay fila para el %d", p)
	}
	return x
}

func TestAtribuirHTTP(t *testing.T) {
	ssdp := exclusionesDe(t, "netsh_es_ipv4_tcp_ssdp.txt", "netsh_es_ipv6_tcp_ssdp.txt")
	mezcla := exclusionesDe(t, "netsh_es_ipv4_tcp_mezcla.txt", "netsh_es_ipv6_tcp_mezcla.txt")
	// El 5357 de WSD y el 5985 de WinRM, como aparecen cuando esos servicios
	// atienden (la misma forma que el 2869: una fila de un puerto).
	sueltas := ports.Exclusions{
		{Proto: ports.TCP, Family: ports.IPv4, Range: ports.R(5357, 5357)},
		{Proto: ports.TCP, Family: ports.IPv4, Range: ports.R(5985, 5985)},
		{Proto: ports.TCP, Family: ports.IPv4, Range: ports.R(40000, 40000)},
		{Proto: ports.UDP, Family: ports.IPv4, Range: ports.R(2869, 2869)},
	}
	svchostSSDP := func(pid uint32) []string {
		if pid == 6528 {
			return []string{"SSDPSRV"}
		}
		return nil
	}
	casos := []struct {
		nombre string
		xs     ports.Exclusions
		ev     evidenciaHTTP
		puerto uint16
		want   ports.HTTPHold // Held=false: sin pruebas (es de un programa)
	}{
		// La cola de SSDPSRV a la vista (lo estaba, sin administrador): la URL
		// registrada, el servicio que la atiende y para qué es.
		{"SSDP con su cola", ssdp, evidenciaHTTP{colas: colasDe(t, "netsh_es_requestq_ssdp.txt"), reservas: reservasDe(t), serviciosDe: svchostSSDP}, 2869,
			ports.HTTPHold{Held: true, URL: "http://*:2869/UPNP/EVENTING/", Service: "SSDPSRV", What: "SSDP (UPnP), el descubrimiento de dispositivos de la red"}},
		// Sin la cola (la de otro usuario solo la ve un administrador): el
		// kernel escuchando y la reserva de fábrica, a nombre de Servicio local.
		{"SSDP sin su cola", ssdp, evidenciaHTTP{reservas: reservasDe(t), kernel: ports.Of(2869)}, 2869,
			ports.HTTPHold{Held: true, URL: "http://*:2869/", Account: "servicio local", What: "SSDP (UPnP), el descubrimiento de dispositivos de la red"}},
		// Solo la reserva: alcanza para decir de quién es.
		{"SSDP solo la reserva", ssdp, evidenciaHTTP{reservas: reservasDe(t)}, 2869,
			ports.HTTPHold{Held: true, URL: "http://*:2869/", Account: "servicio local", What: "SSDP (UPnP), el descubrimiento de dispositivos de la red"}},
		// Solo el kernel escuchando: es de http.sys, sin más detalle.
		{"solo el kernel", ssdp, evidenciaHTTP{kernel: ports.Of(2869)}, 2869, ports.HTTPHold{Held: true}},
		// Sin nada de http.sys en ese puerto: de un programa.
		{"sin pruebas", ssdp, evidenciaHTTP{colas: colasDe(t, "netsh_es_requestq_mezcla.txt"), kernel: ports.Of(80)}, 2869, ports.HTTPHold{}},
		// Un programa cualquiera con su URL en el 2869 no es SSDP: se dice su
		// URL, no "qué es" por el número ni a nombre de quién está la reserva
		// de fábrica (que es de otra URL).
		{"otro en el 2869", ssdp, evidenciaHTTP{colas: []ports.RequestQueue{{Name: "x", Processes: []ports.QueueProcess{{PID: 900}},
			URLs: []ports.QueueURL{{Host: "localhost", Port: 2869, Path: "/APP/"}}}}, reservas: reservasDe(t)}, 2869,
			ports.HTTPHold{Held: true, URL: "http://localhost:2869/APP/"}},
		// WSD: la reserva es de usuarios y de Servicio local.
		{"WSD", sueltas, evidenciaHTTP{reservas: reservasDe(t), kernel: ports.Of(5357)}, 5357,
			ports.HTTPHold{Held: true, URL: "http://*:5357/", Account: "usuarios y servicio local", What: "WSD, con el que la detección de redes publica esta PC"}},
		// WinRM: reservada a dos servicios por su SID (WinRM y Wecsvc); se
		// queda con el que la tabla conoce en ese puerto.
		{"WinRM por el SID", sueltas, evidenciaHTTP{reservas: reservasDe(t), servicioDeSID: sids}, 5985,
			ports.HTTPHold{Held: true, URL: "http://+:5985/wsman/", Service: "WinRM", What: "la administración remota de Windows (WinRM)"}},
		// Un HttpListener cualquiera: la URL de su cola, sin servicio ni "qué es".
		{"HttpListener", mezcla, evidenciaHTTP{colas: colasDe(t, "netsh_es_requestq_mezcla.txt"), reservas: reservasDe(t), serviciosDe: svchostSSDP}, 47230,
			ports.HTTPHold{Held: true, URL: "http://localhost:47230/KP-A/"}},
		// Atada a 127.0.0.1: la fila existe en IPv4 y en IPv6, y la URL se lee.
		{"atada a una IP", mezcla, evidenciaHTTP{colas: colasDe(t, "netsh_es_requestq_mezcla.txt")}, 47233,
			ports.HTTPHold{Held: true, URL: "http://127.0.0.1:47233/KP-C/"}},
		// La reserva de 7 puertos de un programa no es candidata: ni se mira.
		{"reserva de un programa", mezcla, evidenciaHTTP{colas: colasDe(t, "netsh_es_requestq_mezcla.txt"), kernel: ports.Of(47111, 47112)}, 47112, ports.HTTPHold{}},
		// Un puerto suelto sin nada de http.sys.
		{"suelta sin pruebas", sueltas, evidenciaHTTP{reservas: reservasDe(t)}, 40000, ports.HTTPHold{}},
	}
	for _, c := range casos {
		xs := atribuirHTTP(c.xs, c.ev)
		x := filaEn(t, xs, c.puerto)
		if x.HTTP != c.want {
			t.Errorf("%s: %+v\n  esperaba %+v", c.nombre, x.HTTP, c.want)
		}
		wantOrigen := ports.OriginProgram
		if c.want.Held {
			wantOrigen = ports.OriginHTTP
		}
		if x.Origin() != wantOrigen {
			t.Errorf("%s: origen %v, esperaba %v", c.nombre, x.Origin(), wantOrigen)
		}
		// Los bloques de WinNAT quedan intactos, y la entrada no se toca.
		for i, y := range xs {
			if y.Len() == ports.WinNATBlock && (y.HTTP != ports.HTTPHold{} || y.Origin() != ports.OriginWinNAT) {
				t.Errorf("%s: el bloque %v quedó %+v", c.nombre, y.Range, y)
			}
			if c.xs[i].HTTP != (ports.HTTPHold{}) {
				t.Fatalf("%s: atribuirHTTP cambió la tabla de entrada", c.nombre)
			}
		}
	}
	// Una fila UDP de un puerto nunca es de http.sys (atiende por TCP).
	xs := atribuirHTTP(sueltas, evidenciaHTTP{reservas: reservasDe(t), kernel: ports.Of(2869)})
	if x, _ := xs.Find(ports.UDP, ports.IPv4, 2869); x.HTTP.Held {
		t.Errorf("una fila UDP quedó a nombre de http.sys: %+v", x)
	}
}

// Una cuenta de usuario no se nombra (no dice para qué es el puerto y es un
// dato de la persona); las de Windows, por su alias de SDDL.
func TestCuentaSDDL(t *testing.T) {
	for sid, want := range map[string]string{
		"LS": "servicio local", "S-1-5-19": "servicio local", "NS": "servicio de red", "BU": "usuarios",
		"WD": "todos", "SY": "SYSTEM", "BA": "administradores", "AU": "usuarios autenticados", "IU": "usuarios interactivos",
		"S-1-5-21-1111111111-2222222222-3333333333-1001": "", "S-1-5-80-1-2-3-4-5": "", "": "",
	} {
		if got := cuentaSDDL(sid); got != want {
			t.Errorf("cuentaSDDL(%q) = %q, esperaba %q", sid, got, want)
		}
	}
}

func TestElegirServicio(t *testing.T) {
	casos := []struct {
		servicios []string
		puerto    uint16
		want      string
	}{
		{nil, 2869, ""},
		{[]string{"SSDPSRV"}, 2869, "SSDPSRV"},
		{[]string{"SSDPSRV", "SSDPSRV"}, 2869, "SSDPSRV"},
		{[]string{"Wecsvc", "WinRM"}, 5985, "WinRM"},
		{[]string{"Wecsvc", "WinRM"}, 9999, ""},          // ninguno conocido en ese puerto: no se adivina
		{[]string{"SSDPSRV", "upnphost"}, 2869, ""},      // los dos conocidos: no se adivina
		{[]string{"MiServicio", "OtroServicio"}, 80, ""}, // un svchost compartido cualquiera
	}
	for _, c := range casos {
		if got := elegirServicio(c.servicios, c.puerto); got != c.want {
			t.Errorf("elegirServicio(%v, %d) = %q, esperaba %q", c.servicios, c.puerto, got, c.want)
		}
	}
}

// Lo que se dice de cada clase, en --free, en el JSON y al negarse a
// reservar: solo WinNAT se nombra como WinNAT.
func TestTextosDeReserva(t *testing.T) {
	winnat := ports.ExcludedRange{Proto: ports.TCP, Family: ports.IPv4, Range: ports.R(55150, 55249)}
	http := ports.ExcludedRange{Proto: ports.TCP, Family: ports.IPv4, Range: ports.R(2869, 2869),
		HTTP: ports.HTTPHold{Held: true, URL: "http://*:2869/", Account: "servicio local", What: "SSDP (UPnP), el descubrimiento de dispositivos de la red"}}
	httpSinQue := ports.ExcludedRange{Proto: ports.TCP, Family: ports.IPv4, Range: ports.R(47230, 47230), HTTP: ports.HTTPHold{Held: true, URL: "http://localhost:47230/KP-A/"}}
	programa := ports.ExcludedRange{Proto: ports.TCP, Family: ports.IPv4, Range: ports.R(47111, 47117)}
	admin := ports.ExcludedRange{Proto: ports.UDP, Family: ports.IPv4, Range: ports.R(50000, 50059), Administered: true}

	casos := []struct {
		x          ports.ExcludedRange
		free, json string
	}{
		{winnat, "lo reservó WinNAT para Hyper-V, WSL o Docker (bloque 55150–55249)", "winnat"},
		{http, "lo aparta http.sys mientras lo atiende: SSDP (UPnP), el descubrimiento de dispositivos de la red", "httpsys"},
		{httpSinQue, "lo aparta http.sys mientras lo atiende (http://localhost:47230/KP-A/)", "httpsys"},
		{programa, "lo reservó un programa mientras corre, no WinNAT (exclusión 47111–47117)", "programa"},
	}
	for _, c := range casos {
		if got := textoReserva(c.x); got != c.free {
			t.Errorf("textoReserva(%v) = %q\n  esperaba %q", c.x.Range, got, c.free)
		}
		if got := origenJSON(c.x); got != c.json {
			t.Errorf("origenJSON(%v) = %q, esperaba %q", c.x.Range, got, c.json)
		}
	}
	if got := origenJSON(admin); got != "administrada" {
		t.Errorf("origenJSON(administrada) = %q", got)
	}

	for _, x := range []ports.ExcludedRange{http, programa} {
		txt := porQueNoReservo(ports.R(x.Lo, x.Lo), tcp4, x)
		if !strings.Contains(txt, "cae en la exclusión "+x.Range.String()) || !strings.Contains(txt, "no es de WinNAT") || !strings.Contains(txt, "no lo toco") {
			t.Errorf("porQueNoReservo(%v) = %q", x.Range, txt)
		}
	}
	if txt := porQueNoReservo(ports.R(2869, 2869), tcp4, http); !strings.Contains(txt, "SSDP (UPnP)") {
		t.Errorf("no dice para qué es: %q", txt)
	}
}

// conOrigen le pone a cada tramo salteado su fila atribuida, y solo a los
// que no son administrados (una administrada no se confunde con otra fila del
// mismo rango).
func TestConOrigen(t *testing.T) {
	cruda := ports.ExcludedRange{Proto: ports.TCP, Family: ports.IPv4, Range: ports.R(2869, 2869)}
	atribuida := cruda
	atribuida.HTTP = ports.HTTPHold{Held: true, URL: "http://*:2869/"}
	skips := []ports.Skip{
		{Range: ports.R(2869, 2869), Why: ports.SkipReserved, Excluded: cruda},
		{Range: ports.R(2870, 2870), Why: ports.SkipTaken},
	}
	got := conOrigen(skips, ports.Exclusions{atribuida})
	if got[0].Excluded != atribuida || got[1].Why != ports.SkipTaken || skips[0].Excluded != cruda {
		t.Fatalf("conOrigen = %+v (entrada %+v)", got, skips)
	}
}
