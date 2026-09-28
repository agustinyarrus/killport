package killport

// seleccion.go: elegir objetivos sin nombrar puertos. `taskkill /f /im
// node.exe` se lleva puestos todos los node —el language server, el CLI de
// otra pestaña—; acá se eligen solo los que ESCUCHAN un puerto y cumplen:
//
//	--orphans         quedaron sueltos: su padre murió (reaper.Orphans)
//	-n, --name N      su imagen se llama así (sin distinguir mayúsculas, con o
//	                  sin .exe, con comodines) o su ruta coincide con un patrón
//	--project DIR     su carpeta de trabajo (o la de su script) está dentro de DIR
//
// Los criterios se INTERSECAN entre sí y con los puertos, si se nombran:
// `killport --orphans -n node` son los node huérfanos; `killport 3000-3999
// --project .` es lo de este proyecto en esos puertos. Cada criterio achica.
// Lo que comparte un puerto con lo elegido y no cumple queda al margen
// (reaper.Focus): no se toca, y se dice que sigue ahí.
//
// Puro: los datos de cada proceso (carpeta, proyecto, si es huérfano) los da
// quien llama.

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// seleccion es lo que pidió el usuario además de (o en vez de) los puertos.
type seleccion struct {
	huerfanos bool
	nombres   []string // patrones de -n, uno por elemento
	carpeta   string   // --project, absoluta y limpia ("" = no)
}

// activa dice si hay algún selector.
func (s seleccion) activa() bool { return s.huerfanos || len(s.nombres) > 0 || s.carpeta != "" }

// describir nombra lo elegido para los mensajes: "huérfanos, node y de
// C:\dev\shop".
func (s seleccion) describir() string {
	var parts []string
	if s.huerfanos {
		parts = append(parts, "huérfanos")
	}
	if len(s.nombres) > 0 {
		parts = append(parts, unirY(s.nombres))
	}
	if s.carpeta != "" {
		parts = append(parts, "de "+s.carpeta)
	}
	return strings.Join(parts, ", ")
}

// sumarNombres agrega el valor de un -n: uno o varios patrones separados por
// comas. Un patrón mal formado es un error de línea de comandos.
func (s *seleccion) sumarNombres(v string) error {
	n := 0
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		if _, err := path.Match(strings.ToLower(strings.ReplaceAll(p, `\`, "/")), ""); err != nil {
			return fmt.Errorf("%q no es un patrón válido (usá * y ?, como en node*)", p)
		}
		s.nombres = append(s.nombres, p)
		n++
	}
	if n == 0 {
		return errors.New("falta el nombre del programa (ej.: -n node, -n node,python)")
	}
	return nil
}

// coincideNombre dice si un proceso coincide con un patrón de -n. Sin barras,
// el patrón es un NOMBRE de imagen: sin distinguir mayúsculas, y "node" vale
// por "node.exe"; admite comodines ("python*"). Con barras, es una RUTA con
// comodines contra la ruta completa del ejecutable: cada * vale dentro de
// una carpeta, ** vale cualquier cantidad de carpetas, y un patrón que
// termina en \* es "cualquier cosa debajo de esa carpeta" (C:\dev\*).
func coincideNombre(patron, nombre, ruta string) bool {
	p := strings.ToLower(strings.ReplaceAll(patron, `\`, "/"))
	if !strings.Contains(p, "/") {
		n := strings.ToLower(nombre)
		for _, cand := range []string{n, strings.TrimSuffix(n, ".exe")} {
			if ok, _ := path.Match(p, cand); ok {
				return true
			}
		}
		return false
	}
	if ruta == "" {
		return false
	}
	return coincideRuta(strings.Split(p, "/"), strings.Split(strings.ToLower(strings.ReplaceAll(ruta, `\`, "/")), "/"))
}

// coincideRuta compara componente por componente. O(p·r) en el peor caso
// (por **), con p y r cortos.
func coincideRuta(pat, ruta []string) bool {
	for i, pc := range pat {
		if pc == "**" {
			for j := i; j <= len(ruta); j++ {
				if coincideRuta(pat[i+1:], ruta[j:]) {
					return true
				}
			}
			return false
		}
		if i >= len(ruta) {
			return false
		}
		if ok, _ := path.Match(pc, ruta[i]); !ok {
			return false
		}
		if i == len(pat)-1 && pc == "*" {
			return true // "C:\dev\*": cualquier cosa debajo
		}
	}
	return len(pat) == len(ruta)
}

// noEsProyecto dice por qué dir NO sirve de carpeta para --project, o "" si
// sirve. La raíz de una unidad (o de un recurso compartido), el perfil del
// usuario y cualquier carpeta de las que lo contienen, y la carpeta de
// Windows no son un proyecto: es donde abre la terminal por defecto, y
// `killport --project . -y` desde ahí elegía media máquina (Discord, el
// agente de NVIDIA, los CLI que corren en otras pestañas) y la cerraba sin
// preguntar. home y systemRoot vacíos no cuentan. Pura.
func noEsProyecto(dir, home, systemRoot string) string {
	d := strings.TrimRight(strings.ReplaceAll(dir, "/", `\`), `\`)
	switch {
	case esRaiz(d):
		return d + `\ es la raíz de una unidad`
	case strings.HasPrefix(d, `\\`) && strings.Count(strings.Trim(d, `\`), `\`) <= 1:
		return d + " es la raíz de un recurso compartido"
	case home != "" && mismaRuta(d, home):
		return d + " es tu carpeta de usuario"
	case home != "" && dentroDe(home, d):
		return d + " contiene tu carpeta de usuario"
	case systemRoot != "" && dentroDe(d, systemRoot):
		return d + " es la carpeta de Windows"
	}
	return ""
}

// dentroDe dice si la carpeta p es dir o está debajo (sin distinguir
// mayúsculas, en un límite de carpeta).
func dentroDe(p, dir string) bool {
	if p == "" || dir == "" {
		return false
	}
	p = strings.TrimRight(strings.ReplaceAll(p, "/", `\`), `\`)
	dir = strings.TrimRight(strings.ReplaceAll(dir, "/", `\`), `\`)
	if strings.EqualFold(p, dir) {
		return true
	}
	_, ok := debajoDe(p, dir)
	return ok
}

// candidato es lo que la selección necesita saber de un dueño.
type candidato struct {
	nombre, ruta string
	cwd          string    // su carpeta de trabajo ("" si no se supo)
	proyecto     *proyecto // su proyecto, si tiene
	huerfano     bool      // quedó suelto y se ofrece (de desarrollo, o con --system)
}

// cumple dice si un dueño cumple TODOS los criterios pedidos.
func (s seleccion) cumple(c candidato) bool {
	if s.huerfanos && !c.huerfano {
		return false
	}
	if len(s.nombres) > 0 {
		ok := false
		for _, p := range s.nombres {
			ok = ok || coincideNombre(p, c.nombre, c.ruta)
		}
		if !ok {
			return false
		}
	}
	if s.carpeta != "" {
		enCarpeta := dentroDe(c.cwd, s.carpeta)
		if !enCarpeta && c.proyecto != nil && c.proyecto.PorScript {
			// Lanzado desde otro lado con un script del proyecto: es del
			// proyecto (lo mismo que dice la columna PROYECTO).
			enCarpeta = dentroDe(c.proyecto.Raiz, s.carpeta)
		}
		if !enCarpeta {
			return false
		}
	}
	return true
}
