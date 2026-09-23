package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/agustinyarrus/killport/internal/tui"
)

// Start es el arranque común de cada herramienta: interpreta los argumentos,
// resuelve --help / --version / --no-color y devuelve los posicionales. Si
// devuelve done=true, la herramienta tiene que salir con code.
func (a *App) Start(t *tui.Term, args []string) (pos []string, done bool, code int) {
	pos, err := a.Parse(args)
	if a.NoColor {
		t.DisableColor()
	}
	switch {
	case errors.Is(err, ErrHelp):
		t.Lines(a.Help(t))
		return nil, true, ExitOK
	case errors.Is(err, ErrVersion):
		t.Line(t.Paint(tui.Lavender, a.Name) + " " + t.Paint(tui.Subtle, a.Version))
		return nil, true, ExitOK
	case err != nil:
		t.Blank()
		t.Line(t.Paint(tui.Rose, "✗ ") + t.Paint(tui.Text, err.Error()))
		t.Line(t.Paint(tui.Faint, "› "+a.Name+" --help muestra todas las opciones"))
		t.Blank()
		return nil, true, ExitUsage
	}
	return pos, false, ExitOK
}

// Interrupt devuelve un contexto que se cancela con el primer Ctrl+C, para que
// la herramienta corte prolija (borra temporales, restaura la consola). El
// segundo Ctrl+C corta en seco por si algo no respeta la cancelación.
func Interrupt(onHardExit func()) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt)
	go func() {
		<-ch
		cancel()
		<-ch
		if onHardExit != nil {
			onHardExit()
		}
		os.Exit(ExitInterrupted)
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel()
	}
}

// ParseSize entiende tamaños como "25MB", "8 M", "1,5GB", "10MiB", "500k".
// Unidades decimales (K, M, G = 10³, 10⁶, 10⁹) salvo que se pidan binarias
// (KiB, MiB, GiB = 2¹⁰, 2²⁰, 2³⁰). Sin unidad, son bytes.
func ParseSize(s string) (int64, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return 0, errors.New("tamaño vacío")
	}
	i := 0
	for i < len(raw) && (raw[i] >= '0' && raw[i] <= '9' || raw[i] == '.' || raw[i] == ',') {
		i++
	}
	num, unit := raw[:i], strings.ToLower(strings.TrimSpace(raw[i:]))
	v, err := strconv.ParseFloat(strings.ReplaceAll(num, ",", "."), 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%q no es un tamaño (ej.: 25MB, 8M, 1,5GB)", s)
	}
	mult := map[string]float64{
		"": 1, "b": 1,
		"k": 1e3, "kb": 1e3, "m": 1e6, "mb": 1e6, "g": 1e9, "gb": 1e9, "t": 1e12, "tb": 1e12,
		"ki": 1 << 10, "kib": 1 << 10, "mi": 1 << 20, "mib": 1 << 20, "gi": 1 << 30, "gib": 1 << 30,
	}
	m, ok := mult[unit]
	if !ok {
		return 0, fmt.Errorf("unidad %q desconocida (B, KB, MB, GB, KiB, MiB, GiB)", unit)
	}
	return int64(v*m + 0.5), nil
}

// ParseTimecode entiende "90", "90s", "1:30", "01:02:03.5", "1m30s", "2h".
func ParseTimecode(s string) (time.Duration, error) {
	raw := strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if raw == "" {
		return 0, errors.New("tiempo vacío")
	}
	if strings.Contains(raw, ":") {
		parts := strings.Split(raw, ":")
		if len(parts) > 3 {
			return 0, fmt.Errorf("%q no es un tiempo (ej.: 1:30, 01:02:03.5)", s)
		}
		var total float64
		for _, p := range parts {
			v, err := strconv.ParseFloat(p, 64)
			if err != nil || v < 0 {
				return 0, fmt.Errorf("%q no es un tiempo (ej.: 1:30, 01:02:03.5)", s)
			}
			total = total*60 + v
		}
		return time.Duration(total * float64(time.Second)), nil
	}
	if unicode.IsDigit(rune(raw[len(raw)-1])) {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 {
			return 0, fmt.Errorf("%q no es un tiempo", s)
		}
		return time.Duration(v * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%q no es un tiempo (ej.: 90, 1:30, 1m30s)", s)
	}
	return d, nil
}
