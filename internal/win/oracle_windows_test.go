//go:build windows

package win

// Pruebas contra el sistema real, cada una con otra herramienta de Windows
// como oráculo: la configuración de un servicio (contra `sc qc`, otra herramienta que lee lo
// mismo), el registro (contra `reg query`), la salida de consola en la página
// de códigos OEM, el sondeo de bind EXCLUSIVO y la marca de proceso crítico.
//
// Nada de acá cambia la configuración de la máquina: los caminos que la
// cambian (dejar un servicio en manual, arrancar o detener un controlador) se
// prueban solo hasta la negativa —sin administrador, Windows se niega ANTES
// de tocar nada— y se saltean si la prueba corre elevada.

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// correr lanza un comando del sistema con el ejecutor acotado (sin ventana).
func correr(t *testing.T, exe string, args ...string) (string, uint32) {
	t.Helper()
	res, err := RunBounded(context.Background(), SystemExe(exe), args, Bounded{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("%s %v: %v", exe, args, err)
	}
	return ConsoleText(res.Stdout), res.ExitCode
}

// numerosDeSC saca, en orden, los valores numéricos de `sc qc` o `sc query`:
// las líneas «ETIQUETA : N  CONSTANTE». Las etiquetas salen traducidas; los
// números y las constantes, no. OJO: sc escribe los números en HEXADECIMAL y
// sin prefijo ("20  WIN32_SHARE_PROCESS" es 0x20, "110" es 0x110).
var lineaSC = regexp.MustCompile(`(?m)^\s+\S+\s*:\s*([0-9a-fA-F]+)\s+([A-Z][A-Z0-9_]+)`)

func numerosDeSC(out string) (nums []uint32, consts []string) {
	for _, m := range lineaSC.FindAllStringSubmatch(out, -1) {
		n, _ := strconv.ParseUint(m[1], 16, 32)
		nums = append(nums, uint32(n))
		consts = append(consts, m[2])
	}
	return nums, consts
}

// disparadoresDeSC cuenta los disparadores de `sc qtriggerinfo`. La salida
// sale traducida entera ("INICIAR SERVICIO"), así que se cuenta por la forma:
// cada disparador es un bloque cuya primera línea —la acción— tiene ocho
// espacios de sangría; el tipo y los datos van más adentro.
func disparadoresDeSC(out string) int {
	n := 0
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, " \r")
		if strings.HasPrefix(l, "        ") && len(l) > 8 && l[8] != ' ' {
			n++
		}
	}
	return n
}

// La configuración de tres servicios que están en todo Windows, contra lo que
// dice `sc qc`: un controlador del kernel (HTTP: de él cuelga http.sys), un
// servicio compartido de inicio automático (iphlpsvc: el de portproxy) y uno
// con proceso propio (Spooler).
func TestServiceConfigContraSC(t *testing.T) {
	for _, nombre := range []string{"HTTP", "iphlpsvc", "Spooler", "winnat"} {
		out, code := correr(t, "sc.exe", "qc", nombre)
		if code != 0 {
			t.Logf("%s: sc qc salió con %d (¿no está en esta máquina?): no se compara", nombre, code)
			continue
		}
		nums, consts := numerosDeSC(out)
		if len(nums) < 2 {
			t.Fatalf("%s: no entendí la salida de sc qc:\n%s", nombre, out)
		}
		c, err := QueryServiceConfig(nombre)
		if err != nil {
			t.Fatalf("QueryServiceConfig(%s): %v", nombre, err)
		}
		if c.Type != nums[0] || uint32(c.Start) != nums[1] {
			t.Errorf("%s: tipo 0x%x inicio %d; sc qc dice tipo 0x%x (%s) inicio %d (%s)", nombre, c.Type, c.Start, nums[0], consts[0], nums[1], consts[1])
		}
		if c.Name != nombre || c.DisplayName == "" {
			t.Errorf("%s: nombre %q, visible %q", nombre, c.Name, c.DisplayName)
		}
		if c.BinaryPath == "" || !strings.Contains(strings.ToLower(out), strings.ToLower(c.BinaryPath)) {
			t.Errorf("%s: la línea de comandos %q no es la que muestra sc qc", nombre, c.BinaryPath)
		}
		esDriver := strings.Contains(consts[0], "DRIVER")
		if c.Driver() != esDriver {
			t.Errorf("%s: Driver() = %v y sc qc dice %s", nombre, c.Driver(), consts[0])
		}
		if esDriver && c.Image() != "" {
			t.Errorf("%s: un controlador no tiene ejecutable y Image() = %q", nombre, c.Image())
		}
		if !esDriver && !strings.HasSuffix(strings.ToLower(c.Image()), ".exe") {
			t.Errorf("%s: Image() = %q", nombre, c.Image())
		}
		// El inicio retrasado solo lo puede tener uno automático, y sc qc lo
		// dice entre paréntesis.
		if c.Delayed != strings.Contains(out, "(DELAYED)") {
			t.Errorf("%s: Delayed = %v y sc qc dice:\n%s", nombre, c.Delayed, out)
		}
		t.Logf("%s: tipo 0x%x, inicio %d (retrasado %v), %d disparadores, recuperación %v, parcial %v",
			nombre, c.Type, c.Start, c.Delayed, c.Triggers, c.Recovery, c.Partial)
	}
}

// Los disparadores, contra `sc qtriggerinfo`.
func TestServiceTriggersContraSC(t *testing.T) {
	for _, nombre := range []string{"iphlpsvc", "Spooler", "WinRM", "bthserv", "W32Time"} {
		c, err := QueryServiceConfig(nombre)
		if errors.Is(err, ErrServiceNotFound) {
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", nombre, err)
		}
		out, code := correr(t, "sc.exe", "qtriggerinfo", nombre)
		if code != 0 {
			continue
		}
		if want := disparadoresDeSC(out); c.Triggers != want {
			t.Errorf("%s: %d disparadores y sc qtriggerinfo muestra %d:\n%s", nombre, c.Triggers, want, out)
		}
		t.Logf("%s: %d disparadores", nombre, c.Triggers)
	}
}

func TestServiceStateContraSC(t *testing.T) {
	fijos := 0
	for _, nombre := range []string{"HTTP", "iphlpsvc", "winnat", "Spooler", "RpcSs", "DcomLaunch"} {
		out, code := correr(t, "sc.exe", "query", nombre)
		if code != 0 {
			continue
		}
		// Qué órdenes acepta: sc query lo dice entre paréntesis, con las
		// constantes sin traducir ("(STOPPABLE, …" o "(NOT_STOPPABLE, …").
		if st, err := QueryServiceState(nombre); err == nil && st.Running() {
			if want := strings.Contains(out, "(STOPPABLE"); st.AcceptsStop() != want {
				t.Errorf("%s: AcceptsStop() = %v (órdenes 0x%x) y sc query dice:\n%s", nombre, st.AcceptsStop(), st.Accepts, out)
			}
			if !st.AcceptsStop() {
				fijos++
			}
		}
		nums, consts := numerosDeSC(out)
		if len(nums) < 2 {
			t.Fatalf("%s: no entendí la salida de sc query:\n%s", nombre, out)
		}
		st, err := QueryServiceState(nombre)
		if err != nil {
			t.Fatalf("QueryServiceState(%s): %v", nombre, err)
		}
		if st.State != nums[1] {
			t.Errorf("%s: estado %d y sc query dice %d (%s)", nombre, st.State, nums[1], consts[1])
		}
		if st.Running() != (consts[1] == "RUNNING") || st.Stopped() != (consts[1] == "STOPPED") {
			t.Errorf("%s: corriendo %v, detenido %v, y sc query dice %s", nombre, st.Running(), st.Stopped(), consts[1])
		}
	}
	// RpcSs corre en todo Windows y no se deja detener: si ninguno de la lista
	// dio "no acepta", no se está leyendo el campo que corresponde.
	if fijos == 0 {
		t.Error("ningún servicio corre sin aceptar la orden de detenerse (ni RpcSs): AcceptsStop no está mirando dwControlsAccepted")
	}
}

func TestServiceErrores(t *testing.T) {
	const fantasma = "killport-servicio-que-no-existe"
	if _, err := QueryServiceConfig(fantasma); !errors.Is(err, ErrServiceNotFound) {
		t.Errorf("QueryServiceConfig de uno que no existe: %v", err)
	}
	if _, err := QueryServiceState(fantasma); !errors.Is(err, ErrServiceNotFound) {
		t.Errorf("QueryServiceState de uno que no existe: %v", err)
	}
	if err := SetServiceDemandStart(fantasma); !errors.Is(err, ErrServiceNotFound) {
		t.Errorf("SetServiceDemandStart de uno que no existe: %v", err)
	}
	if err := StartService(context.Background(), fantasma, time.Now().Add(time.Second)); !errors.Is(err, ErrServiceNotFound) {
		t.Errorf("StartService de uno que no existe: %v", err)
	}
	if got := ActiveDependents(fantasma); got != nil {
		t.Errorf("ActiveDependents de uno que no existe: %v", got)
	}
	if IsElevated() {
		t.Skip("la prueba corre elevada: no se intenta cambiar ni arrancar un servicio real")
	}
	// Sin administrador Windows se niega al ABRIR el servicio con el permiso
	// de cambiarlo: no llega a tocarse nada. Se verifica que quedó como estaba.
	antes, err := QueryServiceConfig("Spooler")
	if err != nil {
		t.Skipf("no hay Spooler en esta máquina: %v", err)
	}
	if err := SetServiceDemandStart("Spooler"); !errors.Is(err, ErrAccessDenied) {
		t.Errorf("SetServiceDemandStart sin administrador: %v", err)
	}
	if err := StartService(context.Background(), "winnat", time.Now().Add(time.Second)); !errors.Is(err, ErrAccessDenied) && !errors.Is(err, ErrServiceNotFound) {
		t.Errorf("StartService sin administrador: %v", err)
	}
	despues, err := QueryServiceConfig("Spooler")
	if err != nil || despues.Start != antes.Start || despues.Delayed != antes.Delayed {
		t.Errorf("Spooler cambió: antes %+v, después %+v (%v)", antes, despues, err)
	}
}

// Los servicios activos que dependen de un controlador: los que usan http.sys
// son los sospechosos de tener un puerto que figura a nombre de System.
func TestActiveDependents(t *testing.T) {
	st, err := QueryServiceState("HTTP")
	if err != nil || !st.Running() {
		t.Skipf("el controlador HTTP no está corriendo (%v)", err)
	}
	for _, n := range ActiveDependents("HTTP") {
		if n == "" {
			t.Error("un dependiente sin nombre")
			continue
		}
		if s, err := QueryServiceState(n); err != nil || s.Stopped() {
			t.Errorf("%s figura como dependiente activo y su estado es %+v (%v)", n, s, err)
		}
	}
}

// El registro, contra reg.exe.
func TestMachineValuesContraReg(t *testing.T) {
	const clave = `SOFTWARE\Microsoft\Windows NT\CurrentVersion`
	vals, existe, err := MachineValues(clave)
	if err != nil || !existe {
		t.Fatalf("MachineValues: existe %v, %v", existe, err)
	}
	por := map[string]string{}
	for _, v := range vals {
		if _, dup := por[strings.ToLower(v.Name)]; dup {
			t.Errorf("el valor %q salió dos veces", v.Name)
		}
		por[strings.ToLower(v.Name)] = v.Data
	}
	for _, nombre := range []string{"CurrentBuild", "ProductName", "SystemRoot"} {
		out, code := correr(t, "reg.exe", "query", `HKLM\`+clave, "/v", nombre, "/reg:64")
		if code != 0 {
			t.Fatalf("reg query %s salió con %d", nombre, code)
		}
		got, ok := por[strings.ToLower(nombre)]
		if !ok || got == "" {
			t.Errorf("%s no salió (o salió vacío)", nombre)
			continue
		}
		// reg query imprime «    Nombre    REG_SZ    dato».
		if !regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(nombre) + `\s+REG_(EXPAND_)?SZ\s+` + regexp.QuoteMeta(got) + `\s*$`).MatchString(out) {
			t.Errorf("%s = %q y reg query dice:\n%s", nombre, got, out)
		}
	}
	if !strings.EqualFold(por["systemroot"], WindowsDirectory()) {
		t.Errorf("SystemRoot = %q y la carpeta de Windows es %q", por["systemroot"], WindowsDirectory())
	}
	// Los valores que no son de texto se saltean (este es un DWORD).
	if _, ok := por["currentmajorversionnumber"]; ok {
		t.Error("CurrentMajorVersionNumber es un DWORD: no tendría que salir")
	}
	for _, v := range vals {
		if strings.ContainsRune(v.Data, 0) || strings.ContainsRune(v.Name, 0) {
			t.Errorf("%q = %q trae un cero adentro", v.Name, v.Data)
		}
	}
}

func TestMachineValuesNoExiste(t *testing.T) {
	vals, existe, err := MachineValues(`SOFTWARE\killport\una-clave-que-no-existe`)
	if err != nil || existe || vals != nil {
		t.Fatalf("clave inexistente: %v, existe %v, %v", vals, existe, err)
	}
	// Las reglas de portproxy: si nunca hubo ninguna, la clave no existe y eso
	// es "no hay reglas", no un error. Si existe, cada valor es una regla.
	for _, clase := range []string{"v4tov4", "v4tov6", "v6tov4", "v6tov6"} {
		vals, existe, err := MachineValues(`SYSTEM\CurrentControlSet\Services\PortProxy\` + clase + `\tcp`)
		if err != nil {
			t.Errorf("%s: %v", clase, err)
		}
		if !existe && vals != nil {
			t.Errorf("%s: no existe y trae %v", clase, vals)
		}
		for _, v := range vals {
			if !strings.Contains(v.Name, "/") || !strings.Contains(v.Data, "/") {
				t.Errorf("%s: %q = %q no tiene la forma dirección/puerto", clase, v.Name, v.Data)
			}
		}
	}
}

func TestRegString(t *testing.T) {
	u := func(s string) []byte {
		var b []byte
		for _, x := range syscall.StringToUTF16(s) { // con el cero final
			b = append(b, byte(x), byte(x>>8))
		}
		return b
	}
	casos := []struct {
		in   []byte
		want string
	}{
		{u("172.24.1.5/3000"), "172.24.1.5/3000"},
		{u("sin cero")[:16], "sin cero"},                      // el registro guarda lo que le escribieron
		{append(u("hasta acá"), u("basura")...), "hasta acá"}, // corta en el primer cero
		{append(u("impar"), 0x41), "impar"},                   // un byte suelto al final
		{nil, ""},
		{[]byte{0, 0}, ""},
		{[]byte{0x41}, ""},
	}
	for _, c := range casos {
		if got := regString(c.in); got != c.want {
			t.Errorf("regString(% x) = %q, esperaba %q", c.in, got, c.want)
		}
	}
}

func TestConsoleText(t *testing.T) {
	if got := ConsoleText(nil); got != "" {
		t.Errorf("vacío: %q", got)
	}
	utf := "Número máx. de solicitudes: 1000\r\n"
	if got := ConsoleText([]byte(utf)); got != utf {
		t.Errorf("UTF-8 válido tiene que pasar tal cual: %q", got)
	}
	r, _ := call(modkernel32.NewProc("GetOEMCP"))
	// En las páginas 437 y 850 (las de una consola en inglés o en español),
	// 0xA2 es "ó" y 0xA4 es "ñ"; sueltos no son UTF-8 válido.
	if r != 437 && r != 850 {
		t.Skipf("la página OEM de esta máquina es la %d", r)
	}
	if got := ConsoleText([]byte("Exclusi\xa2n de puertos: espa\xa4ol")); got != "Exclusión de puertos: español" {
		t.Errorf("desde la página %d: %q", r, got)
	}
}

// --- C3: el sondeo de bind es exclusivo -------------------------------------

// servidorEn abre un servidor TCP de verdad en esa dirección, en un puerto que
// elige el sistema.
func servidorEn(t *testing.T, red, dir string) (net.Listener, uint16) {
	t.Helper()
	l, err := net.Listen(red, dir)
	if err != nil {
		t.Skipf("no pude escuchar en %s: %v", dir, err)
	}
	t.Cleanup(func() { l.Close() })
	return l, netip.MustParseAddrPort(l.Addr().String()).Port()
}

// El caso que motivó la corrección: un servidor escuchando SOLO en loopback.
// Un bind común a la dirección comodín no choca con él y el sondeo decía
// "libre"; con uso exclusivo, no puede decirlo.
func TestProbeBindVeElLoopback(t *testing.T) {
	_, p4 := servidorEn(t, "tcp4", "127.0.0.1:0")
	st, err := ProbeBind(p4, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if st == BindFree {
		t.Errorf("con un servidor en 127.0.0.1:%d, el sondeo de 0.0.0.0:%d dio libre", p4, p4)
	}

	_, p6 := servidorEn(t, "tcp6", "[::1]:0")
	st, err = ProbeBind(p6, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if st == BindFree {
		t.Errorf("con un servidor en [::1]:%d, el sondeo de [::]:%d dio libre", p6, p6)
	}
}

// Cada familia se sondea por lo suyo (IPV6_V6ONLY): un servidor IPv4 no hace
// que el sondeo de IPv6 diga "prohibido" —que se leería "lo reserva
// Windows"—, ni al revés.
func TestProbeBindFamiliasSeparadas(t *testing.T) {
	_, p4 := servidorEn(t, "tcp4", "127.0.0.1:0")
	if st, err := ProbeBind(p4, true, true); err != nil || st != BindFree {
		// Puede haber OTRO socket en ese puerto en IPv6: se mira la tabla.
		if !puertoEnTabla(t, p4, true) {
			t.Errorf("servidor solo en IPv4 (puerto %d): el sondeo de IPv6 dio %v, %v", p4, st, err)
		}
	}
	_, p6 := servidorEn(t, "tcp6", "[::1]:0")
	if st, err := ProbeBind(p6, true, false); err != nil || st != BindFree {
		if !puertoEnTabla(t, p6, false) {
			t.Errorf("servidor solo en IPv6 (puerto %d): el sondeo de IPv4 dio %v, %v", p6, st, err)
		}
	}
}

// puertoEnTabla dice si algún socket TCP de esa familia tiene ese puerto local.
func puertoEnTabla(t *testing.T, port uint16, ipv6 bool) bool {
	t.Helper()
	socks, err := TCPSockets(ipv6)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range socks {
		if s.Local.Port() == port {
			return true
		}
	}
	return false
}

// El sondeo no deja nada atrás: después de sondear un puerto libre, un
// servidor de verdad lo puede tomar enseguida, con uso exclusivo y todo.
func TestProbeBindNoDejaNada(t *testing.T) {
	l, port := servidorEn(t, "tcp4", "127.0.0.1:0")
	l.Close()
	for i := 0; i < 50; i++ {
		if st, err := ProbeBind(port, true, false); err != nil || st != BindFree {
			if puertoEnTabla(t, port, false) {
				t.Skipf("otro proceso tomó el %d en el medio", port)
			}
			t.Fatalf("sondeo %d del puerto %d: %v, %v", i, port, st, err)
		}
	}
	s, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Closesocket(s)
	if err := setSockOptBool(uintptr(s), solSocket, soExclusiveAddrUse); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(s, &syscall.SockaddrInet4{Port: int(port)}); err != nil {
		t.Errorf("después de 50 sondeos, el puerto %d no se deja atar: %v", port, err)
	}
}

// netshTabla corre `netsh interface ipvX show excludedportrange` y devuelve
// los rangos como (inicio, fin, administrado). Parser mínimo, de prueba: el
// de verdad está en internal/ports, que este paquete no importa.
func netshTabla(t *testing.T, ipv, proto string) [][3]int {
	t.Helper()
	out, code := correr(t, "netsh.exe", "interface", ipv, "show", "excludedportrange", "protocol="+proto)
	if code != 0 {
		t.Skipf("netsh salió con %d", code)
	}
	var rangos [][3]int
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		lo, err1 := strconv.Atoi(f[0])
		hi, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		adm := 0
		if len(f) == 3 && f[2] == "*" {
			adm = 1
		}
		rangos = append(rangos, [3]int{lo, hi, adm})
	}
	return rangos
}

// C3, contra la tabla real de la máquina. Un bloque que reservó WinNAT niega
// el bind (WSAEACCES). Una exclusión ADMINISTRADA (la fila con "*", lo que
// deja `netsh … add excludedportrange`) NO lo niega: solo saca el puerto del
// reparto dinámico. Es lo que hace que `killport --reserve` sirva, y por lo
// que el diagnóstico sondea antes de decir "reservado".
func TestProbeBindContraLasExclusiones(t *testing.T) {
	probados := 0
	for _, c := range []struct {
		ipv, proto string
		tcp, v6    bool
	}{{"ipv4", "tcp", true, false}, {"ipv4", "udp", false, false}, {"ipv6", "tcp", true, true}, {"ipv6", "udp", false, true}} {
		for _, r := range netshTabla(t, c.ipv, c.proto) {
			port := uint16(r[0] + (r[1]-r[0])/2)
			st, err := ProbeBind(port, c.tcp, c.v6)
			if err != nil {
				t.Fatalf("%s %s %d: %v", c.ipv, c.proto, port, err)
			}
			probados++
			switch {
			case r[2] == 0 && st != BindForbidden:
				t.Errorf("%s %s %d está en el bloque %d–%d de WinNAT y el sondeo dio %v", c.ipv, c.proto, port, r[0], r[1], st)
			case r[2] == 1 && st == BindForbidden:
				t.Errorf("%s %s %d está en la exclusión administrada %d–%d y el sondeo dio %v: tendría que dejarse atar", c.ipv, c.proto, port, r[0], r[1], st)
			}
		}
	}
	if probados == 0 {
		t.Skip("esta máquina no tiene rangos excluidos: no hay contra qué probar")
	}
	t.Logf("%d rangos excluidos probados", probados)
}

// La marca de proceso crítico: killport y un proceso cualquiera no la tienen.
func TestHandleCritical(t *testing.T) {
	for _, pid := range []uint32{CurrentProcessID(), spawnPing(t)} {
		h, err := OpenForKill(pid, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		critico, seSupo := h.Critical()
		h.Close()
		if !seSupo || critico {
			t.Errorf("PID %d: crítico %v, se supo %v", pid, critico, seSupo)
		}
	}
}
