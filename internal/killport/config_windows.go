//go:build windows

package killport

// config_windows.go: el flujo de las acciones de configuración de red
// (--reserve, --unreserve, --fix-dynamic). Lee el estado, arma el plan con el
// planificador puro (config.go), lo MUESTRA —cada paso con el comando exacto—
// y pregunta. Siempre pregunta: --yes no saltea esta pregunta y sin consola no
// se hace nada, porque lo que se cambia es de toda la máquina y uno de los
// pasos puede cortar la red de WSL y Docker.
//
// Si killport ya corre como administrador, ejecuta ahí mismo. Si no, el plan
// viaja al proceso elevado como pedido + huella (trabajo.go) y el hijo lo
// vuelve a armar antes de ejecutarlo. En los dos casos, lo que quedó se
// VERIFICA releyendo el sistema: las tablas de netsh y, para lo reservado, un
// bind de verdad.

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

// configReal es el configurador de verdad: netsh y el administrador de
// servicios. El único servicio que toca es WinNAT, y su nombre es una
// constante.
type configReal struct{ correr netshFn }

func (c configReal) detenerWinnat(ctx context.Context, deadline time.Time) error {
	return win.StopService(ctx, controladorWinnat, deadline)
}

func (c configReal) arrancarWinnat(ctx context.Context, deadline time.Time) error {
	return win.StartService(ctx, controladorWinnat, deadline)
}

func (c configReal) netsh(ctx context.Context, args ...string) (string, error) {
	return c.correr(ctx, args...)
}

// configurador devuelve quien ejecuta los pasos.
func (s *sistema) configurador() configurador {
	if s.config != nil {
		return s.config
	}
	return configReal{correr: s.red.netsh}
}

// estadoWinnat consulta si el controlador de WinNAT existe y si corre.
func estadoWinnat() (existe, corriendo bool, err error) {
	st, err := win.QueryServiceState(controladorWinnat)
	switch {
	case errors.Is(err, win.ErrServiceNotFound):
		return false, false, nil
	case err != nil:
		return false, false, err
	}
	return true, st.Running(), nil
}

// leerEstadoRed lee lo que el planificador necesita, de nuevo (no usa lo
// memoizado: decide sobre lo que hay AHORA).
func leerEstadoRed(ctx context.Context, sys *sistema) (estadoRed, error) {
	sys.red.olvidar()
	l := sys.red.leer()
	if l.errExcl != nil {
		return estadoRed{}, errors.New("no pude leer los rangos excluidos: " + l.errExcl.Error())
	}
	socks, err := sys.Sockets(ctx)
	if err != nil {
		return estadoRed{}, errors.New("no pude leer las tablas de red: " + err.Error())
	}
	e := estadoRed{Excl: l.excl, Dyn: l.dyn, Ocupados: ocupadosDe(socks)}
	if e.WinnatExiste, e.WinnatCorriendo, err = sys.estadoWinnat(); err != nil {
		return estadoRed{}, errors.New("no pude consultar el estado de WinNAT: " + err.Error())
	}
	return e, nil
}

// pedidoDe arma el pedido a partir de los flags y los puertos.
func pedidoDe(requested ports.Set, o opciones) pedidoConfig {
	ped := pedidoConfig{Dinamico: o.fixDynamic, TCP: o.tcp, UDP: o.udp}
	switch {
	case o.reserve:
		ped.Reservar = requested
	case o.unreserve:
		ped.Liberar = requested
	}
	return ped
}

// validarConfig revisa la línea de comandos de una acción de configuración.
func validarConfig(pos []string, o opciones) error {
	switch {
	case o.reserve && o.unreserve:
		return errors.New("--reserve y --unreserve no van juntos: elegí uno")
	case (o.reserve || o.unreserve) && len(pos) == 0:
		return errors.New("decime qué puertos: killport 3000 --reserve")
	case !o.reserve && !o.unreserve && len(pos) > 0:
		return errors.New("--fix-dynamic no lleva puertos: cambia el rango dinámico de toda la máquina")
	}
	// Qué flags van con la configuración lo decide validarModos.
	return validarModos(pos, o)
}

// correrConfig es el comando de configuración: mostrar el plan, preguntar,
// hacer y verificar.
func correrConfig(t *tui.Term, sys *sistema, requested ports.Set, o opciones) int {
	ped := pedidoDe(requested, o)
	if err := ped.validar(); err != nil {
		return falloUsage(t, err)
	}
	ctx, stop := contexto()
	defer stop()

	estado, err := leerEstadoRed(ctx, sys)
	if err != nil {
		return fallo(t, err)
	}
	plan := planificarConfig(estado, ped)
	admin := sys.esAdmin()

	if o.json {
		// Para scripts: el plan, sin ejecutarlo (no hay a quién preguntarle).
		// El código es el del texto en la misma situación: un plan que no se
		// puede hacer (un impedimento) sale 3; uno que había que hacer y no se
		// hizo (sin --list: se pidió hacerlo, y sin consola no se confirma, ni
		// con --yes), también 3; solo mirar o nada que hacer, 0.
		escribirJSON(t, configJSON(ped, plan, nil, nil, !admin, true))
		if len(plan.Impedimentos) > 0 || (!o.list && len(plan.Pasos) > 0) {
			return cli.ExitFailure
		}
		return cli.ExitOK
	}

	t.Lines(t.Header("killport", tituloConfig(ped), o.version))
	t.Lines(bloqueConfig(t, estado, ped, plan))
	switch {
	case len(plan.Impedimentos) > 0:
		t.Line(t.Status(tui.Fail, "No cambié nada", "no se hace la mitad de un pedido"))
		t.Blank()
		return cli.ExitFailure
	case len(plan.Pasos) == 0:
		t.Line(t.Status(tui.OK, "No hay nada que hacer", ""))
		t.Blank()
		return cli.ExitOK
	case o.list:
		t.Line(t.Hint("solo mostré el plan; sin --list lo hago (pregunto antes)"))
		t.Blank()
		return cli.ExitOK
	case !t.CanPrompt():
		t.Line(t.Status(tui.Warn, "No cambié nada", "esto configura la red de toda la máquina: hace falta una consola para confirmarlo (--yes no alcanza)"))
		t.Blank()
		return cli.ExitFailure
	}

	pregunta := "¿Hacerlo?"
	aviso := ""
	if !admin {
		aviso = " (va a pedir permiso de administrador)"
	}
	resp, ok := t.Prompt(t.Paint(tui.Lavender, pregunta) + t.Paint(tui.Faint, aviso) + t.Paint(tui.Subtle, " [s/N] "))
	if !ok || !esSi(resp) {
		t.Blank()
		t.Line(t.Status(tui.Skip, "No cambié nada", "cancelaste"))
		t.Blank()
		return cli.ExitInterrupted
	}
	t.Blank()
	return hacerConfig(t, ctx, sys, ped, plan)
}

// hacerConfig ejecuta un plan YA CONFIRMADO por el usuario, verifica cómo
// quedó el sistema y lo cuenta. Devuelve el código de salida.
func hacerConfig(t *tui.Term, ctx contextoT, sys *sistema, ped pedidoConfig, plan planConfig) int {
	var res []resultadoPaso
	sinInforme := false
	if sys.esAdmin() {
		// Cada paso se cuenta apenas termina; los que no se llegaron a
		// intentar, al final.
		res = ejecutarConfig(ctx, sys.configurador(), plan, time.Now, func(r resultadoPaso) {
			if l := lineaPaso(t, r); l != "" {
				t.Line(l)
			}
		})
		for _, r := range res {
			if r.Fin == pasoSalteado {
				t.Line(lineaPaso(t, r))
			}
		}
	} else {
		var falla string
		var ok bool
		res, falla, ok = configElevada(t, ctx, sys, ped, plan)
		if !ok {
			t.Blank()
			return cli.ExitInterrupted
		}
		switch {
		case res == nil:
			// No se sabe qué hizo: no dejó su informe (lo lanzó otro usuario, o
			// se cortó). Lo que sigue es mirar cómo quedó el sistema.
			sinInforme = true
			t.Line(t.Status(tui.Warn, "El proceso elevado no dejó su informe", "miro cómo quedó el sistema"))
		case falla != "":
			t.Line(t.Status(tui.Fail, "El proceso elevado no hizo nada", falla))
		default:
			for _, r := range res {
				t.Line(lineaPaso(t, r))
			}
		}
	}

	// Lo que quedó se mira, diga lo que diga quien lo hizo.
	despues, err := leerEstadoRed(ctx, sys)
	if err != nil {
		t.Line(t.Status(tui.Warn, "No pude verificar cómo quedó", err.Error()))
		t.Blank()
		return cli.ExitFailure
	}
	comprobado := append(verificarConfig(despues, plan, ped), sondearReservas(sys, ped)...)
	bien, pedidoBien, pedidoTotal, sano := 0, 0, 0, true
	for _, k := range comprobado {
		if !k.Salud {
			pedidoTotal++
		}
		if k.Cumple {
			bien++
			if !k.Salud {
				pedidoBien++
			}
			t.Line(t.Status(tui.OK, k.Que, ""))
		} else {
			sano = sano && !k.Salud
			t.Line(t.Status(tui.Fail, "No quedó: "+k.Que, k.Porque))
		}
	}
	switch d := deshacerHechos(plan, res); {
	case bien == len(comprobado) && len(plan.Deshacer) > 0:
		t.Line(t.Hint("se deshace con: " + strings.Join(plan.Deshacer, "  ·  ")))
	case bien < len(comprobado) && len(d) > 0:
		// Quedó a medias: lo que sí se hizo es persistente.
		t.Line(t.Hint("lo que sí quedó hecho se deshace con: " + strings.Join(d, "  ·  ")))
	}
	pasos := tui.Item{Label: "Pasos hechos", Value: strconv.Itoa(hechos(res)) + " de " + strconv.Itoa(len(plan.Pasos)), Dot: colorDe(hechos(res), len(plan.Pasos))}
	if sinInforme {
		pasos = tui.Item{Label: "Pasos hechos", Value: "sin informe", Dot: tui.Peach}
	}
	t.Lines(t.Card("killport", []tui.Item{pasos,
		{Label: "Verificado", Value: strconv.Itoa(bien) + " de " + strconv.Itoa(len(comprobado)), Dot: colorDe(bien, len(comprobado))},
	}))
	switch {
	case pedidoBien == pedidoTotal && sano:
		return cli.ExitOK
	case pedidoBien == 0:
		return cli.ExitFailure
	}
	return cli.ExitPartial
}

func hechos(res []resultadoPaso) int {
	n := 0
	for _, r := range res {
		if r.Fin == pasoHecho {
			n++
		}
	}
	return n
}

func colorDe(bien, total int) tui.Color {
	switch {
	case bien == total:
		return tui.Sage
	case bien == 0:
		return tui.Rose
	}
	return tui.Peach
}

// maxSondeo acota cuántos puertos reservados se prueban con un bind.
const maxSondeo = 64

// sondearReservas prueba, con un bind de verdad, que cada puerto reservado se
// deja atar. Es lo que importa: que el servidor del usuario lo pueda tomar.
// Un puerto en uso también cuenta (lo tiene alguien: no está reservado por
// Windows).
func sondearReservas(sys *sistema, ped pedidoConfig) []comprobacion {
	if ped.Reservar.IsEmpty() || ped.Reservar.Len() > maxSondeo {
		return nil
	}
	sondeo := sys.ProbeBind // el de las pruebas, si lo fijaron
	var out []comprobacion
	for _, c := range enCombos(ped.protocolos(ports.TCP)) {
		negados := ports.Of()
		for p := range ped.Reservar.All() {
			if sondeo(c.proto, c.fam, p) == ports.BindForbidden {
				negados = negados.Union(ports.Of(p))
			}
		}
		k := comprobacion{Que: ped.Reservar.String() + " " + c.String() + " se deja atar", Cumple: negados.IsEmpty()}
		if !k.Cumple {
			k.Porque = "Windows le sigue negando el bind a " + negados.String()
		}
		out = append(out, k)
	}
	return out
}

// configElevada manda el plan al proceso elevado y devuelve lo que informó.
// ok=false si no hubo proceso elevado (el usuario dijo que no en el UAC, o
// cortó). res es nil si el hijo corrió y no dejó su informe: no se sabe qué
// hizo. falla es lo que el hijo dijo si no llegó a ejecutar nada.
func configElevada(t *tui.Term, ctx contextoT, sys *sistema, ped pedidoConfig, plan planConfig) (res []resultadoPaso, falla string, ok bool) {
	job := trabajo{Clase: trabajoConfig, Pedido: ped, Huella: plan.huella()}
	texto, err := codificarJob(job)
	if err != nil {
		t.Line(t.Status(tui.Warn, "No pude preparar el pedido de administrador", err.Error()))
		return nil, "", false
	}
	resultPath, err := prepararResultado()
	if err != nil {
		t.Line(t.Status(tui.Warn, "No pude preparar el pedido de administrador", err.Error()))
		return nil, "", false
	}
	defer removerResultado(resultPath)

	h, err := win.ShellExecuteProcess("runas", sys.exe, []string{"--yes", "--elevated-job", texto, "--result", resultPath}, true)
	switch {
	case errors.Is(err, win.ErrUACDeclined):
		t.Line(t.Status(tui.Skip, "No cambié nada", "cancelaste el pedido de administrador"))
		return nil, "", false
	case err != nil:
		t.Line(t.Status(tui.Warn, "No pude elevar", err.Error()))
		return nil, "", false
	case h == nil:
		return nil, "", false
	}
	defer h.Close()
	t.Line(t.Status(tui.Step, "Haciéndolo como administrador…", ""))
	if _, r, _ := win.WaitAny(ctx, []*win.Handle{h}, time.Time{}); r == win.WaitCanceled {
		t.Line(t.Status(tui.Skip, "Cancelaste", "el proceso elevado termina lo que empezó: si detuvo WinNAT, lo vuelve a arrancar"))
		return nil, "", false
	}
	doc, err := leerResultadoConfig(resultPath)
	if err != nil {
		return nil, "", true
	}
	res, falla = pasosInformados(plan, doc)
	return res, falla, true
}

// pasosInformados acomoda lo que informó el hijo sobre los pasos del plan que
// el usuario confirmó. Del informe se toma, paso por paso, cómo terminó y
// cuánto tardó; QUÉ pasos son no sale del informe sino del plan. Si el hijo
// informa otra cantidad de pasos, u otra clase en algún lugar, de ese paso no
// se le cree nada. Los textos del informe se limpian antes de mostrarlos.
func pasosInformados(plan planConfig, doc jsonConfig) (res []resultadoPaso, falla string) {
	res = make([]resultadoPaso, len(plan.Pasos))
	for i, p := range plan.Pasos {
		res[i] = resultadoPaso{Paso: p, Fin: pasoSalteado}
	}
	if len(doc.Pasos) == len(plan.Pasos) {
		for i, jp := range doc.Pasos {
			if jp.Clase != plan.Pasos[i].Clase.clave() {
				continue
			}
			switch jp.Resultado {
			case pasoHecho.clave():
				res[i].Fin = pasoHecho
			case pasoFallo.clave():
				res[i].Fin, res[i].Err = pasoFallo, errors.New(textoAjeno(jp.Error))
			}
			if jp.MS > 0 && jp.MS < time.Hour.Milliseconds() {
				res[i].Tardo = time.Duration(jp.MS) * time.Millisecond
			}
		}
	}
	return res, textoAjeno(doc.Error)
}

// correrConfigElevado es el hijo elevado de una acción de configuración.
// Vuelve a leer el sistema, vuelve a planificar y ejecuta SOLO si el plan que
// le sale es el que el usuario confirmó.
func correrConfigElevado(ctx contextoT, sys *sistema, job trabajo, resultPath string) int {
	informar := func(plan planConfig, res []resultadoPaso, falla string) int {
		doc := configJSON(job.Pedido, plan, res, nil, false, len(res) == 0)
		doc.Error = falla
		escribirResultado(resultPath, doc)
		if falla != "" || !todoHecho(res) {
			return cli.ExitFailure
		}
		return cli.ExitOK
	}
	if !sys.esAdmin() {
		return informar(planConfig{}, nil, "el proceso no corre como administrador")
	}
	estado, err := leerEstadoRed(ctx, sys)
	if err != nil {
		return informar(planConfig{}, nil, err.Error())
	}
	plan := planificarConfig(estado, job.Pedido)
	switch {
	case plan.huella() != job.Huella:
		return informar(plan, nil, "la red cambió desde que confirmaste el plan: no hice nada; volvé a correr killport")
	case !plan.ejecutable():
		return informar(plan, nil, "no hay nada que hacer")
	}
	return informar(plan, ejecutarConfig(ctx, sys.configurador(), plan, time.Now, nil), "")
}

// --- lo que se muestra -------------------------------------------------------

func tituloConfig(ped pedidoConfig) string {
	switch {
	case !ped.Reservar.IsEmpty() && ped.Dinamico:
		return "reservar puertos y sanear el rango dinámico"
	case !ped.Reservar.IsEmpty():
		return "reservar puertos para vos"
	case !ped.Liberar.IsEmpty() && ped.Dinamico:
		return "quitar reservas y sanear el rango dinámico"
	case !ped.Liberar.IsEmpty():
		return "quitar reservas de puertos"
	}
	return "devolver el rango dinámico al de fábrica"
}

// hoyConfig cuenta cómo está HOY lo que se pidió cambiar: por qué hace falta.
func hoyConfig(e estadoRed, ped pedidoConfig) []string {
	var out []string
	pedidos := ped.Reservar.Union(ped.Liberar)
	if !pedidos.IsEmpty() {
		for _, c := range enCombos(ped.protocolos(ports.TCP)) {
			var partes []string
			for _, x := range e.Excl {
				if x.Proto != c.proto || x.Family != c.fam || ports.NewSet(x.Range).Intersect(pedidos).IsEmpty() {
					continue
				}
				if x.Administered {
					partes = append(partes, "reservado (exclusión administrada "+x.Range.String()+")")
				} else {
					partes = append(partes, "tapado por el bloque "+x.Range.String()+" de WinNAT")
				}
			}
			if uso := pedidos.Intersect(e.Ocupados[c]); !uso.IsEmpty() {
				partes = append(partes, "en uso ("+uso.String()+")")
			}
			if len(partes) == 0 {
				partes = []string{"sin reservas"}
			}
			out = append(out, c.String()+": "+strings.Join(partes, ", "))
		}
	}
	if ped.Dinamico {
		for _, c := range enCombos(ped.protocolos(ports.TCP, ports.UDP)) {
			d, ok := e.Dyn.Find(c.proto, c.fam)
			switch {
			case !ok:
				out = append(out, "rango dinámico "+c.String()+": no se pudo leer")
			case d.Default():
				out = append(out, "rango dinámico "+c.String()+": "+d.Range().String()+" (el de fábrica)")
			default:
				out = append(out, "rango dinámico "+c.String()+": "+d.Range().String())
			}
		}
	}
	return out
}

// bloqueConfig muestra el pedido, cómo está hoy y el plan, paso por paso con
// el comando exacto que se va a correr.
func bloqueConfig(t *tui.Term, e estadoRed, ped pedidoConfig, plan planConfig) []string {
	que := ""
	switch {
	case !ped.Reservar.IsEmpty():
		que = "reservar " + ped.Reservar.String()
	case !ped.Liberar.IsEmpty():
		que = "quitar la reserva de " + ped.Liberar.String()
	}
	if ped.Dinamico {
		if que != "" {
			que += " · "
		}
		que += "rango dinámico a 49152–65535"
	}
	out := []string{"", tui.Margin + t.Paint(tui.Accent(0), "●") + " " + t.Paint(tui.Text, que)}
	if len(plan.Pasos) > 0 || len(plan.Hechos) == 0 {
		// Sin nada que hacer, lo que ya está lo dicen los "↷" de abajo: el
		// "hoy" repetiría cada dato (X-40).
		for _, l := range hoyConfig(e, ped) {
			out = append(out, tui.Margin+"    "+t.Paint(tui.Faint, "hoy · "+l))
		}
	}
	if len(plan.Pasos) > 0 {
		ancho := 0
		for _, p := range plan.Pasos {
			ancho = max(ancho, tui.Width(p.texto()))
		}
		out = append(out, "", tui.Margin+t.Paint(tui.Subtle, "Plan"))
		for i, p := range plan.Pasos {
			n := tui.Margin + "  " + t.Paint(tui.Peach, strconv.Itoa(i+1)+".") + " "
			linea := n + t.Paint(tui.Text, tui.PadRight(p.texto(), ancho)) + "  " + t.Paint(tui.Faint, p.comando())
			if t.Bounded() && tui.Width(linea) > t.Width() {
				// El comando nunca se parte: va entero en su renglón, debajo.
				out = append(out, n+t.Paint(tui.Text, p.texto()), tui.Margin+"     "+t.Paint(tui.Faint, p.comando()))
				continue
			}
			out = append(out, linea)
		}
	}
	out = append(out, "")
	for _, h := range plan.Hechos {
		out = append(out, t.Status(tui.Skip, h, ""))
	}
	for _, a := range plan.Avisos {
		out = append(out, estado(t, tui.Warn, "Ojo", a)...)
	}
	for _, x := range plan.Impedimentos {
		out = append(out, estado(t, tui.Fail, "No se puede", x)...)
	}
	if len(plan.Pasos) > 0 && len(plan.Deshacer) > 0 {
		out = append(out, t.Hint("se deshace con: "+strings.Join(plan.Deshacer, "  ·  ")))
	}
	return append(out, "")
}

// lineaPaso narra cómo terminó un paso ("" si todavía no terminó: corren en
// décimas de segundo, no vale la pena anunciar que empiezan).
func lineaPaso(t *tui.Term, r resultadoPaso) string {
	switch r.Fin {
	case pasoHecho:
		return t.Status(tui.OK, mayuscula(r.Paso.texto()), tui.Duration(r.Tardo))
	case pasoFallo:
		detalle := ""
		if r.Err != nil {
			detalle = r.Err.Error()
		}
		return t.Status(tui.Fail, "No pude "+r.Paso.texto(), detalle)
	case pasoSalteado:
		return t.Status(tui.Skip, "No llegué a "+r.Paso.texto(), "")
	}
	return ""
}

func mayuscula(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// --- JSON ---------------------------------------------------------------------

// jsonConfig es el documento de una acción de configuración: con --json, el
// plan (no se ejecuta: no hay a quién preguntarle); en el archivo de
// resultado del proceso elevado, además, cómo le fue a cada paso.
type jsonConfig struct {
	Reservar     []uint16           `json:"reservar,omitempty"`
	Quitar       []uint16           `json:"quitar_reserva,omitempty"`
	Dinamico     bool               `json:"rango_dinamico,omitempty"`
	Pasos        []jsonPaso         `json:"pasos"`
	Hechos       []string           `json:"ya_estaba"`
	Avisos       []string           `json:"avisos"`
	Impedimentos []string           `json:"impedimentos"`
	Deshacer     []string           `json:"deshacer"`
	PideAdmin    bool               `json:"pide_admin"`
	SoloPlan     bool               `json:"solo_plan"` // true si no se ejecutó nada
	Huella       string             `json:"huella"`
	Comprobado   []jsonComprobacion `json:"comprobado,omitempty"`
	// deshacer_hecho: si no se hicieron todos los pasos, los comandos que
	// deshacen los que sí (una reserva a medias es persistente).
	DeshacerHecho []string `json:"deshacer_hecho,omitempty"`
	Error         string   `json:"error,omitempty"`
}

type jsonPaso struct {
	Clase     string `json:"clase"` // detener-winnat | reservar | quitar-reserva | rango-dinamico | arrancar-winnat
	Que       string `json:"que"`
	Comando   string `json:"comando"`
	Resultado string `json:"resultado,omitempty"` // hecho | error | salteado
	Error     string `json:"error,omitempty"`
	MS        int64  `json:"ms,omitempty"`
}

type jsonComprobacion struct {
	Que    string `json:"que"`
	Cumple bool   `json:"cumple"`
	Porque string `json:"porque,omitempty"`
}

func noNulo(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func configJSON(ped pedidoConfig, plan planConfig, res []resultadoPaso, comprobado []comprobacion, pideAdmin, soloPlan bool) jsonConfig {
	doc := jsonConfig{Dinamico: ped.Dinamico, Pasos: []jsonPaso{}, Hechos: noNulo(plan.Hechos), Avisos: noNulo(plan.Avisos),
		Impedimentos: noNulo(plan.Impedimentos), Deshacer: noNulo(plan.Deshacer), PideAdmin: pideAdmin, SoloPlan: soloPlan, Huella: plan.huella()}
	if !ped.Reservar.IsEmpty() {
		doc.Reservar = listaPuertos(ped.Reservar)
	}
	if !ped.Liberar.IsEmpty() {
		doc.Quitar = listaPuertos(ped.Liberar)
	}
	for i, p := range plan.Pasos {
		jp := jsonPaso{Clase: p.Clase.clave(), Que: p.texto(), Comando: p.comando()}
		if i < len(res) {
			jp.Resultado = res[i].Fin.clave()
			jp.MS = res[i].Tardo.Milliseconds()
			if res[i].Err != nil {
				jp.Error = res[i].Err.Error()
			}
		}
		doc.Pasos = append(doc.Pasos, jp)
	}
	for _, k := range comprobado {
		doc.Comprobado = append(doc.Comprobado, jsonComprobacion{Que: k.Que, Cumple: k.Cumple, Porque: k.Porque})
	}
	if res != nil && hechos(res) < len(plan.Pasos) {
		doc.DeshacerHecho = deshacerHechos(plan, res)
	}
	return doc
}
