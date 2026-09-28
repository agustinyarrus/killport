//go:build windows

package killport

// libre_windows.go: el flujo de --free contra el sistema de verdad, la
// sugerencia del próximo libre cuando un puerto no se pudo liberar, y cómo
// se muestra todo (texto y JSON). La búsqueda es ports.FindFree; la
// consulta y los textos, libre.go.

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

// salteosVisibles es cuántos tramos salteados se cuentan en la consola; el
// resto se resume (el JSON los trae todos).
const salteosVisibles = 8

// correrLibre es `killport --free [inicio | rango] [--count N]`.
//
// En consola cuenta qué encontró y por qué se salteó lo anterior. Sin consola
// (`$port = killport --free 3000`) escribe SOLO los números, uno por línea, y
// los avisos van a la salida de errores: lo que capture el script es el
// puerto y nada más.
func correrLibre(t *tui.Term, sys *sistema, pos []string, o opciones) int {
	q, err := consultaLibre(pos, o.tcp, o.udp, o.count)
	if err != nil {
		return falloUsage(t, err)
	}
	ctx, stop := contexto()
	defer stop()
	res, err := buscarLibres(ctx, sys, q)
	if err != nil {
		if !o.json && !t.Interactive() {
			fmt.Fprintln(os.Stderr, "killport: "+err.Error())
			return cli.ExitFailure
		}
		return fallo(t, err)
	}
	quien := nuevoNombrador(ctx, sys)
	code := codigoLibre(q, res)
	if o.json || t.Interactive() {
		// Lo que se cuenta de lo salteado dice de quién es cada exclusión.
		if xs, err := sys.exclusiones(); err == nil {
			res.Skips = conOrigen(res.Skips, xs)
		}
	}

	switch {
	case o.json:
		escribirJSON(t, libreJSON(q, res, quien.imagen))
		return code
	case !t.Interactive():
		for _, p := range res.Found {
			t.Print(u32(uint32(p)) + "\n")
		}
		if code != cli.ExitOK {
			fmt.Fprintln(os.Stderr, "killport: "+faltanLibres(q, res))
		}
		return code
	}

	t.Lines(t.Header("killport", "un puerto libre de verdad", o.version))
	detalle := protosTexto(q.Protos) + " · IPv4 e IPv6 · confirmado con un bind exclusivo"
	switch {
	case len(res.Found) == 0:
		t.Lines([]string{t.Status(tui.Fail, faltanLibres(q, res), "")})
	case code == cli.ExitPartial:
		t.Lines([]string{t.Status(tui.Warn, listaPuertosTexto(res.Found)+" "+tui.Plural(int64(len(res.Found)), "libre", "libres"), faltanLibres(q, res))})
	default:
		t.Lines([]string{t.Status(tui.OK, listaPuertosTexto(res.Found)+" "+tui.Plural(int64(len(res.Found)), "libre", "libres"), detalle)})
	}
	t.Lines(lineasSalteos(t, res.Skips, quien.quien, salteosVisibles))
	t.Blank()
	if len(res.Found) > 0 {
		t.Lines(pista(t, pistaScript(pos, o)))
		t.Blank()
	}
	return code
}

// pistaScript es el mismo pedido escrito para un script, donde sale solo el
// número. En PowerShell, que es la consola de Windows ("PORT=$(…)" es bash:
// PowerShell no lo entiende), y la de bash al lado.
func pistaScript(pos []string, o opciones) string {
	cmd := strings.Join(append([]string{"killport", "--free"}, pos...), " ")
	if ps := protosPedidos(o.tcp, o.udp, ports.TCP); !slices.Equal(ps, []ports.Proto{ports.TCP}) {
		for _, p := range ps {
			cmd += " --" + strings.ToLower(p.String())
		}
	}
	if o.count > 1 {
		cmd += " --count " + u32(uint32(o.count))
		return "en un script, sin consola, sale un número por línea: $ports = " + cmd + " (en bash, PORTS=$(" + cmd + "))"
	}
	return "en un script, sin consola, sale solo el número: $port = " + cmd + " (en bash, PORT=$(" + cmd + "))"
}

// buscarLibres lee lo que hace falta (la tabla completa sin dueños, los
// rangos excluidos y los dinámicos) y busca. Sin exclusiones leídas se sigue:
// el sondeo exclusivo igual rechaza lo que tapa WinNAT. Sin rangos dinámicos,
// FindFree asume el de fábrica.
func buscarLibres(ctx context.Context, sys *sistema, q ports.FreeQuery) (ports.FreeResult, error) {
	socks, err := sys.AllSockets(ctx)
	if err != nil {
		return ports.FreeResult{}, fmt.Errorf("no pude leer las tablas de red: %w", err)
	}
	l := sys.red.leer()
	return ports.FindFree(q, ports.FreeWorld{Sockets: socks, Excluded: l.excl, Dynamic: l.dyn}, sys.sondear()), nil
}

// sondear es el sondeo de bind exclusivo como ports.Prober.
func (s *sistema) sondear() ports.Prober { return s.ProbeBind }

// codigoLibre: 0 si encontró los pedidos, 1 si encontró menos, 3 si ninguno.
func codigoLibre(q ports.FreeQuery, r ports.FreeResult) int {
	switch {
	case len(r.Found) >= max(q.Count, 1):
		return cli.ExitOK
	case len(r.Found) > 0:
		return cli.ExitPartial
	}
	return cli.ExitFailure
}

// faltanLibres cuenta lo que no se encontró.
func faltanLibres(q ports.FreeQuery, r ports.FreeResult) string {
	donde := "entre el " + u32(uint32(q.Within.Lo)) + " y el " + u32(uint32(q.Within.Hi)) + " (" + protosTexto(q.Protos) + ")"
	if r.Capped {
		donde += "; dejé de buscar después de " + tui.Count(int64(r.Probes), "sondeo", "sondeos") + " que fallaron"
	}
	if len(r.Found) == 0 {
		return "No hay ningún puerto libre " + donde
	}
	return "encontré " + u32(uint32(len(r.Found))) + " de " + u32(uint32(max(q.Count, 1))) + " " + donde
}

// nombrador nombra a los dueños de los puertos salteados con UNA foto de
// procesos por corrida (y el proyecto, que también se recuerda).
type nombrador struct {
	sys    *sistema
	ctx    context.Context
	forest *reaper.Forest
}

func nuevoNombrador(ctx context.Context, sys *sistema) *nombrador {
	return &nombrador{sys: sys, ctx: ctx}
}

func (n *nombrador) proc(pid uint32) (reaper.Proc, bool) {
	if n.forest == nil {
		procs, _ := n.sys.Snapshot(n.ctx)
		n.forest = reaper.NewForest(procs)
	}
	return n.forest.LivePID(pid)
}

// imagen es el nombre de la imagen ("node.exe"), o "" si no se supo.
func (n *nombrador) imagen(pid uint32) string {
	if p, ok := n.proc(pid); ok {
		return p.Name
	}
	return ""
}

// quien es "node.exe (PID 812 · shop-web)": quienEs con el proyecto, si
// tiene. Sin nombre, "PID 812 · shop-web". "" si el PID no está.
func (n *nombrador) quien(pid uint32) string {
	p, ok := n.proc(pid)
	if !ok {
		return ""
	}
	pr, conProyecto := n.sys.proyectoDe(p.ID)
	switch {
	case !conProyecto:
		return quienEs(p.Name, pid)
	case p.Name == "":
		return quienEs("", pid) + " · " + pr.Nombre
	}
	return p.Name + " (PID " + u32(pid) + " · " + pr.Nombre + ")"
}

// lineasSalteos son los renglones de lo salteado, alineados; a lo sumo tope
// (el resto se resume).
func lineasSalteos(t *tui.Term, skips []ports.Skip, quien func(uint32) string, tope int) []string {
	if len(skips) == 0 {
		return nil
	}
	ancho := 0
	for i, s := range skips {
		if i < tope {
			ancho = max(ancho, tui.Width(s.Range.String()))
		}
	}
	out := []string{"", tui.Margin + t.Paint(tui.Subtle, "salteé")}
	for i, s := range skips {
		if i == tope {
			out = append(out, tui.Margin+"  "+t.Paint(tui.Faint, "y "+tui.Count(int64(len(skips)-tope), "tramo más", "tramos más")+" (--json los trae todos)"))
			break
		}
		out = append(out, tui.Margin+"  "+t.Paint(tui.Peach, tui.PadRight(s.Range.String(), ancho))+"   "+t.Paint(tui.Subtle, textoSalteo(s, quien)))
	}
	return out
}

// --- La sugerencia -------------------------------------------------------------

// maxAlternativas acota a cuántos puertos que no se liberaron se les busca un
// reemplazo: más que eso es un pedido de rangos, y ahí la respuesta es --free.
const maxAlternativas = 8

// alternativas busca, para cada puerto de ps, el próximo libre de verdad (del
// siguiente para arriba, sin el rango dinámico). protos dice en qué
// protocolos tiene que estar libre cada uno. Una sola lectura de las tablas
// para todos. Lo que no se pudo buscar, no figura.
func alternativas(ctx context.Context, sys *sistema, ps []uint16, protos func(uint16) []ports.Proto) map[uint16]uint16 {
	if len(ps) == 0 || sys == nil {
		return nil
	}
	if len(ps) > maxAlternativas {
		ps = ps[:maxAlternativas]
	}
	socks, err := sys.AllSockets(ctx)
	if err != nil {
		return nil
	}
	l := sys.red.leer()
	w := ports.FreeWorld{Sockets: socks, Excluded: l.excl, Dynamic: l.dyn}
	out := map[uint16]uint16{}
	for _, p := range ps {
		if p == 65535 {
			continue
		}
		r := ports.FindFree(ports.FreeQuery{Within: ports.Range{Lo: p + 1, Hi: 65535}, Protos: protos(p), Count: 1}, w, sys.sondear())
		if len(r.Found) == 1 {
			out[p] = r.Found[0]
		}
	}
	return out
}

// protosAlternativa: el reemplazo tiene que estar libre en lo que pidió el
// usuario (--tcp/--udp) o, si no dijo, en el protocolo que tapa el puerto.
func protosAlternativa(o opciones, tapa ports.Proto) []ports.Proto {
	if tapa == 0 {
		tapa = ports.TCP
	}
	return protosPedidos(o.tcp, o.udp, tapa)
}

// alternativasDe busca reemplazo para los puertos del reporte que no quedaron
// libres.
func alternativasDe(ctx context.Context, sys *sistema, rep reaper.Report, o opciones) map[uint16]uint16 {
	var ps []uint16
	tapa := map[uint16]ports.Proto{}
	for _, p := range rep.Ports {
		if p.Status != reaper.PortFree {
			ps = append(ps, p.Port)
			tapa[p.Port] = p.Proto
		}
	}
	if rep.Canceled {
		return nil
	}
	return alternativas(ctx, sys, ps, func(p uint16) []ports.Proto { return protosAlternativa(o, tapa[p]) })
}

// lineaAlternativa es el renglón de la sugerencia.
func lineaAlternativa(t *tui.Term, alt uint16) string {
	return "    " + t.Paint(tui.Faint, "→ ") + t.Paint(tui.Sage, "el "+u32(uint32(alt))+" está libre") + t.Paint(tui.Faint, ", si te sirve otro puerto")
}

// anotarAlternativas pone la sugerencia en el JSON del reporte.
func (doc *jsonReporte) anotarAlternativas(alts map[uint16]uint16) {
	for i := range doc.Puertos {
		if a, ok := alts[doc.Puertos[i].Puerto]; ok {
			doc.Puertos[i].Alternativa = a
		}
	}
}

// --- JSON -----------------------------------------------------------------------

// jsonLibre es el documento de --free. encontrados: los puertos libres (una
// lista; "libres" en el reporte de cerrar es un número). inicio y fin: el
// rango en el que se buscó ("desde" en el JSON es siempre una hora).
type jsonLibre struct {
	Encontrados []uint16       `json:"encontrados"`
	Pedidos     int            `json:"pedidos"`
	Inicio      uint16         `json:"inicio"`
	Fin         uint16         `json:"fin"`
	Explicito   bool           `json:"explicito"` // el rango lo escribió el usuario: vale el dinámico
	Protos      []string       `json:"protos"`
	Salteados   []jsonSalteado `json:"salteados"`
	Sondeos     int            `json:"sondeos"`
	Tope        bool           `json:"tope,omitempty"` // se dejó de buscar: el sondeo falló demasiadas veces
}

// jsonSalteado es un tramo salteado. motivo: tomado | winnat | httpsys |
// programa | administrada | dinamico | sondeo.
type jsonSalteado struct {
	Inicio   uint16        `json:"inicio"`
	Fin      uint16        `json:"fin"`
	Motivo   string        `json:"motivo"`
	PID      uint32        `json:"pid,omitempty"`     // tomado
	Proceso  string        `json:"proceso,omitempty"` // tomado
	Estado   string        `json:"estado,omitempty"`  // tomado en TCP: listen | established | time_wait | …
	Rango    *jsonRango    `json:"rango,omitempty"`   // winnat, httpsys, programa, administrada: la fila entera
	Dinamico *jsonDinamico `json:"dinamico,omitempty"`
	Proto    string        `json:"proto,omitempty"`   // tomado, sondeo
	Familia  string        `json:"familia,omitempty"` // sondeo
	Bind     string        `json:"bind,omitempty"`    // sondeo: en_uso | prohibido | error
}

func libreJSON(q ports.FreeQuery, r ports.FreeResult, imagen func(uint32) string) jsonLibre {
	doc := jsonLibre{Encontrados: append([]uint16{}, r.Found...), Pedidos: max(q.Count, 1), Inicio: q.Within.Lo, Fin: q.Within.Hi,
		Explicito: q.Explicit, Protos: []string{}, Salteados: []jsonSalteado{}, Sondeos: r.Probes, Tope: r.Capped}
	for _, p := range q.Protos {
		doc.Protos = append(doc.Protos, p.String())
	}
	for _, s := range r.Skips {
		js := jsonSalteado{Inicio: s.Lo, Fin: s.Hi}
		switch s.Why {
		case ports.SkipTaken:
			js.Motivo, js.PID, js.Proto = "tomado", s.Socket.PID, s.Socket.Proto.String()
			if imagen != nil && s.Socket.PID != 0 {
				js.Proceso = imagen(s.Socket.PID)
			}
			if s.Socket.Proto == ports.TCP {
				js.Estado = strings.ToLower(s.Socket.State.String())
			}
		case ports.SkipReserved, ports.SkipAdministered:
			js.Motivo = origenJSON(s.Excluded)
			js.Rango = rangoJSON(s.Excluded)
		case ports.SkipDynamic:
			d := s.Dynamic
			js.Motivo = "dinamico"
			js.Dinamico = &jsonDinamico{Proto: d.Proto.String(), Familia: d.Family.String(), Inicio: d.Start, Cantidad: d.Count, DeFabrica: d.Default()}
		case ports.SkipProbe:
			js.Motivo, js.Proto, js.Familia = "sondeo", s.Proto.String(), s.Family.String()
			switch s.Bind {
			case ports.BindInUse:
				js.Bind = "en_uso"
			case ports.BindForbidden:
				js.Bind = "prohibido"
			default:
				js.Bind = "error"
			}
		}
		doc.Salteados = append(doc.Salteados, js)
	}
	return doc
}
