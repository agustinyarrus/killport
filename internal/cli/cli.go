// Package cli interpreta la línea de comandos con la convención GNU (-o x,
// --out=x, -abc, --, --no-flag) y genera una ayuda con el mismo lenguaje
// visual que el resto de la salida. Los valores se validan UNA vez, acá, en la
// frontera: adentro de cada herramienta ya son del tipo correcto.
package cli

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/agustinyarrus/killport/internal/textdist"
	"github.com/agustinyarrus/killport/internal/tui"
)

// Códigos de salida de killport (los mismos con --json).
const (
	ExitOK          = 0   // todo salió bien
	ExitPartial     = 1   // algunos elementos fallaron
	ExitUsage       = 2   // línea de comandos inválida
	ExitFailure     = 3   // no se pudo hacer nada
	ExitInterrupted = 130 // Ctrl+C (128 + SIGINT, la convención de las shells)
)

// Señales de Parse que no son errores sino pedidos.
var (
	ErrHelp    = errors.New("pidió la ayuda")
	ErrVersion = errors.New("pidió la versión")
)

// Example es una línea de la sección de ejemplos.
type Example struct{ Cmd, Desc string }

type flag struct {
	long, arg, help, def string
	short                rune
	isBool, hidden       bool
	set                  func(string) error
}

type section struct {
	title string
	flags []*flag
}

// App describe una herramienta: qué es, cómo se usa y qué flags acepta.
type App struct {
	Name, Version, Tagline string
	Usage                  []string
	Examples               []Example
	Notes                  []string
	NoColor                bool

	sections []*section
	long     map[string]*flag
	short    map[rune]*flag
}

// New arma la App y registra los flags generales (-h, -V, --no-color).
func New(name, version, tagline string) *App {
	a := &App{Name: name, Version: version, Tagline: tagline, long: map[string]*flag{}, short: map[rune]*flag{}}
	return a
}

// Section abre un grupo de flags en la ayuda; los que se registren después caen ahí.
func (a *App) Section(title string) {
	a.sections = append(a.sections, &section{title: title})
}

func (a *App) add(f *flag) {
	if _, dup := a.long[f.long]; dup {
		panic("cli: flag duplicado --" + f.long) // error de programación, no del user
	}
	if f.short != 0 {
		if _, dup := a.short[f.short]; dup {
			panic("cli: flag duplicado -" + string(f.short))
		}
		a.short[f.short] = f
	}
	a.long[f.long] = f
	if f.hidden {
		return
	}
	if len(a.sections) == 0 {
		a.Section("opciones")
	}
	s := a.sections[len(a.sections)-1]
	s.flags = append(s.flags, f)
}

// Bool registra un interruptor. Acepta --x, --no-x y --x=false.
func (a *App) Bool(p *bool, long string, short rune, help string) {
	a.add(&flag{long: long, short: short, help: help, isBool: true, set: func(v string) error {
		b, err := parseBool(v)
		if err == nil {
			*p = b
		}
		return err
	}})
}

// String registra un texto libre.
func (a *App) String(p *string, long string, short rune, arg, help string) {
	a.add(&flag{long: long, short: short, arg: arg, help: help, def: *p, set: func(v string) error {
		*p = v
		return nil
	}})
}

// Strings registra un flag repetible que acumula valores.
func (a *App) Strings(p *[]string, long string, short rune, arg, help string) {
	a.add(&flag{long: long, short: short, arg: arg, help: help, set: func(v string) error {
		*p = append(*p, v)
		return nil
	}})
}

// Int registra un entero acotado a [lo, hi].
func (a *App) Int(p *int, long string, short rune, arg, help string, lo, hi int) {
	a.add(&flag{long: long, short: short, arg: arg, help: help, def: strconv.Itoa(*p), set: func(v string) error {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("%q no es un número entero", v)
		}
		if n < lo || n > hi {
			return fmt.Errorf("tiene que estar entre %d y %d (vino %d)", lo, hi, n)
		}
		*p = n
		return nil
	}})
}

// Float registra un número real acotado; acepta coma o punto decimal.
func (a *App) Float(p *float64, long string, short rune, arg, help string, lo, hi float64) {
	a.add(&flag{long: long, short: short, arg: arg, help: help, def: tui.Fixed(*p, -1), set: func(v string) error {
		f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(v), ",", "."), 64)
		if err != nil {
			return fmt.Errorf("%q no es un número", v)
		}
		if f < lo || f > hi {
			return fmt.Errorf("tiene que estar entre %s y %s", tui.Fixed(lo, -1), tui.Fixed(hi, -1))
		}
		*p = f
		return nil
	}})
}

// Enum registra una elección cerrada; el error sugiere la opción más cercana.
func (a *App) Enum(p *string, long string, short rune, help string, choices ...string) {
	a.add(&flag{long: long, short: short, arg: strings.Join(choices, "|"), help: help, def: *p, set: func(v string) error {
		v = strings.ToLower(strings.TrimSpace(v))
		for _, c := range choices {
			if v == c {
				*p = c
				return nil
			}
		}
		if s := textdist.Closest(v, choices); s != "" {
			return fmt.Errorf("%q no es una opción; ¿quisiste decir %q? (%s)", v, s, strings.Join(choices, ", "))
		}
		return fmt.Errorf("%q no es una opción (%s)", v, strings.Join(choices, ", "))
	}})
}

// Func registra un flag con un intérprete propio.
func (a *App) Func(long string, short rune, arg, help, def string, fn func(string) error) {
	a.add(&flag{long: long, short: short, arg: arg, help: help, def: def, set: fn})
}

// Hidden registra un flag interno que no aparece en la ayuda.
func (a *App) Hidden(long, arg string, fn func(string) error) {
	a.add(&flag{long: long, arg: arg, hidden: true, set: fn})
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "1", "true", "t", "si", "sí", "s", "yes", "y", "on":
		return true, nil
	case "0", "false", "f", "no", "n", "off":
		return false, nil
	}
	return false, fmt.Errorf("%q no es sí/no", v)
}

func isNumber(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// Parse consume los argumentos y devuelve los posicionales en orden.
// Convención GNU: flags y posicionales se pueden intercalar; "--" corta.
func (a *App) Parse(args []string) ([]string, error) {
	var pos []string
	for i := 0; i < len(args); i++ {
		s := args[i]
		switch {
		case s == "--":
			return append(pos, args[i+1:]...), nil
		case strings.HasPrefix(s, "--"):
			name, val, hasVal := strings.Cut(s[2:], "=")
			switch name {
			case "help":
				return nil, ErrHelp
			case "version":
				return nil, ErrVersion
			case "no-color":
				a.NoColor = true
				continue
			}
			f := a.long[name]
			if f == nil && strings.HasPrefix(name, "no-") && !hasVal {
				if g := a.long[name[3:]]; g != nil && g.isBool {
					if err := g.set("false"); err != nil {
						return nil, err
					}
					continue
				}
			}
			if f == nil {
				return nil, a.unknown("--" + name)
			}
			if f.isBool {
				if !hasVal {
					val = "true"
				}
				if err := f.set(val); err != nil {
					return nil, fmt.Errorf("--%s: %w", name, err)
				}
				continue
			}
			if !hasVal {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("--%s necesita un valor (%s)", name, f.arg)
				}
				i++
				val = args[i]
			}
			if err := f.set(val); err != nil {
				return nil, fmt.Errorf("--%s: %w", name, err)
			}
		case len(s) > 1 && s[0] == '-' && !isNumber(s):
			consumedNext, err := a.shortCluster(s[1:], args, i)
			if err != nil {
				return nil, err
			}
			if consumedNext {
				i++
			}
		case s == "/?":
			return nil, ErrHelp
		default:
			pos = append(pos, s)
		}
	}
	return pos, nil
}

// shortCluster interpreta "-abc" (varios interruptores) o "-ovalor" / "-o valor".
func (a *App) shortCluster(cluster string, args []string, i int) (consumedNext bool, err error) {
	runes := []rune(cluster)
	for j := 0; j < len(runes); j++ {
		r := runes[j]
		switch r {
		case 'h', '?':
			if a.short[r] == nil {
				return false, ErrHelp
			}
		case 'V':
			if a.short[r] == nil {
				return false, ErrVersion
			}
		}
		f := a.short[r]
		if f == nil {
			return false, a.unknown("-" + string(r))
		}
		if f.isBool {
			if err := f.set("true"); err != nil {
				return false, fmt.Errorf("-%c: %w", r, err)
			}
			continue
		}
		val := strings.TrimPrefix(string(runes[j+1:]), "=")
		if val == "" {
			if i+1 >= len(args) {
				return false, fmt.Errorf("-%c necesita un valor (%s)", r, f.arg)
			}
			val, consumedNext = args[i+1], true
		}
		if err := f.set(val); err != nil {
			return false, fmt.Errorf("-%c: %w", r, err)
		}
		return consumedNext, nil
	}
	return false, nil
}

func (a *App) unknown(flagText string) error {
	names := make([]string, 0, len(a.long)+3)
	for n, f := range a.long {
		if !f.hidden {
			names = append(names, "--"+n)
		}
	}
	names = append(names, "--help", "--version", "--no-color")
	sort.Strings(names)
	if s := textdist.Closest(flagText, names); s != "" {
		return fmt.Errorf("no conozco %s; ¿quisiste decir %s?", flagText, s)
	}
	// La pista de --help la agrega Start debajo del error: no dos veces.
	return fmt.Errorf("no conozco %s", flagText)
}
