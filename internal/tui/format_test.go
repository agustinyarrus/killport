package tui

import (
	"testing"
	"time"
)

func TestInt(t *testing.T) {
	casos := map[int64]string{0: "0", 7: "7", 999: "999", 1000: "1.000", 1234567: "1.234.567", -1234: "-1.234"}
	for in, want := range casos {
		if got := Int(in); got != want {
			t.Errorf("Int(%d) = %q, esperaba %q", in, got, want)
		}
	}
}

func TestFixed(t *testing.T) {
	casos := []struct {
		f    float64
		prec int
		want string
	}{
		{3.14159, 2, "3,14"},
		{1234.5, 1, "1.234,5"},
		{-0.04, 1, "0,0"}, // sin "-0,0": un cero no tiene signo
		{-12.5, 1, "-12,5"},
		{0, 0, "0"},
	}
	for _, c := range casos {
		if got := Fixed(c.f, c.prec); got != c.want {
			t.Errorf("Fixed(%v, %d) = %q, esperaba %q", c.f, c.prec, got, c.want)
		}
	}
}

func TestBytes(t *testing.T) {
	casos := map[int64]string{
		0:          "0 B",
		999:        "999 B",
		1000:       "1,00 KB",
		38_200:     "38,2 KB",
		211_000:    "211 KB",
		999_500:    "1,00 MB", // nunca "1.000 KB": sube de unidad
		9_810_000:  "9,81 MB",
		25_000_000: "25,0 MB",
		-38_200:    "-38,2 KB",
		3e12:       "3,00 TB",
	}
	for in, want := range casos {
		if got := Bytes(in); got != want {
			t.Errorf("Bytes(%d) = %q, esperaba %q", in, got, want)
		}
	}
}

func TestDuration(t *testing.T) {
	casos := map[time.Duration]string{
		400 * time.Microsecond:           "0,40 ms",
		412 * time.Millisecond:           "412 ms",
		3240 * time.Millisecond:          "3,24 s",
		42100 * time.Millisecond:         "42,1 s",
		3*time.Minute + 7*time.Second:    "3 min 07 s",
		2*time.Hour + 13*time.Minute:     "2 h 13 min",
		-(3*time.Minute + 7*time.Second): "-3 min 07 s",
	}
	for in, want := range casos {
		if got := Duration(in); got != want {
			t.Errorf("Duration(%v) = %q, esperaba %q", in, got, want)
		}
	}
}

func TestClockPercentCount(t *testing.T) {
	if got := Clock(3*time.Minute + 7*time.Second); got != "3:07" {
		t.Errorf("Clock = %q", got)
	}
	if got := Clock(time.Hour + 4*time.Minute + 9*time.Second); got != "1:04:09" {
		t.Errorf("Clock = %q", got)
	}
	if got := Clock(-time.Second); got != "0:00" {
		t.Errorf("Clock negativo = %q, esperaba 0:00", got)
	}
	if got := Percent(0.7512, 0); got != "75 %" {
		t.Errorf("Percent = %q", got)
	}
	if got := Count(1, "archivo", "archivos"); got != "1 archivo" {
		t.Errorf("Count(1) = %q", got)
	}
	if got := Count(1200, "archivo", "archivos"); got != "1.200 archivos" {
		t.Errorf("Count(1200) = %q", got)
	}
}
