//go:build windows

package killport

// elevacion.go: el flujo de administrador. Si tras la escalada quedó algo que
// solo un administrador puede hacer —un proceso con acceso denegado, un
// servicio, una regla de portproxy, un servicio que hay que dejar en manual— y
// killport no corre elevado, se ofrece a reintentar elevado. El trabajo viaja
// POR ARGUMENTOS (ver trabajo.go: qué viaja y por qué no es una lista de
// órdenes). El resultado vuelve por un archivo que el hijo elevado crea con
// CreateNewExact (nuevo, sin seguir enlaces, con la ruta verificada), y que
// solo sirve para CONTAR qué se intentó: qué murió, qué regla se borró, cómo
// quedó cada servicio y qué puertos quedaron libres lo mira el padre por su
// cuenta, con o sin informe.
//
// Este archivo es la segunda mitad de la defensa del proceso elevado: todo lo
// que llega por argumentos se CONTRASTA CON EL SISTEMA antes de tocar nada. El
// hijo toma su propia foto, arma su propio plan con las reglas de siempre, y
// del trabajo usa solamente la selección: de lo que el plan permite, qué
// hacer. Y la ruta del informe tampoco se obedece sin más: el hijo escribe
// solo en sus propias carpetas de trabajo y con un nombre que no elige nadie
// (ver rutaResultadoValida): un proceso con token de administrador no crea
// archivos donde le digan.
//
// El clic del UAC no se puede automatizar; esa parte queda para una prueba
// manual (docs/VERIFICACION.md › killport › "Pendiente manual", con
// _oracle/ADMIN.md y admin.ps1). La plomería —codificar y decodificar el trabajo,
// crear el archivo seguro, lanzar con "runas", esperar y leer el resultado—
// está probada de punta a punta con "open" en internal/win.

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

// necesitaElevacion dice si quedó algo que un administrador sí podría hacer:
// una víctima con acceso denegado, un externo que pide administrador (una
// regla de portproxy), o un servicio que no se pudo dejar en manual.
func necesitaElevacion(rep reaper.Report, manuales []resultadoManual) bool {
	for _, v := range rep.Victims {
		if v.Outcome == reaper.OutcomeDenied {
			return true
		}
	}
	for _, x := range rep.Externals {
		if x.Outcome == reaper.OutcomeDenied && pideAdmin(x.External) {
			return true
		}
	}
	for _, m := range manuales {
		if m.Fin == manualDenegado && m.Elevable {
			return true
		}
	}
	return false
}

// planPideAdmin dice si el plan tiene objetivos que se sabe de antemano que
// necesitan administrador: la pregunta lo avisa antes de que el usuario
// confirme, y si confirma no se le vuelve a preguntar.
func planPideAdmin(plan reaper.Plan) bool {
	for _, tg := range plan.Targets {
		switch {
		case tg.Kind == reaper.StopThirdSvc:
			return true
		case tg.Kind == reaper.StopExternal && pideAdmin(tg.External):
			return true
		}
	}
	return false
}

// intentarElevar ofrece (o, con --yes o si la pregunta ya lo había avisado,
// lanza directo) el reintento como administrador, espera al hijo elevado, y
// RE-VERIFICA por su cuenta. Devuelve el reporte y los resultados de --manual
// actualizados; si el usuario rechaza el UAC o dice que no, los originales.
func intentarElevar(t *tui.Term, ctx contextoT, sys *sistema, forest *reaper.Forest, requested ports.Set, plan reaper.Plan,
	cfg reaper.Config, opts reaper.Options, o opciones, rep reaper.Report, manuales []resultadoManual, avisado bool) (reaper.Report, []resultadoManual) {
	if !o.yes && !avisado {
		if !t.CanPrompt() {
			t.Line(t.Hint("quedó algo que pide administrador; corré killport como administrador"))
			return rep, manuales
		}
		resp, ok := t.Prompt(t.Paint(tui.Lavender, "¿Reintentar como administrador?") + t.Paint(tui.Subtle, " [s/N] "))
		if !ok || !esSi(resp) {
			return rep, manuales
		}
	}

	job := trabajoDe(plan, rep, manuales, o)
	elevado, ok := lanzarElevado(t, ctx, sys, job, &rep.Canceled)
	if !ok {
		return rep, manuales
	}
	// Lo que dice el hijo sirve para saber qué intentó; lo que quedó, se mira.
	var despues *reaper.Forest
	if procs, err := sys.Snapshot(ctx); err == nil {
		despues = reaper.NewForest(procs)
	}
	rep.Victims = fusionarVictimas(rep.Victims, elevado, despues)
	rep.Externals = fusionarExternos(sys, rep.Externals, elevado)
	manuales = fusionarManual(sys, manuales, elevado, cfg.SystemRoot)
	sys.red.olvidar()
	rep.Ports = reaper.Verify(ctx, sys, forest, requested, plan, rep, opts)
	return rep, manuales
}

// lanzarElevado lanza el trabajo por el UAC, espera al hijo y lee lo que dejó.
// ok=false si no hubo hijo que esperar (el usuario dijo que no, o no se pudo
// lanzar) o si se canceló la espera. cancelado, si no es nil, se enciende
// cuando el usuario cortó con Ctrl+C.
func lanzarElevado(t *tui.Term, ctx contextoT, sys *sistema, job trabajo, cancelado *bool) (jsonReporte, bool) {
	var vacio jsonReporte
	texto, err := codificarJob(job)
	if err != nil {
		t.Line(t.Status(tui.Warn, "No pude preparar el reintento elevado", err.Error()))
		return vacio, false
	}
	resultPath, err := prepararResultado()
	if err != nil {
		t.Line(t.Status(tui.Warn, "No pude preparar el reintento elevado", err.Error()))
		return vacio, false
	}
	defer removerResultado(resultPath)

	args := []string{"--yes", "--elevated-job", texto, "--result", resultPath}
	h, err := win.ShellExecuteProcess("runas", sys.exe, args, true)
	switch {
	case errors.Is(err, win.ErrUACDeclined):
		t.Line(t.Status(tui.Skip, "Cancelaste el pedido de administrador", ""))
		return vacio, false
	case err != nil:
		t.Line(t.Status(tui.Warn, "No pude elevar", err.Error()))
		return vacio, false
	case h == nil:
		return vacio, false
	}
	defer h.Close()
	t.Lines([]string{"", t.Status(tui.Step, "Haciéndolo como administrador…", "")})
	if _, res, _ := win.WaitAny(ctx, []*win.Handle{h}, time.Time{}); res == win.WaitCanceled {
		if cancelado != nil {
			*cancelado = true
		}
		return vacio, false
	}
	doc, err := leerResultado(resultPath)
	if err != nil {
		// Sin resultado no se sabe qué intentó el hijo; lo que quedó se va a
		// mirar igual.
		return vacio, true
	}
	return doc, true
}

// correrElevado es el proceso hijo elevado: hace el trabajo y escribe el
// resultado. No pregunta, no se vuelve a elevar, no imprime (corre oculto).
func correrElevado(sys *sistema, o opciones) int {
	job, err := decodificarJob(o.elevatedJob)
	if err != nil {
		return cli.ExitUsage
	}
	ctx, stop := contexto()
	defer stop()
	if job.Clase == trabajoConfig {
		return correrConfigElevado(ctx, sys, job, o.resultPath)
	}

	o.tree, o.force, o.tcp, o.udp, o.allStates, o.timeout = job.Tree, job.Force, job.TCP, job.UDP, job.AllStates, job.Timeout
	forest, socks, svc, cfg, err := recolectar(ctx, sys, o)
	if err != nil {
		return cli.ExitFailure
	}
	hold := holding(socks, o.filtro(), job.Puertos)
	plan := planElevado(job, hold, forest, svc, cfg, mirarElevado(ctx, sys, job, hold, forest, svc, cfg))

	opts := reaper.Options{Timeout: job.Timeout, Filter: o.filtro(), Settle: settleWindow}
	rep := reaper.Run(ctx, sys, forest, job.Puertos, plan, cfg, opts)

	doc := construirJSONReporte(rep, reaper.Plan{}, false)
	doc.Manual = manualJSON(manualElevado(sys, job, plan, rep, cfg))
	escribirResultado(o.resultPath, doc)
	return codigoDeSalida(rep)
}

// manualElevado es el --manual del hijo elevado. Que el trabajo nombre un
// servicio no alcanza para cambiarle el inicio: tiene que ser uno que ESTA
// corrida detuvo —un objetivo del plan que armó el hijo, o sea que tenía uno
// de los puertos pedidos cuando él miró—. Sin esa atadura, un trabajo armado
// por cualquiera dejaría en manual a cualquier servicio de terceros que
// estuviera detenido. Recién después valen las reglas de dejarEnManual (de
// terceros, detenido, nunca deshabilitar).
func manualElevado(sys *sistema, job trabajo, plan reaper.Plan, rep reaper.Report, cfg reaper.Config) []resultadoManual {
	detuvo := map[string]string{} // en minúsculas → el nombre como lo tiene el plan
	for _, s := range serviciosDetenidos(plan, rep) {
		detuvo[strings.ToLower(s)] = s
	}
	var out []resultadoManual
	for _, nombre := range job.Manual {
		suyo, ok := detuvo[strings.ToLower(nombre)]
		if !ok {
			out = append(out, resultadoManual{Servicio: nombre, Fin: manualNoDetenido,
				Motivo: "el proceso elevado no lo detuvo: no le cambio el inicio a un servicio que esta corrida no detuvo"})
			continue
		}
		// Del trabajo sale la selección; el nombre, del plan del hijo.
		out = append(out, sys.svc.dejarEnManual(suyo, cfg.SystemRoot))
	}
	return out
}

// detras es lo que el hijo elevado LEYÓ del sistema para mirar detrás de los
// intermediarios de Windows: las reglas de portproxy que hay en el registro y
// las colas de http.sys. Lo lee él, con sus ojos de administrador; del trabajo
// no sale ni una regla ni una URL.
type detras struct {
	reglas []ports.ProxyRule
	colas  []ports.RequestQueue
}

// mirarElevado lee lo que hace falta y nada más: las reglas si el trabajo
// nombra alguna y el IP Helper tiene un puerto pedido, las colas si System
// tiene un puerto pedido.
func mirarElevado(ctx contextoT, sys *sistema, job trabajo, hold []ports.Socket, forest *reaper.Forest, svc map[uint32][]reaper.Service, cfg reaper.Config) detras {
	var d detras
	for _, m := range reaper.PlanKill(job.Puertos, hold, forest, svc, cfg).Masked() {
		switch {
		case m.Relay == reaper.RelayPortProxy && len(job.Reglas) > 0 && d.reglas == nil && sys.proxy != nil:
			sys.proxy.consultar()
			d.reglas = sys.proxy.reglas
		case m.Relay == reaper.RelayHTTP && d.colas == nil && sys.http != nil:
			sys.http.consultar(ctx)
			d.colas = sys.http.colas
		}
	}
	return d
}

// planElevado es el plan del hijo elevado: el de siempre sobre lo que ve
// AHORA, más lo que hay detrás de los intermediarios, y podado a lo que el
// trabajo nombra. Puro.
func planElevado(job trabajo, hold []ports.Socket, forest *reaper.Forest, svc map[uint32][]reaper.Service, cfg reaper.Config, visto detras) reaper.Plan {
	plan := reaper.PlanKill(job.Puertos, hold, forest, svc, cfg)
	plan = detrasElevado(plan, job, forest, visto)
	return soloIdentidades(plan, job) // actuar SOLO sobre lo que el padre marcó
}

// soloIdentidades poda el plan para que el proceso elevado toque únicamente
// lo que el trabajo nombra: nada nuevo que haya aparecido en el puerto en el
// ínterin. Es la garantía de que elevar no mata a un tercero. Un servicio
// pasa si coinciden el nombre Y el proceso que lo corre: si se reinició entre
// que el usuario confirmó y que el hijo miró, ya no es lo que confirmó. De los
// externos solo pasan las reglas de portproxy: un contenedor o un proceso
// Linux no se detienen mejor por ser administrador.
func soloIdentidades(plan reaper.Plan, job trabajo) reaper.Plan {
	kill := map[reaper.ID]bool{}
	for _, id := range job.Kill {
		kill[id.Canon()] = true
	}
	svc := map[string]reaper.ID{}
	for _, s := range job.Servicios {
		svc[strings.ToLower(s.Nombre)] = s.Owner.Canon()
	}
	var keep []reaper.Target
	for _, tg := range plan.Targets {
		switch tg.Kind {
		case reaper.StopThirdSvc:
			if owner, ok := svc[strings.ToLower(tg.Service)]; ok && owner == tg.Root.Canon() {
				keep = append(keep, tg)
			}
		case reaper.StopExternal:
			if r, ok := tg.External.Ref.(*reglaProxy); ok && reglaPedida(job, r.ProxyRule) {
				keep = append(keep, tg)
			}
		default:
			if kill[tg.Root.Canon()] {
				keep = append(keep, tg)
			}
		}
	}
	plan.Targets = keep
	return plan
}

// reglaPedida dice si el trabajo nombra exactamente esa regla: la misma clase,
// el mismo lugar donde escucha y EL MISMO DESTINO. Una regla que cambió de
// destino desde que el usuario la vio no es la que confirmó.
func reglaPedida(job trabajo, r ports.ProxyRule) bool {
	for _, x := range job.Reglas {
		if x.Same(r) {
			return true
		}
	}
	return false
}

// detrasElevado mira detrás de los intermediarios del plan del hijo elevado.
// El hijo planifica de nuevo y, como todo plan, arranca negando a los
// intermediarios: sin esto, lo que el padre pidió quedaría negado y la
// elevación no haría nada. Cada puerta es angosta:
//
//   - un RELAY DE WSL colgado (un wslrelay lanzado desde una sesión elevada,
//     que el padre no pudo cerrar): solo un reenvío de WSL —jamás el motor de
//     contenedores ni VS Code—, solo con la identidad exacta que viajó en el
//     trabajo y solo por los puertos pedidos que sostiene. No se vuelve a mirar
//     dentro de WSL: la decisión ya la tomó el padre y la confirmó el usuario;
//   - una REGLA DE PORTPROXY: solo si sigue en el registro tal como la vio el
//     usuario (mismo destino) y tiene un socket del IP Helper en uno de los
//     puertos pedidos;
//   - detrás de HTTP.SYS: los procesos que las colas que ve el hijo le
//     atribuyen a ese puerto. Entran por la clasificación de siempre
//     (reaper.Unmask), y después soloIdentidades deja pasar únicamente a los
//     que el trabajo nombra.
func detrasElevado(plan reaper.Plan, job trabajo, forest *reaper.Forest, visto detras) reaper.Plan {
	pedidos := map[reaper.ID]bool{}
	for _, id := range job.Kill {
		pedidos[id.Canon()] = true
	}
	var vistos []reaper.Unmasking
	for _, m := range plan.Masked() {
		u := reaper.Unmasking{Via: m.Via, Port: m.Port, Proto: m.Proto, Verdict: reaper.UnmaskFound}
		switch m.Relay {
		case reaper.RelayWSL:
			if pedidos[m.Via.Canon()] {
				u.Verdict, u.KillVia, u.Reason = reaper.UnmaskEmpty, true, "relay colgado: en WSL no escucha nadie"
				vistos = append(vistos, u)
			}
		case reaper.RelayPortProxy:
			for _, r := range sostienen(visto.reglas, m.Sockets) {
				if reglaPedida(job, r) {
					u.Behind = append(u.Behind, externoDeRegla(r))
				}
			}
			if len(u.Behind) > 0 {
				vistos = append(vistos, u)
			}
		case reaper.RelayHTTP:
			var colas []ports.RequestQueue
			for _, q := range visto.colas {
				if len(q.On(m.Port)) > 0 {
					colas = append(colas, q)
				}
			}
			if u.Procs = identidades(colas, forest); len(u.Procs) > 0 {
				vistos = append(vistos, u)
			}
		}
	}
	if len(vistos) == 0 {
		return plan
	}
	return reaper.Unmask(plan, vistos)
}

// trabajoDe arma el trabajo con lo que quedó sin hacer por falta de permiso, y
// nada más: lo que el padre ya cerró, detuvo o borró no viaja.
func trabajoDe(plan reaper.Plan, rep reaper.Report, manuales []resultadoManual, o opciones) trabajo {
	denegado := map[reaper.ID]bool{}
	for _, v := range rep.Victims {
		if v.Outcome == reaper.OutcomeDenied {
			denegado[v.ID.Canon()] = true
		}
	}
	// Un objetivo se reintenta si alguna de sus víctimas quedó denegada.
	reintentar := func(tg reaper.Target) bool {
		for _, v := range tg.Victims {
			if denegado[v.Canon()] {
				return true
			}
		}
		return false
	}
	reglaDenegada := map[string]bool{}
	for _, x := range rep.Externals {
		if x.Outcome == reaper.OutcomeDenied && pideAdmin(x.External) {
			reglaDenegada[x.External.Key] = true
		}
	}
	job := trabajo{Clase: trabajoCierre, Puertos: puertosDe(plan), Tree: o.tree, Force: o.force, TCP: o.tcp, UDP: o.udp, AllStates: o.allStates, Timeout: o.timeout}
	for _, tg := range plan.Targets {
		switch tg.Kind {
		case reaper.StopThirdSvc:
			if reintentar(tg) {
				job.Servicios = append(job.Servicios, svcObjetivo{Nombre: tg.Service, Owner: tg.Root})
			}
		case reaper.StopExternal:
			if r, ok := tg.External.Ref.(*reglaProxy); ok && reglaDenegada[tg.External.Key] {
				job.Reglas = append(job.Reglas, r.ProxyRule)
			}
		default:
			if reintentar(tg) {
				job.Kill = append(job.Kill, tg.Root)
			}
		}
	}
	for _, m := range manuales {
		if m.Fin == manualDenegado && m.Elevable {
			job.Manual = append(job.Manual, m.Servicio)
		}
	}
	return job
}

func puertosDe(plan reaper.Plan) ports.Set {
	s := plan.Ownerless
	for _, tg := range plan.Targets {
		s = s.Union(tg.Ports)
	}
	for _, r := range plan.Refusals {
		s = s.Union(r.Ports)
	}
	return s
}

// --- archivo de resultado ---------------------------------------------------

// carpetaDeTrabajo es la carpeta de killport entre los datos locales del
// usuario (%LOCALAPPDATA%\navaja\killport): no viaja con un perfil móvil ni se
// sincroniza, y solo la escribe su dueño.
func carpetaDeTrabajo() string {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return ""
	}
	return filepath.Join(local, "navaja", "killport")
}

// carpetasDeResultado son las carpetas donde puede ir un archivo de resultado,
// en su forma canónica: la de trabajo de killport y la de temporales, las dos
// del usuario con el que corre ESTE proceso. El padre elige una; el hijo
// elevado acepta la ruta solo si cae en una de las suyas. Con el UAC de
// siempre (el mismo usuario, elevado) son las mismas. Si el que eleva es OTRO
// usuario (las credenciales de un administrador en la sesión de un usuario
// común) no coinciden y el hijo no deja informe: hace su trabajo igual, y el
// padre mira cómo quedó el sistema.
func carpetasDeResultado() []string {
	var out []string
	for _, dir := range []string{carpetaDeTrabajo(), os.TempDir()} {
		if dir == "" {
			continue
		}
		if canon, err := win.FinalPath(dir); err == nil {
			out = append(out, canon)
		}
	}
	return out
}

// prepararResultado elige la ruta del archivo de resultado, en la carpeta de
// trabajo (o en %TEMP% si esa no se puede crear), y la devuelve en su forma
// canónica: el hijo elevado creará el archivo AHÍ con CreateNewExact, que se
// niega si la carpeta fue desviada.
func prepararResultado() (string, error) {
	dir := carpetaDeTrabajo()
	if dir == "" || os.MkdirAll(dir, 0o755) != nil {
		dir = os.TempDir()
	}
	canon, err := win.FinalPath(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(canon, nombreResultado(win.CurrentProcessID(), time.Now().UnixNano())), nil
}

// escribirResultado guarda el documento para el padre, de forma segura: solo
// en una ruta que el hijo acepta (rutaResultadoValida), en un archivo nuevo,
// sin seguir enlaces y con la ruta final verificada. Best-effort: si no se
// puede, no hay informe y el padre igual mira por su cuenta cómo quedó todo.
func escribirResultado(path string, doc any) {
	if !rutaResultadoValida(path, carpetasDeResultado()) {
		return
	}
	f, err := win.CreateNewExact(path)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(doc)
}

// maxResultado acota lo que el padre lee del archivo de resultado.
const maxResultado = 1 << 20

// leerArchivoResultado lee el archivo de resultado, hasta maxResultado. El
// tope se aplica sobre lo que se LEE del archivo abierto, no sobre el tamaño
// que decía tener: entre una cosa y la otra puede crecer.
func leerArchivoResultado(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxResultado+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxResultado {
		return nil, errors.New("el resultado es demasiado grande")
	}
	return b, nil
}

// textoAjeno limpia un texto que viene del archivo de resultado antes de
// mostrarlo: lo escribió el proceso elevado, pero en una carpeta donde escribe
// cualquier programa del usuario. Una sola línea, sin caracteres de control
// (ni secuencias de escape de terminal) y de largo acotado.
func textoAjeno(s string) string {
	const tope = 240
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == tope {
			b.WriteString("…")
			break
		}
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		if unicode.IsControl(r) || r == utf8.RuneError {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// removerResultado borra el archivo de resultado (el padre, al terminar).
func removerResultado(path string) { os.Remove(path) }

// leerResultadoConfig lee el resultado de una acción de configuración.
func leerResultadoConfig(path string) (jsonConfig, error) {
	var doc jsonConfig
	b, err := leerArchivoResultado(path)
	if err != nil {
		return doc, err
	}
	err = json.Unmarshal(b, &doc)
	return doc, err
}

func leerResultado(path string) (jsonReporte, error) {
	var doc jsonReporte
	b, err := leerArchivoResultado(path)
	if err != nil {
		return doc, err
	}
	err = json.Unmarshal(b, &doc)
	return doc, err
}

// fusionarVictimas actualiza el destino de las víctimas denegadas con lo que
// pasó en el proceso elevado. despues es el bosque de una instantánea tomada
// cuando el hijo ya volvió: que una víctima murió se comprueba AHÍ, lo haya
// informado el hijo o no (nil si no se pudo tomar: queda lo que dijo el hijo).
//
//   - ya no está: se fue. Cómo —cerró sola, en seco, se detuvo su servicio— lo
//     cuenta el informe; sin informe, lo único que se sabe es que ya no está.
//   - sigue viva y el hijo dijo que la había cerrado: no se le cree.
//   - sigue viva: el motivo que dio el hijo (protegida, crítica…).
func fusionarVictimas(orig []reaper.VictimResult, elevado jsonReporte, despues *reaper.Forest) []reaper.VictimResult {
	type dicho struct {
		fin reaper.Outcome
		con reaper.Method
	}
	nuevo := map[uint32]dicho{}
	for _, v := range elevado.Victimas {
		nuevo[v.PID] = dicho{outcomeDeJSON(v.Destino), metodoDeTexto(v.Metodo)}
	}
	for i := range orig {
		v := &orig[i]
		if v.Outcome != reaper.OutcomeDenied {
			continue
		}
		d, dijo := nuevo[v.ID.PID]
		dicho := d.fin
		vive := dijo && !dicho.Done() // sin instantánea, lo que dijo el hijo
		if despues != nil {
			_, vive = despues.Lookup(v.ID.Canon())
		}
		switch {
		case !vive && (dijo || despues != nil):
			v.Outcome, v.Method, v.Err = reaper.OutcomeGone, reaper.MethodNone, nil
			if dijo && dicho.Done() {
				v.Outcome = dicho
			}
			switch v.Outcome {
			case reaper.OutcomeServiceStopped:
				v.Method = reaper.MethodService
			case reaper.OutcomeTerminated:
				v.Method = reaper.MethodTerminate
			case reaper.OutcomeClosed:
				v.Method = d.con // con qué cedió: solo para contarlo
			}
		case !dijo:
			// Sin informe y viva: queda como estaba.
		case dicho.Done():
			v.Outcome, v.Err = reaper.OutcomeError, errors.New("el proceso elevado informó que lo cerró, pero sigue vivo")
		default:
			v.Outcome = dicho
		}
	}
	return orig
}

// fusionarExternos actualiza el destino de los externos denegados (las reglas
// de portproxy) con lo que pasó en el proceso elevado. Como con todo lo que
// vuelve del hijo, manda lo que se ve: que una regla se borró se comprueba en
// el registro, lo haya informado el hijo o no.
func fusionarExternos(sys *sistema, orig []reaper.ExternalResult, elevado jsonReporte) []reaper.ExternalResult {
	nuevo := map[string]jsonExterno{}
	for _, x := range elevado.Externos {
		nuevo[x.Clave] = x
	}
	for i := range orig {
		r := &orig[i]
		if r.Outcome != reaper.OutcomeDenied {
			continue
		}
		regla, esRegla := r.External.Ref.(*reglaProxy)
		if !esRegla || sys.proxy == nil {
			continue // lo único que viaja al proceso elevado son las reglas
		}
		_, sigue, err := sys.proxy.buscar(regla.ProxyRule)
		x, dijo := nuevo[r.External.Key]
		switch {
		case err != nil:
			r.Outcome, r.Err = reaper.OutcomeError, errors.New("no pude volver a leer las reglas de portproxy: "+err.Error())
		case !sigue:
			r.Outcome, r.Method, r.Err = reaper.OutcomeClosed, reaper.MethodNetsh, nil
			if x.MS > 0 && x.MS < time.Hour.Milliseconds() {
				r.Elapsed = time.Duration(x.MS) * time.Millisecond
			}
		case !dijo:
			// Sin informe y con la regla en su lugar: queda como estaba.
		case outcomeDeJSON(x.Destino).Done():
			r.Outcome, r.Err = reaper.OutcomeError, errors.New("el proceso elevado informó que la borró, pero la regla sigue en el registro")
		default:
			r.Outcome, r.Err = reaper.OutcomeError, errors.New("el proceso elevado no pudo borrarla")
			if dijo := textoAjeno(x.Error); dijo != "" {
				r.Err = errors.New(dijo)
			}
		}
	}
	return orig
}

// fusionarManual reemplaza los --manual que viajaron al proceso elevado por lo
// que pasó allá. Lo que el hijo dice que HIZO no se cree: se vuelve a leer la
// configuración del servicio, y si no quedó en manual es un error. Y al revés:
// si el hijo no dijo nada de un servicio (no dejó su informe) pero el servicio
// quedó en manual, quedó en manual. Manda lo que se ve.
func fusionarManual(sys *sistema, orig []resultadoManual, elevado jsonReporte, systemRoot string) []resultadoManual {
	nuevo := map[string]jsonManual{}
	for _, m := range elevado.Manual {
		nuevo[strings.ToLower(m.Servicio)] = m
	}
	for i := range orig {
		r := &orig[i]
		if r.Fin != manualDenegado || !r.Elevable {
			continue
		}
		r.Elevable = false // ya se elevó: lo que quedó, quedó
		sys.svc.olvidar(r.Servicio)
		ahora, leido := sys.svc.de(r.Servicio)
		quedo := leido && ahora.Start == win.StartDemand
		// hecho: cómo deshacerlo y los avisos salen de lo que el padre había
		// leído ANTES, jamás de lo que diga el archivo.
		hecho := func() {
			r.Fin, r.Motivo, r.Deshacer, r.Aviso = manualHecho, "", "", ""
			if antes := r.Antes; antes.Name != "" {
				r.Deshacer, r.Aviso = comandoInicio(r.Servicio, antes), avisoDisparadores(antes)
			}
		}
		m, dijo := nuevo[strings.ToLower(r.Servicio)]
		if !dijo {
			if quedo && r.Antes.Name != "" && r.Antes.Start != win.StartDemand {
				hecho()
			}
			continue
		}
		switch finDeClave(m.Resultado) {
		case manualHecho:
			if !quedo {
				r.Fin, r.Motivo = manualFallo, "el proceso elevado informó el cambio, pero el servicio no quedó en inicio manual"
				continue
			}
			hecho()
		case manualYaEstaba:
			r.Fin, r.Motivo = manualYaEstaba, "ya estaba en inicio manual"
		case manualNoSeToca:
			// El porqué sale de lo que el padre lee, no del archivo. Si el padre
			// no le ve ningún impedimento, lo dice tal cual.
			por := "el proceso elevado no lo reconoció como un servicio de terceros"
			if leido {
				if p := motivoNoManual(ahora, systemRoot); p != "" {
					por = p
				}
			}
			r.Fin, r.Motivo = manualNoSeToca, por+": killport no le cambia el inicio"
		case manualSigueVivo:
			r.Fin, r.Motivo = manualSigueVivo, "no quedó detenido: no le cambio el inicio a un servicio que sigue corriendo"
		case manualNoDetenido:
			r.Fin, r.Motivo = manualNoDetenido, "el proceso elevado no lo detuvo (cuando miró, ya no tenía el puerto): no le cambió el inicio"
		default:
			r.Fin, r.Motivo = manualFallo, "el proceso elevado no pudo cambiarle el inicio"
		}
	}
	return orig
}

// finDeClave es la inversa de finManual.clave.
func finDeClave(s string) finManual {
	for _, f := range []finManual{manualHecho, manualYaEstaba, manualNoSeToca, manualSigueVivo, manualDenegado, manualNoDetenido} {
		if f.clave() == s {
			return f
		}
	}
	return manualFallo
}

// metodoDeTexto es la inversa de reaper.Method.String para los métodos con
// los que un proceso de Windows cede por las buenas; cualquier otra cosa,
// MethodNone.
func metodoDeTexto(s string) reaper.Method {
	for _, m := range []reaper.Method{reaper.MethodCtrlC, reaper.MethodClose, reaper.MethodSelf} {
		if m.String() == s {
			return m
		}
	}
	return reaper.MethodNone
}

// outcomeDeJSON es la inversa de destinoJSON (solo lo que puede volver del hijo).
func outcomeDeJSON(s string) reaper.Outcome {
	switch s {
	case "cerrado":
		return reaper.OutcomeClosed
	case "terminado":
		return reaper.OutcomeTerminated
	case "ya_no_estaba":
		return reaper.OutcomeGone
	case "servicio_detenido":
		return reaper.OutcomeServiceStopped
	case "reciclado":
		return reaper.OutcomeRecycled
	case "protegido":
		return reaper.OutcomeProtected
	case "critico":
		return reaper.OutcomeCritical
	case "denegado":
		return reaper.OutcomeDenied
	}
	return reaper.OutcomeError
}
