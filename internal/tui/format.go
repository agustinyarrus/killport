package tui

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Formato es-AR: miles con punto, decimales con coma, espacio antes del "%".

// Int agrupa miles: 1234567 → "1.234.567".
func Int(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	if len(s) > 3 {
		var b strings.Builder
		lead := len(s) % 3
		if lead > 0 {
			b.WriteString(s[:lead])
		}
		for i := lead; i < len(s); i += 3 {
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(s[i : i+3])
		}
		s = b.String()
	}
	if neg {
		return "-" + s
	}
	return s
}

// Fixed escribe f con prec decimales y coma decimal: Fixed(3.14159, 2) → "3,14".
func Fixed(f float64, prec int) string {
	s := strconv.FormatFloat(f, 'f', prec, 64)
	intPart, frac, hasFrac := strings.Cut(s, ".")
	neg := strings.HasPrefix(intPart, "-")
	intPart = strings.TrimPrefix(intPart, "-")
	v, err := strconv.ParseInt(intPart, 10, 64)
	grouped := intPart
	if err == nil {
		grouped = Int(v)
	}
	if neg && (v != 0 || strings.Trim(frac, "0") != "") {
		grouped = "-" + grouped
	}
	if hasFrac {
		return grouped + "," + frac
	}
	return grouped
}

// sig3 elige los decimales para mostrar ~3 cifras significativas.
func sig3(v float64) int {
	switch {
	case v < 10:
		return 2
	case v < 100:
		return 1
	}
	return 0
}

// Bytes usa unidades DECIMALES (1 MB = 1.000.000 B), las mismas con las que los
// servicios anuncian sus límites de subida: "25 MB" en Gmail son 25 millones.
func Bytes(n int64) string {
	if n < 0 {
		return "-" + Bytes(-n)
	}
	if n < 1000 {
		return Int(n) + " B"
	}
	units := [...]string{"KB", "MB", "GB", "TB", "PB"}
	v := float64(n)
	for _, u := range units {
		v /= 1000
		if v < 999.5 || u == "PB" {
			return Fixed(v, sig3(v)) + " " + u
		}
	}
	return Int(n) + " B"
}

// Duration abrevia según la escala: 412 ms · 3,24 s · 42,1 s · 3 min 07 s · 2 h 13 min.
func Duration(d time.Duration) string {
	switch {
	case d < 0:
		return "-" + Duration(-d)
	case d < time.Millisecond:
		return Fixed(float64(d)/float64(time.Millisecond), 2) + " ms"
	case d < time.Second:
		return strconv.Itoa(int(d/time.Millisecond)) + " ms"
	case d < time.Minute:
		s := d.Seconds()
		return Fixed(s, sig3(s)) + " s"
	case d < time.Hour:
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		return strconv.Itoa(m) + " min " + pad2(s) + " s"
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	return strconv.Itoa(h) + " h " + pad2(m) + " min"
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// Clock escribe una duración como reloj: 1:04:09 o 3:07.
func Clock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Round(time.Second) / time.Second)
	h, m, s := total/3600, (total/60)%60, total%60
	if h > 0 {
		return strconv.Itoa(h) + ":" + pad2(m) + ":" + pad2(s)
	}
	return strconv.Itoa(m) + ":" + pad2(s)
}

// Percent: 0.7512 → "75 %"; con prec decimales si se pide.
func Percent(frac float64, prec int) string {
	if math.IsNaN(frac) || math.IsInf(frac, 0) {
		return "— %"
	}
	return Fixed(frac*100, prec) + " %"
}

// Plural elige la forma según n: Plural(1, "archivo", "archivos").
func Plural(n int64, one, many string) string {
	if n == 1 || n == -1 {
		return one
	}
	return many
}

// Count une número y sustantivo: Count(12, "archivo", "archivos") → "12 archivos".
func Count(n int64, one, many string) string {
	return Int(n) + " " + Plural(n, one, many)
}

// Ago describe cuánto hace que pasó algo: "hace 2 h 13 min".
func Ago(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	if d < time.Second {
		return "recién"
	}
	if d >= 48*time.Hour {
		days := int(d / (24 * time.Hour))
		return "hace " + strconv.Itoa(days) + " días"
	}
	return "hace " + Duration(d.Truncate(time.Second))
}

// Rate escribe una tasa en bytes por segundo: "84,2 MB/s".
func Rate(bytes int64, d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	return Bytes(int64(float64(bytes)/d.Seconds())) + "/s"
}
