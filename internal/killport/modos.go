//go:build windows

package killport

// modos.go: killport hace cuatro cosas distintas con la misma línea de
// comandos —cerrar (o listar), buscar un puerto libre (--free), esperar
// (--wait, --wait-free) y configurar la red (--reserve, --unreserve,
// --fix-dynamic)— y cada una lleva sus flags. Un flag de un modo en otro no
// se ignora en silencio: `killport 3000 --wait 30s -y` no cierra nada, y
// callar que el -y no hizo nada es dejar que el usuario crea que sí. Acá se
// decide qué va con qué, en un solo lugar, con el nombre exacto de lo que
// sobra.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/agustinyarrus/killport/internal/cli"
	"github.com/agustinyarrus/killport/internal/tui"
)

// esperaMaxima es el tope de --wait, --wait-free y --stable. La espera sin
// fin no existe: un script que espera para siempre a un servidor que nunca
// arrancó es un script colgado.
const esperaMaxima = 30 * time.Minute

// plazoEspera interpreta un plazo de espera (cli.ParseTimecode: 60, 60s, 1:30,
// 750ms…) y lo acota.
func plazoEspera(v string) (time.Duration, error) {
	d, err := cli.ParseTimecode(v)
	if err != nil {
		return 0, err
	}
	if d > esperaMaxima {
		return 0, fmt.Errorf("el máximo es 30 min (vino %s): la espera sin fin no existe; si hace falta más, repetí el comando", v)
	}
	return d, nil
}

// plazoProlijo interpreta --timeout, la espera del cierre prolijo. Cero no
// es un plazo: "no esperar nada" es terminar en seco, y eso es --force (el
// motor tomaba el 0 como "no dado" y esperaba los 3 s de siempre, y lo mismo
// el docker stop, el script de WSL y el trabajo elevado).
func plazoProlijo(v string) (time.Duration, error) {
	d, err := cli.ParseTimecode(v)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, errors.New("tiene que ser mayor que cero: para terminar en seco, sin pedirle que cierre, usá --force")
	}
	return d, nil
}

// cantidadLibres interpreta --count: de 1 a 100.
func cantidadLibres(v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 1, fmt.Errorf("%q no es un número entero", v)
	}
	if n < 1 || n > 100 {
		return 1, fmt.Errorf("tiene que estar entre 1 y 100 (vino %d)", n)
	}
	return n, nil
}

// bandera es un flag de la línea de comandos y si se usó.
type bandera struct {
	nombre string
	puesta bool
}

// deCerrar son los flags que solo tienen sentido al cerrar o al listar.
func (o opciones) deCerrar() []bandera {
	return []bandera{
		{"--list", o.list}, {"--yes", o.yes}, {"--force", o.force}, {"--tree", o.tree}, {"--manual", o.manual},
		{"--timeout", o.timeoutDado}, {"--system", o.system}, {"--wsl", o.wsl}, {"--no-docker", o.noDocker},
		{"--orphans", o.sel.huerfanos}, {"--name", len(o.sel.nombres) > 0}, {"--project", o.sel.carpeta != ""},
	}
}

// primera devuelve el nombre del primer flag puesto, o "".
func primera(bs []bandera) string {
	for _, b := range bs {
		if b.puesta {
			return b.nombre
		}
	}
	return ""
}

// validarModos revisa que los flags pedidos vayan juntos. pos son los
// posicionales. Es el ÚNICO lugar que decide qué flag va con qué modo (Main
// solo despacha; validarConfig mira los puertos de la configuración): un flag
// que no hace nada en el modo que se eligió es un error de línea de comandos,
// no algo que se ignora en silencio.
func validarModos(pos []string, o opciones) error {
	espera := o.waitDado || o.waitFreeDado
	config := o.reserve || o.unreserve || o.fixDynamic
	modos := []bandera{{"--free", o.free}, {"--wait", o.waitDado}, {"--wait-free", o.waitFreeDado}, {"--reserve", o.reserve},
		{"--unreserve", o.unreserve}, {"--fix-dynamic", o.fixDynamic}}
	var puestos []string
	for _, m := range modos {
		if m.puesta {
			puestos = append(puestos, m.nombre)
		}
	}
	switch {
	case o.waitDado && o.waitFreeDado:
		return errors.New("--wait y --wait-free no van juntos: uno espera que escuche y el otro que se libere")
	case o.free && (espera || config):
		return fmt.Errorf("%s y %s no van juntos: son cosas distintas", puestos[0], puestos[1])
	case espera && config:
		return fmt.Errorf("%s no va con %s: esperar no cambia la red", puestos[0], puestos[1])
	}

	// Los flags de un modo, fuera de él.
	switch {
	case o.countDado && !o.free:
		return errors.New("--count va con --free (killport --free 3000 --count 3)")
	case !espera && (o.any || o.stableDado || o.nuevos):
		que := primera([]bandera{{"--any", o.any}, {"--stable", o.stableDado}, {"--new", o.nuevos}})
		return fmt.Errorf("%s va con --wait o --wait-free (ej.: %s)", que, ejemploEspera[que])
	case o.nuevos && o.waitFreeDado:
		return errors.New("--new va con --wait: al esperar que se libere no hay nada nuevo que esperar")
	}

	switch {
	case o.free:
		if f := primera(append(o.deCerrar(), bandera{"--all-states", o.allStates})); f != "" {
			return fmt.Errorf("--free busca un puerto libre y no cierra ni lista nada: no va con %s", f)
		}
	case espera:
		que := "--wait"
		if o.waitFreeDado {
			que = "--wait-free"
		}
		if f := primera(o.deCerrar()); f != "" {
			return fmt.Errorf("%s espera sin cerrar nada: no va con %s", que, f)
		}
		if o.allStates && o.waitDado {
			return errors.New("--all-states va con --wait-free (hasta que no quede ni una conexión); --wait espera a alguien que ESCUCHE")
		}
		if len(pos) == 0 {
			return fmt.Errorf("%s necesita los puertos: killport 5173 %s 60s", que, que)
		}
		if o.waitFreeDado && o.stable > o.waitFree {
			return fmt.Errorf("--stable %s no entra en --wait-free %s: nunca se cumpliría", textoPlazo(o.stable), textoPlazo(o.waitFree))
		}
	case config:
		if o.sel.activa() {
			return errors.New("--reserve, --unreserve y --fix-dynamic van con puertos, no con --orphans, --name ni --project")
		}
		ajenos := []bandera{{"--force", o.force}, {"--tree", o.tree}, {"--all-states", o.allStates}, {"--manual", o.manual},
			{"--wsl", o.wsl}, {"--system", o.system}, {"--no-docker", o.noDocker}, {"--timeout", o.timeoutDado}}
		if f := primera(ajenos); f != "" {
			return fmt.Errorf("--reserve, --unreserve y --fix-dynamic configuran la red y no cierran nada: no van con %s", f)
		}
	case len(pos) == 0 && !o.sel.activa():
		// El listado general: mirar. Lo de cerrar no tiene a quién.
		if o.manual {
			return errors.New("--manual necesita el puerto del servicio: killport 3306 --manual")
		}
		if f := primera([]bandera{{"--yes", o.yes}, {"--force", o.force}, {"--tree", o.tree}, {"--timeout", o.timeoutDado}}); f != "" {
			return fmt.Errorf("sin puertos killport solo lista lo que escucha: %s va con un puerto (killport 3000 %s)", f, f)
		}
	default:
		// Cerrar (o mirar) puertos.
		if o.list {
			if f := primera([]bandera{{"--yes", o.yes}, {"--force", o.force}, {"--timeout", o.timeoutDado}}); f != "" {
				return fmt.Errorf("--list solo mira: no va con %s", f)
			}
		}
		if o.wsl {
			return errors.New("--wsl es del listado (killport --wsl): con puertos, killport ya mira dentro de WSL cuando un reenvío lo tiene")
		}
		if o.system && !o.sel.activa() {
			return errors.New("--system es del listado y de -n/--project: con puertos, killport ya mira todo lo que los ocupa")
		}
	}
	return nil
}

// ejemploEspera es cómo se usa cada flag de la espera.
var ejemploEspera = map[string]string{
	"--any":    "killport 3000 8080 --wait 60s --any",
	"--stable": "killport 5173 --wait 60s --stable 750ms",
	"--new":    "killport 5173 --wait 60s --new",
}

// textoPlazo escribe un plazo como tui.Duration pero sin ceros de más: el
// usuario escribió "5s", no "5,00 s".
func textoPlazo(d time.Duration) string {
	s := tui.Duration(d)
	num, unidad, ok := strings.Cut(s, " ")
	if !ok || !strings.Contains(num, ",") || strings.Contains(unidad, " ") {
		return s
	}
	return strings.TrimSuffix(strings.TrimRight(num, "0"), ",") + " " + unidad
}
