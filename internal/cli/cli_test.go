package cli

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type opts struct {
	out   string
	force bool
	rec   bool
	jobs  int
	mode  string
	pw    []string
}

func newApp() (*App, *opts) {
	o := &opts{jobs: 4, mode: "fast"}
	a := New("prueba", "1.0.0", "una herramienta de prueba")
	a.String(&o.out, "out", 'o', "RUTA", "salida")
	a.Bool(&o.force, "force", 'f', "pisar")
	a.Bool(&o.rec, "recursive", 'r', "recursivo")
	a.Int(&o.jobs, "jobs", 'j', "N", "hilos", 1, 64)
	a.Enum(&o.mode, "mode", 0, "modo", "fast", "slow")
	a.Strings(&o.pw, "password", 'p', "CLAVE", "claves")
	return a, o
}

func TestParseFormasGNU(t *testing.T) {
	a, o := newApp()
	pos, err := a.Parse([]string{"a.webp", "--out=x", "-f", "b.webp", "-r"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pos, []string{"a.webp", "b.webp"}) || o.out != "x" || !o.force || !o.rec {
		t.Errorf("pos=%v out=%q force=%v rec=%v", pos, o.out, o.force, o.rec)
	}
}

func TestParseCortosAgrupados(t *testing.T) {
	a, o := newApp()
	if _, err := a.Parse([]string{"-fr", "-oy", "-j8"}); err != nil {
		t.Fatal(err)
	}
	if !o.force || !o.rec || o.out != "y" || o.jobs != 8 {
		t.Errorf("force=%v rec=%v out=%q jobs=%d", o.force, o.rec, o.out, o.jobs)
	}
}

func TestParseDobleGuion(t *testing.T) {
	a, o := newApp()
	pos, err := a.Parse([]string{"-o", "z", "--", "-f", "--out"})
	if err != nil {
		t.Fatal(err)
	}
	if o.force || o.out != "z" || !reflect.DeepEqual(pos, []string{"-f", "--out"}) {
		t.Errorf("después de -- todo es posicional: pos=%v force=%v", pos, o.force)
	}
}

func TestParseNegacionYRepetibles(t *testing.T) {
	a, o := newApp()
	o.force = true
	if _, err := a.Parse([]string{"--no-force", "-p", "a", "--password=b"}); err != nil {
		t.Fatal(err)
	}
	if o.force || !reflect.DeepEqual(o.pw, []string{"a", "b"}) {
		t.Errorf("force=%v pw=%v", o.force, o.pw)
	}
}

func TestParseNumeroNegativoEsPosicional(t *testing.T) {
	a, _ := newApp()
	pos, err := a.Parse([]string{"-5"})
	if err != nil || !reflect.DeepEqual(pos, []string{"-5"}) {
		t.Errorf("pos=%v err=%v", pos, err)
	}
}

func TestParseErrores(t *testing.T) {
	casos := []struct {
		args    []string
		contain string
	}{
		{[]string{"--froce"}, "¿quisiste decir --force?"},
		{[]string{"--out"}, "necesita un valor"},
		{[]string{"-o"}, "necesita un valor"},
		{[]string{"--jobs", "0"}, "entre 1 y 64"},
		{[]string{"-j", "abc"}, "no es un número entero"},
		{[]string{"--mode", "fsat"}, `¿quisiste decir "fast"?`},
		{[]string{"--force=tal-vez"}, "no es sí/no"},
	}
	for _, c := range casos {
		a, _ := newApp()
		_, err := a.Parse(c.args)
		if err == nil || !strings.Contains(err.Error(), c.contain) {
			t.Errorf("Parse(%v): error = %v, esperaba que diga %q", c.args, err, c.contain)
		}
	}
}

func TestParseEnumNormaliza(t *testing.T) {
	a, o := newApp()
	if _, err := a.Parse([]string{"--mode", " SLOW "}); err != nil || o.mode != "slow" {
		t.Errorf("mode=%q err=%v", o.mode, err)
	}
}

func TestParseBuiltins(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"/?"}, {"-?"}} {
		a, _ := newApp()
		if _, err := a.Parse(args); !errors.Is(err, ErrHelp) {
			t.Errorf("Parse(%v) = %v, esperaba ErrHelp", args, err)
		}
	}
	a, _ := newApp()
	if _, err := a.Parse([]string{"-V"}); !errors.Is(err, ErrVersion) {
		t.Errorf("-V = %v, esperaba ErrVersion", err)
	}
	a, _ = newApp()
	pos, err := a.Parse([]string{"--no-color", "x"})
	if err != nil || !a.NoColor || !reflect.DeepEqual(pos, []string{"x"}) {
		t.Errorf("--no-color: pos=%v NoColor=%v err=%v", pos, a.NoColor, err)
	}
}

func TestParseSize(t *testing.T) {
	casos := map[string]int64{
		"25MB": 25_000_000, "25 mb": 25_000_000, "8M": 8_000_000, "1,5GB": 1_500_000_000,
		"1.5G": 1_500_000_000, "10MiB": 10 << 20, "500k": 500_000, "100": 100, "2KiB": 2048,
	}
	for in, want := range casos {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; esperaba %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "5XB", "-3MB"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) tendría que fallar", bad)
		}
	}
}

func TestParseTimecode(t *testing.T) {
	casos := map[string]time.Duration{
		"90":         90 * time.Second,
		"1:30":       90 * time.Second,
		"01:02:03.5": time.Hour + 2*time.Minute + 3500*time.Millisecond,
		"1m30s":      90 * time.Second,
		"2h":         2 * time.Hour,
		"0,5":        500 * time.Millisecond,
	}
	for in, want := range casos {
		got, err := ParseTimecode(in)
		if err != nil || got != want {
			t.Errorf("ParseTimecode(%q) = %v, %v; esperaba %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "1:2:3:4", "-5", "ayer", "1:xx"} {
		if _, err := ParseTimecode(bad); err == nil {
			t.Errorf("ParseTimecode(%q) tendría que fallar", bad)
		}
	}
}
