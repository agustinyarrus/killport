//go:build windows

package killport

// portproxy.go: mirar DETRÁS del svchost del IP Helper. Cuando la tabla dice
// que un svchost escucha el 3000 y el servicio es iphlpsvc, lo que hay es una
// regla de `netsh interface portproxy` —la receta clásica para llegar desde la
// red a un servidor dentro de WSL 2— que quedó en el registro y vuelve en cada
// arranque. El objetivo no es un proceso: es LA REGLA, y se libera borrándola.
//
// Borrar una regla pide administrador. Sin elevar, killport la muestra, dice
// exactamente qué va a hacer y, si el usuario confirma, la borra por el
// trabajo elevado (elevacion.go), que vuelve a leer el registro y solo borra
// si la regla sigue ahí y sigue conectando al mismo destino.
//
// Lo que NO se hace nunca es detener el IP Helper, que es lo que recomiendan
// los foros (`net stop iphlpsvc`): de él dependen el reenvío de localhost de
// WSL y los túneles de IPv6.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win"
)

// portproxy es el cliente de una corrida. Las tres funciones se inyectan en
// las pruebas: ninguna prueba toca el registro ni las reglas de la máquina.
type portproxy struct {
	leer    func(clave string) (valores []win.RegValue, existe bool, err error)
	netsh   netshFn
	elevado func() bool

	once      sync.Once
	reglas    []ports.ProxyRule
	ilegibles []string // valores del registro que no se pudieron interpretar
	err       error
}

func nuevoPortproxy(n netshFn) *portproxy {
	return &portproxy{leer: win.MachineValues, netsh: n, elevado: win.IsElevated}
}

// leerReglas lee las cuatro claves y devuelve las reglas en un orden fijo
// (por clase y, dentro de cada una, por puerto y dirección). Que una clave no
// exista es "no hay reglas de esa clase": en una máquina donde nunca se usó
// portproxy no existe ninguna. Un error al leer cualquiera invalida todo: con
// media lista, "no hay regla para ese puerto" sería una mentira posible.
func leerReglas(leer func(string) ([]win.RegValue, bool, error)) (reglas []ports.ProxyRule, ilegibles []string, err error) {
	for _, k := range ports.ProxyKinds {
		vals, existe, err := leer(k.RegistryPath())
		if err != nil {
			return nil, nil, err
		}
		if !existe {
			continue
		}
		var clase []ports.ProxyRule
		for _, v := range vals {
			r, err := ports.ParseProxyRule(k, v.Name, v.Data)
			if err != nil {
				ilegibles = append(ilegibles, k.String()+" "+v.Name)
				continue
			}
			clase = append(clase, r)
		}
		sort.SliceStable(clase, func(i, j int) bool {
			if clase[i].ListenPort != clase[j].ListenPort {
				return clase[i].ListenPort < clase[j].ListenPort
			}
			return clase[i].ListenAddr < clase[j].ListenAddr
		})
		reglas = append(reglas, clase...)
	}
	return reglas, ilegibles, nil
}

// consultar lee las reglas, una sola vez por corrida.
func (p *portproxy) consultar() {
	p.once.Do(func() { p.reglas, p.ilegibles, p.err = leerReglas(p.leer) })
}

// sostienen son las reglas que tienen alguno de esos sockets.
func sostienen(reglas []ports.ProxyRule, socks []ports.Socket) []ports.ProxyRule {
	var out []ports.ProxyRule
	for _, r := range reglas {
		for _, s := range socks {
			if r.Holds(s) {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// desenmascarar contesta, por cada puerto que sostiene el IP Helper, qué regla
// lo tiene (Found), que ninguna (Empty) o que no se pudo leer el registro
// (Unavailable).
func (p *portproxy) desenmascarar(masked []reaper.MaskedPort) []reaper.Unmasking {
	p.consultar()
	out := make([]reaper.Unmasking, 0, len(masked))
	for _, m := range masked {
		u := reaper.Unmasking{Via: m.Via, Port: m.Port, Proto: m.Proto}
		switch reglas := sostienen(p.reglas, m.Sockets); {
		case p.err != nil:
			u.Verdict = reaper.UnmaskUnavailable
			u.Reason = "lo escucha el servicio IP Helper por una regla de portproxy, y no pude leer las reglas: " + p.err.Error()
			u.Advice = "`netsh interface portproxy show all` las muestra; no detengas iphlpsvc: de él dependen WSL e IPv6"
		case len(reglas) > 0:
			u.Verdict = reaper.UnmaskFound
			for _, r := range reglas {
				u.Behind = append(u.Behind, externoDeRegla(r))
			}
		default:
			u.Verdict = reaper.UnmaskEmpty
			u.Reason = "lo escucha el servicio IP Helper (iphlpsvc), pero ninguna regla de portproxy tiene ese puerto"
			if len(p.ilegibles) > 0 {
				u.Reason += " (hay " + contar(len(p.ilegibles), "regla que no pude interpretar", "reglas que no pude interpretar") + ": " + strings.Join(p.ilegibles, ", ") + ")"
			}
			u.Advice = "`netsh interface portproxy show all` muestra las reglas; no detengas iphlpsvc: de él dependen WSL e IPv6"
		}
		out = append(out, u)
	}
	return out
}

// externoDeRegla arma el objetivo externo de una regla.
func externoDeRegla(r ports.ProxyRule) reaper.External {
	return reaper.External{
		Kind: reaper.ExtRule, Where: "portproxy", Key: r.Key(), Name: r.String(),
		Ports: ports.Of(r.ListenPort), Ref: &reglaProxy{ProxyRule: r},
	}
}

// errPideAdmin: la operación necesita el token de administrador.
var errPideAdmin = errors.New("hace falta administrador")

// borrar borra UNA regla, y solo esa. Antes vuelve a leer el registro: la
// regla tiene que seguir ahí y seguir conectando al mismo destino que vio el
// usuario; si cambió, es otra regla y no se toca. Después lo vuelve a leer:
// que netsh haya dicho que sí no alcanza.
func (p *portproxy) borrar(ctx context.Context, r ports.ProxyRule) reaper.ExternalStop {
	if !p.elevado() {
		return reaper.ExternalStop{Outcome: reaper.OutcomeDenied, Err: errPideAdmin}
	}
	switch actual, esta, err := p.buscar(r); {
	case err != nil:
		return reaper.ExternalStop{Outcome: reaper.OutcomeError, Err: err}
	case !esta:
		return reaper.ExternalStop{Outcome: reaper.OutcomeGone}
	case !actual.Same(r):
		return reaper.ExternalStop{Outcome: reaper.OutcomeError,
			Err: errors.New("la regla cambió mientras tanto (ahora es " + actual.String() + "): no la borro")}
	}
	if _, err := p.netsh(ctx, r.DeleteArgs()...); err != nil {
		if ctx.Err() != nil {
			return reaper.ExternalStop{Outcome: reaper.OutcomePending}
		}
		return reaper.ExternalStop{Outcome: reaper.OutcomeError, Err: err}
	}
	if _, esta, err := p.buscar(r); err != nil || esta {
		return reaper.ExternalStop{Outcome: reaper.OutcomeError, Err: errors.New("netsh no informó ningún error, pero la regla sigue en el registro")}
	}
	return reaper.ExternalStop{Outcome: reaper.OutcomeClosed, Method: reaper.MethodNetsh}
}

// buscar relee el registro y devuelve la regla que hoy tiene esa identidad
// (la clase y dónde escucha), conecte adonde conecte.
func (p *portproxy) buscar(r ports.ProxyRule) (actual ports.ProxyRule, esta bool, err error) {
	reglas, _, err := leerReglas(p.leer)
	if err != nil {
		return ports.ProxyRule{}, false, err
	}
	for _, x := range reglas {
		if strings.EqualFold(x.Key(), r.Key()) {
			return x, true, nil
		}
	}
	return ports.ProxyRule{}, false, nil
}

// --- la regla como objetivo externo (ver externo.go) --------------------------

// reglaProxy es una regla de portproxy como objetivo.
type reglaProxy struct{ ports.ProxyRule }

// detener: borrarla. Una regla no tiene cierre prolijo ni forzado: está o no
// está, así que el plazo y --force no cambian nada.
func (r *reglaProxy) detener(ctx context.Context, s *sistema, _ time.Time, _ bool) (reaper.ExternalStop, bool) {
	res := s.proxy.borrar(ctx, r.ProxyRule)
	return res, ctx.Err() == nil
}

// presentar: la regla en el título; qué hace, por qué sigue ahí y el comando
// exacto que la borra.
func (r *reglaProxy) presentar(t *tui.Term, _ reaper.Target, _ time.Time) (string, []string) {
	titulo := t.Paint(tui.Faint, "regla de portproxy ") + t.Paint(tui.Text, r.String())
	return titulo, []string{
		t.Paint(tui.Faint, "reenvía cada conexión que llega a ese puerto; la atiende el servicio IP Helper"),
		t.Paint(tui.Faint, "es persistente: está en el registro y vuelve en cada arranque hasta que se la borre"),
		t.Paint(tui.Faint, "se borra con `netsh "+strings.Join(r.DeleteArgs(), " ")+"` (pide administrador)"),
	}
}

// anotar: el objeto "regla" del JSON.
func (r *reglaProxy) anotar(o *jsonObjetivo) { o.Regla = reglaJSON(r.ProxyRule) }

// pideAdmin: borrar una regla necesita administrador.
func (r *reglaProxy) pideAdmin() bool { return true }
