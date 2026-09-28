//go:build windows

package killport

// Función 3, segunda mitad: detrás del svchost del IP Helper hay una regla de
// portproxy, y el objetivo es la regla. El registro y netsh son dobles:
// ninguna de estas pruebas lee ni cambia las reglas de la máquina.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

// registroFalso es el registro de las pruebas: clave → valores. netsh delete
// lo cambia, como el de verdad.
type registroFalso struct {
	claves  map[string][]win.RegValue
	err     error    // leer falla con esto
	pedidos []string // lo que se le pidió a netsh
	falla   error    // netsh falla con esto (y no cambia nada)
	sordo   bool     // netsh dice que sí y no borra nada
	lee     int      // cuántas veces se leyó el registro
}

func (r *registroFalso) leer(clave string) ([]win.RegValue, bool, error) {
	r.lee++
	if r.err != nil {
		return nil, false, r.err
	}
	vals, ok := r.claves[clave]
	return vals, ok, nil
}

func (r *registroFalso) netsh(_ context.Context, args ...string) (string, error) {
	r.pedidos = append(r.pedidos, strings.Join(args, " "))
	if r.falla != nil {
		return "", r.falla
	}
	if r.sordo {
		return "\n", nil
	}
	// interface portproxy delete CLASE listenport=P [listenaddress=D] protocol=tcp
	clave, puerto, dir := `SYSTEM\CurrentControlSet\Services\PortProxy\`+args[3]+`\tcp`, "", "*"
	for _, a := range args[4:] {
		switch k, v, _ := strings.Cut(a, "="); k {
		case "listenport":
			puerto = v
		case "listenaddress":
			dir = v
		}
	}
	var quedan []win.RegValue
	for _, v := range r.claves[clave] {
		if !strings.EqualFold(v.Name, dir+"/"+puerto) {
			quedan = append(quedan, v)
		}
	}
	if len(quedan) == len(r.claves[clave]) {
		return "", &errorNetsh{Codigo: 1, Dijo: "El sistema no puede encontrar el archivo especificado."}
	}
	r.claves[clave] = quedan
	return "\n", nil
}

func (r *registroFalso) cliente(elevado bool) *portproxy {
	return &portproxy{leer: r.leer, netsh: r.netsh, elevado: func() bool { return elevado }}
}

const (
	claveV4V4 = `SYSTEM\CurrentControlSet\Services\PortProxy\v4tov4\tcp`
	claveV6V4 = `SYSTEM\CurrentControlSet\Services\PortProxy\v6tov4\tcp`
)

func registroDePrueba() *registroFalso {
	return &registroFalso{claves: map[string][]win.RegValue{
		claveV4V4: {
			{Name: "0.0.0.0/8080", Data: "172.24.1.5/80"},
			{Name: "0.0.0.0/3000", Data: "172.24.1.5/3000"},
			{Name: "*/4000", Data: "localhost/4000"},
			{Name: "esto no es una regla", Data: "x"},
		},
		claveV6V4: {{Name: "::/3000", Data: "127.0.0.1/3000"}},
		// v4tov6 y v6tov6 no existen: nunca hubo reglas de esas clases.
	}}
}

func textos(rs []ports.ProxyRule) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Kind.String()+" "+r.String())
	}
	return out
}

func TestPortproxyLeerReglas(t *testing.T) {
	reg := registroDePrueba()
	reglas, ilegibles, err := leerReglas(reg.leer)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"v4tov4 0.0.0.0:3000 → 172.24.1.5:3000", "v4tov4 *:4000 → localhost:4000", "v4tov4 0.0.0.0:8080 → 172.24.1.5:80",
		"v6tov4 [::]:3000 → 127.0.0.1:3000",
	}
	if got := textos(reglas); !reflect.DeepEqual(got, want) {
		t.Errorf("reglas = %q", got)
	}
	if len(ilegibles) != 1 || ilegibles[0] != "v4tov4 esto no es una regla" {
		t.Errorf("ilegibles = %q", ilegibles)
	}
	if reg.lee != 4 {
		t.Errorf("se leyeron %d claves, esperaba las cuatro", reg.lee)
	}

	// Una máquina donde nunca se usó portproxy: ninguna clave, ninguna regla,
	// ningún error.
	vacio := &registroFalso{}
	if reglas, ilegibles, err := leerReglas(vacio.leer); err != nil || reglas != nil || ilegibles != nil {
		t.Errorf("sin claves: %v, %v, %v", reglas, ilegibles, err)
	}
	// Un error al leer invalida todo: con media lista, "no hay regla" podría
	// ser mentira.
	roto := registroDePrueba()
	roto.err = win.ErrAccessDenied
	if reglas, _, err := leerReglas(roto.leer); !errors.Is(err, win.ErrAccessDenied) || reglas != nil {
		t.Errorf("con error: %v, %v", reglas, err)
	}
}

// ipHelperID es el svchost que aloja al IP Helper en las pruebas.
func bosqueProxy() (*reaper.Forest, map[uint32][]reaper.Service, reaper.Proc) {
	host := svchostGenuino(2200)
	svc := map[uint32][]reaper.Service{2200: {{Name: "iphlpsvc"}, {Name: "Schedule"}}}
	return reaper.NewForest(conWindows(host)), svc, host
}

func TestPortproxyDesenmascara(t *testing.T) {
	forest, svc, host := bosqueProxy()
	reg := registroDePrueba()
	sys := &sistema{proxy: reg.cliente(false), admin: func() bool { return false }}
	socks := []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 3000, ports.Listen, 2200, "iphlpsvc"),
		sock(ports.TCP, "::", 3000, ports.Listen, 2200, "iphlpsvc"),
		sock(ports.TCP, "0.0.0.0", 4000, ports.Listen, 2200, "iphlpsvc"),
		sock(ports.TCP, "0.0.0.0", 5000, ports.Listen, 2200, "iphlpsvc"),  // escucha, pero no hay regla
		sock(ports.TCP, "0.0.0.0", 49665, ports.Listen, 2200, "Schedule"), // otro servicio del mismo host
	}
	pedidos := ports.Of(3000, 4000, 5000, 49665)
	plan := reaper.PlanKill(pedidos, socks, forest, svc, cfgC2)
	if len(plan.Targets) != 0 {
		t.Fatalf("antes de mirar, todo negado: %+v", plan.Targets)
	}
	plan = desenmascarar(context.Background(), sys, plan, socks, forest, opciones{})

	var objetivos []string
	for _, tg := range plan.Targets {
		if tg.Kind != reaper.StopExternal || tg.External.Kind != reaper.ExtRule || tg.Via != host.ID.Canon() || tg.ViaName != "svchost.exe" ||
			tg.ViaClass != reaper.RelayPortProxy || len(tg.Victims) != 0 || !pideAdmin(tg.External) {
			t.Errorf("objetivo = %+v", tg)
		}
		objetivos = append(objetivos, tg.External.Key+" "+tg.Ports.String()+" "+u32(uint32(len(tg.Sockets))))
	}
	// El 3000 lo tienen DOS reglas (una por familia): dos objetivos, cada uno
	// con su socket.
	want := []string{"portproxy/v4tov4/0.0.0.0/3000 3000 2", "portproxy/v6tov4/::/3000 3000 2", "portproxy/v4tov4/*/4000 4000 1"}
	if !reflect.DeepEqual(objetivos, want) {
		t.Errorf("objetivos = %q\n  esperaba %q", objetivos, want)
	}
	por := map[uint16]reaper.Refusal{}
	for _, r := range plan.Refusals {
		for p := range r.Ports.All() {
			por[p] = r
		}
	}
	if r := por[5000]; !strings.Contains(r.Reason, "ninguna regla de portproxy tiene ese puerto") || !strings.Contains(r.Reason, "1 regla que no pude interpretar") {
		t.Errorf("5000 = %+v", r)
	}
	if r := por[49665]; r.Service != "Schedule" || r.Relay != reaper.RelayNone {
		t.Errorf("49665 = %+v", r)
	}
	for _, r := range plan.Refusals {
		if strings.Contains(strings.ToLower(r.Advice), "net stop iphlpsvc") {
			t.Errorf("aconseja detener el IP Helper: %+v", r)
		}
	}
	if !planPideAdmin(plan) {
		t.Error("borrar una regla pide administrador: la pregunta lo tiene que avisar")
	}

	term := tui.Open()
	term.DisableColor()
	texto := strings.Join(bloques(term, sys, forest, plan, cfgC2, detallar(sys, plan, opciones{})), "\n")
	for _, dice := range []string{
		"regla de portproxy 0.0.0.0:3000 → 172.24.1.5:3000",
		"es persistente: está en el registro y vuelve en cada arranque",
		"se borra con `netsh interface portproxy delete v4tov4 listenport=3000 listenaddress=0.0.0.0 protocol=tcp` (pide administrador)",
		"se borra con `netsh interface portproxy delete v4tov4 listenport=4000 protocol=tcp` (pide administrador)",
		"Windows lo ve como svchost.exe (PID 2200): es el reenvío de puertos de Windows (portproxy), no se toca",
	} {
		if !strings.Contains(texto, dice) {
			t.Errorf("los bloques no dicen %q:\n%s", dice, texto)
		}
	}

	una := reaper.Plan{Targets: plan.Targets[2:]}
	if q := preguntaCierre(term, una, false, true); !strings.Contains(q, "¿Borrar la regla de portproxy *:4000 → localhost:4000 y liberar el 4000?") ||
		!strings.Contains(q, "(va a pedir permiso de administrador)") {
		t.Errorf("pregunta = %q", q)
	}

	b, _ := json.Marshal(construirJSONReporte(reaper.Report{}, plan, true))
	var doc struct {
		Objetivos []struct {
			Tipo  string `json:"tipo"`
			PID   uint32 `json:"pid"`
			Regla struct {
				Clase, Escucha, Destino, Borrar string
				Puerto                          uint16
			} `json:"regla"`
			Via struct {
				PID     uint32 `json:"pid"`
				Proceso string `json:"proceso"`
			} `json:"via"`
		} `json:"objetivos"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if o := doc.Objetivos; len(o) != 3 || o[0].Tipo != "regla" || o[0].PID != 0 || o[0].Via.PID != 2200 || o[0].Regla.Clase != "v4tov4" ||
		o[0].Regla.Escucha != "0.0.0.0" || o[0].Regla.Puerto != 3000 || o[0].Regla.Destino != "172.24.1.5:3000" ||
		o[0].Regla.Borrar != "netsh interface portproxy delete v4tov4 listenport=3000 listenaddress=0.0.0.0 protocol=tcp" {
		t.Errorf("JSON = %s", b)
	}
}

func TestPortproxySinRegistro(t *testing.T) {
	forest, svc, _ := bosqueProxy()
	reg := registroDePrueba()
	reg.err = win.ErrAccessDenied
	sys := &sistema{proxy: reg.cliente(false)}
	socks := []ports.Socket{sock(ports.TCP, "0.0.0.0", 3000, ports.Listen, 2200, "iphlpsvc")}
	plan := desenmascarar(context.Background(), sys, reaper.PlanKill(ports.Of(3000), socks, forest, svc, cfgC2), socks, forest, opciones{})
	if len(plan.Targets) != 0 || len(plan.Refusals) != 1 || !strings.Contains(plan.Refusals[0].Reason, "no pude leer las reglas") ||
		strings.Contains(strings.ToLower(plan.Refusals[0].Advice), "net stop iphlpsvc") {
		t.Fatalf("plan = %+v", plan)
	}
}

// Borrar una regla: solo como administrador, solo si sigue siendo la que vio
// el usuario, y se verifica que se fue.
func TestPortproxyBorrar(t *testing.T) {
	regla, _ := ports.ParseProxyRule(ports.V4ToV4, "0.0.0.0/3000", "172.24.1.5/3000")
	comodin, _ := ports.ParseProxyRule(ports.V4ToV4, "*/4000", "localhost/4000")
	const borrar3000 = "interface portproxy delete v4tov4 listenport=3000 listenaddress=0.0.0.0 protocol=tcp"

	casos := []struct {
		nombre  string
		armar   func(*registroFalso)
		elevado bool
		regla   ports.ProxyRule
		fin     reaper.Outcome
		metodo  reaper.Method
		dice    string
		pedidos []string
	}{
		{"sin administrador no se intenta nada", nil, false, regla, reaper.OutcomeDenied, reaper.MethodNone, "administrador", nil},
		{"como administrador", nil, true, regla, reaper.OutcomeClosed, reaper.MethodNetsh, "", []string{borrar3000}},
		{"la regla del comodín se borra sin nombrar la dirección", nil, true, comodin, reaper.OutcomeClosed, reaper.MethodNetsh, "",
			[]string{"interface portproxy delete v4tov4 listenport=4000 protocol=tcp"}},
		{"ya no estaba", func(r *registroFalso) { r.claves[claveV4V4] = r.claves[claveV4V4][:1] }, true, regla, reaper.OutcomeGone, reaper.MethodNone, "", nil},
		{"cambió de destino: es otra regla", func(r *registroFalso) { r.claves[claveV4V4][1].Data = "10.0.0.66/3000" }, true, regla,
			reaper.OutcomeError, reaper.MethodNone, "la regla cambió mientras tanto (ahora es 0.0.0.0:3000 → 10.0.0.66:3000)", nil},
		{"no se puede leer el registro", func(r *registroFalso) { r.err = errors.New("registro roto") }, true, regla,
			reaper.OutcomeError, reaper.MethodNone, "registro roto", nil},
		{"netsh dice que no", func(r *registroFalso) { r.falla = &errorNetsh{Codigo: 1, Dijo: "Acceso denegado."} }, true, regla,
			reaper.OutcomeError, reaper.MethodNone, "netsh: Acceso denegado.", []string{borrar3000}},
		{"netsh dice que sí y la regla sigue", func(r *registroFalso) { r.sordo = true }, true, regla,
			reaper.OutcomeError, reaper.MethodNone, "la regla sigue en el registro", []string{borrar3000}},
	}
	for _, c := range casos {
		reg := registroDePrueba()
		if c.armar != nil {
			c.armar(reg)
		}
		antes := len(reg.claves[claveV4V4]) + len(reg.claves[claveV6V4])
		res := reg.cliente(c.elevado).borrar(context.Background(), c.regla)
		if res.Outcome != c.fin || res.Method != c.metodo {
			t.Errorf("%s: %v / %v (%v)", c.nombre, res.Outcome, res.Method, res.Err)
		}
		if (c.dice == "") != (res.Err == nil) || (res.Err != nil && !strings.Contains(res.Err.Error(), c.dice)) {
			t.Errorf("%s: error = %v, esperaba %q", c.nombre, res.Err, c.dice)
		}
		if !reflect.DeepEqual(reg.pedidos, c.pedidos) {
			t.Errorf("%s: a netsh se le pidió %q, esperaba %q", c.nombre, reg.pedidos, c.pedidos)
		}
		// Se borró UNA regla, o ninguna: jamás otra.
		despues := len(reg.claves[claveV4V4]) + len(reg.claves[claveV6V4])
		if want := antes; (c.fin == reaper.OutcomeClosed && despues != antes-1) || (c.fin != reaper.OutcomeClosed && despues != want) {
			t.Errorf("%s: había %d valores y quedaron %d", c.nombre, antes, despues)
		}
		if c.fin == reaper.OutcomeClosed {
			if _, esta, _ := reg.cliente(true).buscar(c.regla); esta {
				t.Errorf("%s: la regla sigue", c.nombre)
			}
		}
	}
}

// Por el motor: sin administrador la regla queda denegada (y viaja en el
// trabajo elevado); como administrador se borra y se narra.
func TestPortproxyPorElMotor(t *testing.T) {
	for _, elevado := range []bool{false, true} {
		reg := registroDePrueba()
		sys := &sistema{proxy: reg.cliente(elevado), admin: func() bool { return elevado }}
		regla, _ := ports.ParseProxyRule(ports.V4ToV4, "0.0.0.0/3000", "172.24.1.5/3000")
		x := externoDeRegla(regla)
		var fin reaper.ExternalStop
		llamadas := 0
		sys.StopExternal(context.Background(), []reaper.External{x}, time.Now().Add(time.Second), false, func(i int, r reaper.ExternalStop) {
			llamadas++
			fin = r
		})
		if llamadas != 1 {
			t.Fatalf("elevado=%v: done se llamó %d veces", elevado, llamadas)
		}
		rep := reaper.Report{Externals: []reaper.ExternalResult{{External: x, Outcome: fin.Outcome, Method: fin.Method, Err: fin.Err}}}
		term := tui.Open()
		term.DisableColor()
		if elevado {
			if fin.Outcome != reaper.OutcomeClosed || necesitaElevacion(rep, nil) {
				t.Errorf("como administrador: %+v", fin)
			}
			l := lineaEvento(term, reaper.Event{Kind: reaper.EvExternalStopped, External: x, Outcome: fin.Outcome, Method: fin.Method, Elapsed: 120 * time.Millisecond}, narracion{admin: true})
			if !strings.Contains(l, "la regla de portproxy 0.0.0.0:3000 → 172.24.1.5:3000 quedó borrada") || !strings.Contains(l, "netsh") {
				t.Errorf("narración = %q", l)
			}
			if items, enSeco := itemsExternos(rep.Externals); len(items) != 1 || items[0].Label != "Reglas borradas" || enSeco != 0 {
				t.Errorf("tarjeta = %+v", items)
			}
			continue
		}
		if fin.Outcome != reaper.OutcomeDenied || len(reg.pedidos) != 0 || !necesitaElevacion(rep, nil) {
			t.Errorf("sin administrador: %+v, pedidos %q", fin, reg.pedidos)
		}
		// El trabajo elevado lleva la regla, con su destino.
		plan := reaper.Plan{Targets: []reaper.Target{{Kind: reaper.StopExternal, External: x, Ports: ports.Of(3000)}}}
		job := trabajoDe(plan, rep, nil, opciones{timeout: 3 * time.Second})
		if len(job.Reglas) != 1 || !job.Reglas[0].Same(regla) || len(job.Kill) != 0 {
			t.Fatalf("trabajo = %+v", job)
		}
		texto, err := codificarJob(job)
		if err != nil || !strings.Contains(texto, ";rule=v4tov4|0.0.0.0|3000|172.24.1.5/3000") {
			t.Errorf("trabajo = %q (%v)", texto, err)
		}
	}
	// Un contenedor denegado NO viaja: ser administrador no lo detiene mejor.
	cont := reaper.External{Kind: reaper.ExtContainer, Key: "abc", Name: "db-1", Ref: &contenedor{ID: "abc", Nombre: "db-1"}}
	rep := reaper.Report{Externals: []reaper.ExternalResult{{External: cont, Outcome: reaper.OutcomeDenied}}}
	if necesitaElevacion(rep, nil) {
		t.Error("un contenedor denegado no se arregla elevando")
	}
}

// El hijo elevado: del trabajo usa la selección; la regla la lee él del
// registro, y si ya no es la que el usuario vio, no pasa.
func TestPortproxyElevado(t *testing.T) {
	forest, svc, host := bosqueProxy()
	socks := []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 3000, ports.Listen, 2200, "iphlpsvc"),
		sock(ports.TCP, "0.0.0.0", 8080, ports.Listen, 2200, "iphlpsvc"),
		sock(ports.TCP, "0.0.0.0", 49665, ports.Listen, 2200, "Schedule"),
	}
	pedidos := ports.Of(3000, 8080, 49665)
	regla3000, _ := ports.ParseProxyRule(ports.V4ToV4, "0.0.0.0/3000", "172.24.1.5/3000")
	enRegistro, _, _ := leerReglas(registroDePrueba().leer)
	planear := func(job trabajo, reglas []ports.ProxyRule) reaper.Plan {
		job.Puertos = pedidos
		return planElevado(job, holding(socks, ports.Filter{}, pedidos), forest, svc, cfgC2, detras{reglas: reglas})
	}

	plan := planear(trabajo{Reglas: []ports.ProxyRule{regla3000}}, enRegistro)
	if len(plan.Targets) != 1 || plan.Targets[0].External.Key != "portproxy/v4tov4/0.0.0.0/3000" || plan.Targets[0].Via != host.ID.Canon() {
		t.Fatalf("objetivos = %+v", plan.Targets)
	}
	// La del 8080 está en el registro y tiene un socket, pero el trabajo no la
	// nombra: no se toca.
	for _, tg := range plan.Targets {
		if tg.Ports.Contains(8080) {
			t.Errorf("la regla del 8080 no se pidió: %+v", tg)
		}
	}

	otroDestino, _ := ports.ParseProxyRule(ports.V4ToV4, "0.0.0.0/3000", "10.0.0.66/3000")
	sinSocket, _ := ports.ParseProxyRule(ports.V4ToV4, "*/4000", "localhost/4000")
	inventada, _ := ports.ParseProxyRule(ports.V4ToV4, "0.0.0.0/49665", "1.2.3.4/80")
	for nombre, c := range map[string]struct {
		job    trabajo
		reglas []ports.ProxyRule
	}{
		"el trabajo dice otro destino que el registro":         {trabajo{Reglas: []ports.ProxyRule{otroDestino}}, enRegistro},
		"el registro cambió de destino":                        {trabajo{Reglas: []ports.ProxyRule{regla3000}}, []ports.ProxyRule{otroDestino}},
		"la regla ya no está en el registro":                   {trabajo{Reglas: []ports.ProxyRule{regla3000}}, nil},
		"la regla no tiene ningún socket":                      {trabajo{Reglas: []ports.ProxyRule{sinSocket}}, enRegistro},
		"una regla inventada sobre el puerto de otro servicio": {trabajo{Reglas: []ports.ProxyRule{inventada}}, append([]ports.ProxyRule{inventada}, enRegistro...)},
		"sin reglas en el trabajo":                             {trabajo{}, enRegistro},
		// El svchost jamás: ni nombrándolo como proceso a cerrar.
		"el svchost como proceso a cerrar": {trabajo{Kill: []reaper.ID{host.ID}}, enRegistro},
	} {
		if plan := planear(c.job, c.reglas); len(plan.Targets) != 0 {
			t.Errorf("%s: objetivos = %+v", nombre, plan.Targets)
		}
	}
}

func TestPortproxyEnElListado(t *testing.T) {
	forest, svc, _ := bosqueProxy()
	reg := registroDePrueba()
	sys := &sistema{proxy: reg.cliente(false)}
	socks := []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 3000, ports.Listen, 2200, "iphlpsvc"),
		sock(ports.TCP, "0.0.0.0", 5000, ports.Listen, 2200, "iphlpsvc"),
		sock(ports.TCP, "0.0.0.0", 49665, ports.Listen, 2200, "Schedule"),
		sock(ports.UDP, "::", 3544, ports.StateNone, 2200, "iphlpsvc"),
	}
	filas := anotarWindows(context.Background(), sys, filasDeListado(socks, forest, svc, cfgC2), forest, svc, cfgC2)
	muestra, ocultas := visibles(filas, mostrar{})
	if len(muestra) != 1 || ocultas.filas != 3 {
		t.Fatalf("visibles = %+v (ocultas %d)", muestra, ocultas.filas)
	}
	f := muestra[0]
	if f.Port != 3000 || nombreFila(f) != "portproxy" || f.Name != "svchost.exe" || f.Comando != "regla 0.0.0.0:3000 → 172.24.1.5:3000" || f.Via != "svchost.exe" ||
		f.Relay != reaper.RelayPortProxy || f.EsWindows || f.Regla == nil || f.Servicio != "" {
		t.Errorf("fila = %+v", f)
	}
	for _, f := range filas {
		if f.Port != 3000 && (f.Relay != reaper.RelayNone || !f.EsWindows || f.Regla != nil) {
			t.Errorf("fila del %d = %+v", f.Port, f)
		}
	}
	b, _ := json.Marshal(construirJSONListado(muestra, ocultas))
	for _, dice := range []string{`"intermediario":"portproxy"`, `"via":"svchost.exe"`, `"regla":{"clase":"v4tov4","escucha":"0.0.0.0","puerto":3000,"destino":"172.24.1.5:3000"`} {
		if !strings.Contains(string(b), dice) {
			t.Errorf("el JSON no dice %s:\n%s", dice, b)
		}
	}
	if strings.Contains(string(b), "otras_reglas") {
		t.Errorf("con una sola regla en el puerto no hay otras:\n%s", b)
	}

	// Dos reglas en el mismo puerto, una por familia (v4tov4 en 0.0.0.0 y
	// v6tov4 en [::]): la fila funde las dos familias, así que nombra la
	// primera y dice que hay otra; el JSON lleva las dos. Borrar solo la
	// primera no libera el puerto.
	socks = []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 3000, ports.Listen, 2200, "iphlpsvc"),
		sock(ports.TCP, "::", 3000, ports.Listen, 2200, "iphlpsvc"),
	}
	sys = &sistema{proxy: registroDePrueba().cliente(false)}
	filas = anotarWindows(context.Background(), sys, filasDeListado(socks, forest, svc, cfgC2), forest, svc, cfgC2)
	if len(filas) != 1 {
		t.Fatalf("dos familias del mismo puerto = %+v", filas)
	}
	if f := filas[0]; f.Comando != "regla 0.0.0.0:3000 → 172.24.1.5:3000 y 1 regla más" || len(f.OtrasReglas) != 1 ||
		f.OtrasReglas[0].Kind != ports.V6ToV4 || f.EsWindows {
		t.Errorf("fila con dos reglas = %+v", f)
	}
	b, _ = json.Marshal(construirJSONListado(filas, sinMostrar{}))
	if !strings.Contains(string(b), `"otras_reglas":[{"clase":"v6tov4","escucha":"::","puerto":3000,"destino":"127.0.0.1:3000"`) {
		t.Errorf("el JSON no lista la otra regla:\n%s", b)
	}
}
