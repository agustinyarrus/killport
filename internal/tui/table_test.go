package tui

import (
	"math/rand"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLayoutEntraTodo(t *testing.T) {
	cols := []Col{{Title: "A"}, {Title: "B", Flex: true}, {Title: "C", Drop: 1}}
	w, shown := layout(cols, []int{5, 20, 6}, 100, 3)
	if !reflect.DeepEqual(w, []int{5, 20, 6}) || !reflect.DeepEqual(shown, []bool{true, true, true}) {
		t.Fatalf("con lugar de sobra no se toca nada: %v %v", w, shown)
	}
}

// Llenado de agua: la Flex más ancha cede primero; la angosta no se toca
// hasta que el techo baja hasta ella.
func TestLayoutLaMasAnchaCedePrimero(t *testing.T) {
	cols := []Col{{Title: "P"}, {Title: "N", Flex: true, Min: 4}, {Title: "C", Flex: true, Min: 4}}
	// natural: 6 + 10 + 60 + 2 huecos de 3 = 82; ancho 50 → sobran 32 de Flex.
	w, _ := layout(cols, []int{6, 10, 60}, 50, 3)
	if w[0] != 6 || w[1] != 10 || w[2] != 50-6-10-6 {
		t.Fatalf("anchos %v: la angosta (10) no tenía que ceder", w)
	}
	// Más angosto todavía: las dos al mismo techo, y lo que sobra por
	// redondeo va a la más ancha.
	w, _ = layout(cols, []int{6, 10, 60}, 25, 3)
	if w[1]+w[2] != 25-6-6 || w[1] > w[2] || w[2]-w[1] > 1 {
		t.Fatalf("anchos %v: las dos Flex tenían que quedar a la par", w)
	}
}

// Las opcionales se van en orden de Drop, y solo si hace falta.
func TestLayoutSacaOpcionalesEnOrden(t *testing.T) {
	cols := []Col{
		{Title: "PUERTO"},
		{Title: "DIR", Drop: 1},
		{Title: "PROCESO", Flex: true, Min: 12},
		{Title: "DESDE", Drop: 2},
		{Title: "COMANDO", Flex: true, Min: 20, Middle: true},
	}
	nat := []int{6, 12, 20, 9, 150}
	w, shown := layout(cols, nat, 176, 3)
	if !shown[1] || !shown[3] || w[4] != 176-6-12-20-9-12 {
		t.Fatalf("a 176 entra todo con COMANDO recortado: %v %v", w, shown)
	}
	// 6+12+12+9+20 + 12 = 71 con las Flex en su Min: a 70 se va DIR (Drop 1).
	w, shown = layout(cols, nat, 70, 3)
	if shown[1] || !shown[3] {
		t.Fatalf("a 70 se va DIR y queda DESDE: %v", shown)
	}
	if got := 6 + w[2] + 9 + w[4] + 9; got != 70 {
		t.Fatalf("sacada DIR, las Flex vuelven a crecer hasta usar el ancho: %v (total %d)", w, got)
	}
	// 6+12+20 + 6 = 44 sin opcionales: a 45 se van las dos.
	_, shown = layout(cols, nat, 45, 3)
	if shown[1] || shown[3] || !shown[0] || !shown[2] || !shown[4] {
		t.Fatalf("a 45 se van las dos opcionales y nada más: %v", shown)
	}
	// Sin nada más para sacar, las Flex ceden por debajo de su Min.
	w, _ = layout(cols, nat, 30, 3)
	if got := w[0] + w[2] + w[4] + 6; got != 30 || w[2] < hardMin || w[4] < hardMin {
		t.Fatalf("a 30 las Flex ceden de más y la tabla entra justa: %v (total %d)", w, got)
	}
}

// Propiedades del reparto sobre tablas al azar (semilla fija).
func TestLayoutPropiedades(t *testing.T) {
	rng := rand.New(rand.NewSource(20260928))
	for caso := 0; caso < 20000; caso++ {
		k := 1 + rng.Intn(9)
		cols := make([]Col, k)
		nat := make([]int, k)
		for i := range cols {
			cols[i].Flex = rng.Intn(2) == 0
			if cols[i].Flex {
				cols[i].Min = rng.Intn(20)
			}
			if rng.Intn(3) == 0 {
				cols[i].Drop = 1 + rng.Intn(3)
			}
			nat[i] = 1 + rng.Intn(70)
		}
		width := rng.Intn(200)
		w, shown := layout(cols, nat, width, 3)

		total, n, cutAny := 0, 0, false
		for i := range cols {
			if !shown[i] {
				continue
			}
			n++
			total += w[i]
			switch {
			case !cols[i].Flex && w[i] != nat[i]:
				t.Fatalf("caso %d: una columna fija cambió de ancho: %v %v %v", caso, cols, nat, w)
			case cols[i].Flex && (w[i] > nat[i] || w[i] < min(nat[i], hardMin, minDe(cols[i]))):
				t.Fatalf("caso %d: Flex fuera de [piso, natural]: %v %v %v", caso, cols, nat, w)
			case cols[i].Flex && w[i] < nat[i]:
				cutAny = true
			}
		}
		total += 3 * max(n-1, 0)
		// Factible: con las Flex en el piso duro entra lo que se muestra.
		piso := 3 * max(n-1, 0)
		for i := range cols {
			if shown[i] {
				if cols[i].Flex {
					piso += min(nat[i], hardMin, minDe(cols[i]))
				} else {
					piso += nat[i]
				}
			}
		}
		if piso <= width {
			if total > width {
				t.Fatalf("caso %d: se pasó del ancho (%d > %d): %v %v %v %v", caso, total, width, cols, nat, w, shown)
			}
			if cutAny && total != width {
				t.Fatalf("caso %d: recortó y no usó el ancho entero (%d de %d)", caso, total, width)
			}
		}
		// Las ocultas son un prefijo del orden de Drop, y la última que se
		// sacó hacía falta sacarla.
		var hidden []int
		for i := range cols {
			if !shown[i] {
				if cols[i].Drop == 0 {
					t.Fatalf("caso %d: sacó una columna que no es opcional", caso)
				}
				hidden = append(hidden, i)
			}
		}
		for _, h := range hidden {
			for i := range cols {
				if shown[i] && cols[i].Drop > 0 && (cols[i].Drop < cols[h].Drop || (cols[i].Drop == cols[h].Drop && i > h)) {
					t.Fatalf("caso %d: sacó %d antes que %d, que va primero", caso, h, i)
				}
			}
		}
		if len(hidden) > 0 {
			lastOut, back := hidden[0], append([]bool(nil), shown...)
			for _, h := range hidden {
				if cols[h].Drop > cols[lastOut].Drop || (cols[h].Drop == cols[lastOut].Drop && h < lastOut) {
					lastOut = h
				}
			}
			back[lastOut] = true
			if _, ok := fill(cols, nat, back, width, 3, true); ok {
				t.Fatalf("caso %d: sacó %d sin necesidad", caso, lastOut)
			}
		}
		// Más ancho nunca esconde lo que se veía con menos.
		_, wider := layout(cols, nat, width+1+rng.Intn(20), 3)
		for i := range cols {
			if shown[i] && !wider[i] {
				t.Fatalf("caso %d: con más ancho se escondió la columna %d", caso, i)
			}
		}
	}
}

// minDe es el Min efectivo de una Flex.
func minDe(c Col) int {
	if c.Min <= 0 {
		return flexMin
	}
	return c.Min
}

func TestTruncateMiddleKeep(t *testing.T) {
	cmd := `node C:\dev\shop\web\node_modules\vite\bin\vite.js --port 5173`
	got := TruncateMiddleKeep(cmd, 5, 30)
	if Width(got) != 30 || !strings.HasPrefix(got, "node ") || !strings.HasSuffix(got, "5173") || !strings.Contains(got, "…") {
		t.Errorf("TruncateMiddleKeep = %q (%d columnas)", got, Width(got))
	}
	if got := TruncateMiddleKeep("corto", 3, 10); got != "corto" {
		t.Errorf("lo que entra no se toca: %q", got)
	}
	// El principio respetado no entra: se corta al final.
	if got := TruncateMiddleKeep("programa-larguísimo resto", 19, 10); got != "programa-…" {
		t.Errorf("sin lugar para el principio: %q", got)
	}
	if got := TruncateMiddleKeep("日本語 abcdef", 7, 10); Width(got) > 10 || !strings.HasPrefix(got, "日本語 ") {
		t.Errorf("con runas anchas: %q (%d)", got, Width(got))
	}
}

// La tabla entera a 100 y a 180 columnas: ninguna línea se pasa, y con lugar
// sobra, no se recorta nada.
func TestTableAnchos(t *testing.T) {
	cols := []Col{
		{Title: "PUERTO", Align: Right},
		{Title: "DIRECCIÓN", Drop: 1},
		{Title: "PROCESO", Flex: true, Min: 12},
		{Title: "COMANDO", Flex: true, Min: 16, Middle: true},
	}
	rows := [][]Cell{
		{C("3000", Lavender), C("[::1]", Subtle), C("node.exe", Text), {Text: `node C:\dev\shop\web\node_modules\vite\bin\vite.js --port 3000`, Keep: 5}},
		{C("5432", Lavender), C("0.0.0.0 [::]", Subtle), C("docker · shop-db-1", Text), C("postgres:16", Faint)},
	}
	for _, ancho := range []int{100, 60, 50, 30, 180} {
		term := &Term{}
		term.SetWidth(ancho)
		for _, l := range term.Table(cols, rows) {
			if Width(l) > ancho-len(Margin) {
				t.Errorf("a %d columnas la línea se pasa (%d): %q", ancho, Width(l), l)
			}
		}
	}
	term := &Term{}
	term.SetWidth(180)
	out := strings.Join(term.Table(cols, rows), "\n")
	if strings.Contains(out, "…") || !strings.Contains(out, "vite.js --port 3000") {
		t.Errorf("a 180 entra todo sin recortar:\n%s", out)
	}
	// A 60 (56 útiles) entra con las Flex en su Min: DIRECCIÓN se queda.
	// A 50 ya no: se va, y el programa sigue entero.
	term.SetWidth(60)
	if out = strings.Join(term.Table(cols, rows), "\n"); !strings.Contains(out, "DIRECCIÓN") {
		t.Errorf("a 60 DIRECCIÓN todavía entra:\n%s", out)
	}
	term.SetWidth(50)
	out = strings.Join(term.Table(cols, rows), "\n")
	if strings.Contains(out, "DIRECCIÓN") || !strings.Contains(out, "node ") {
		t.Errorf("a 50 se va DIRECCIÓN y el programa se ve entero:\n%s", out)
	}
}

func TestAgoShort(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	casos := map[time.Duration]string{
		0:                               "recién",
		42 * time.Second:                "hace 42 s",
		41*time.Minute + 59*time.Second: "hace 41 min",
		28*time.Hour + 41*time.Minute:   "hace 28 h",
		47*time.Hour + 59*time.Minute:   "hace 47 h",
		3*24*time.Hour + 5*time.Hour:    "hace 3 días",
	}
	for d, want := range casos {
		if got := AgoShort(now.Add(-d), now); got != want {
			t.Errorf("AgoShort(-%v) = %q, esperaba %q", d, got, want)
		}
	}
	if got := AgoShort(time.Time{}, now); got != "—" {
		t.Errorf("AgoShort(cero) = %q", got)
	}
}

// Alt: si el texto no entra, se usa la forma corta (con su Keep) antes de
// recortar; si entra, el texto entero.
func TestTableAlt(t *testing.T) {
	cols := []Col{{Title: "PUERTO"}, {Title: "COMANDO", Flex: true, Min: 12, Middle: true}}
	largo := `node ~\dev\_pruebas\killport\revision\x\srv.js normal 0.0.0.0 0 A ~\dev\_pruebas\killport\revision\x\m-L4.txt`
	rows := [][]Cell{{C("3000", Lavender), {Text: largo, Keep: 5, Alt: `node srv.js normal 0.0.0.0 0 A m-L4.txt`, AltKeep: len("node srv.js ")}}}
	term := &Term{}
	term.SetWidth(200)
	if out := strings.Join(term.Table(cols, rows), "\n"); !strings.Contains(out, largo) {
		t.Fatalf("a 180 entra entero:\n%s", out)
	}
	term.SetWidth(60)
	if out := strings.Join(term.Table(cols, rows), "\n"); !strings.Contains(out, "node srv.js normal 0.0.0.0 0 A m-L4.txt") {
		t.Fatalf("a 60 va la forma corta entera:\n%s", out)
	}
	term.SetWidth(40)
	if out := strings.Join(term.Table(cols, rows), "\n"); !strings.Contains(out, "node srv.js ") || !strings.Contains(out, "…") || !strings.Contains(out, "L4.txt") {
		t.Fatalf("a 40 la forma corta, recortada respetando el programa y el script:\n%s", out)
	}
}

// En una consola angosta el aire entre columnas baja a dos antes de sacar una
// columna: con tres no entra la opcional, con dos sí (X-12).
func TestTableHuecoDeDos(t *testing.T) {
	cols := []Col{{Title: "AAAA"}, {Title: "BBBB"}, {Title: "CCCC", Drop: 1}, {Title: "DDDD"}, {Title: "EEEE", Flex: true, Min: 10}}
	rows := [][]Cell{{C("aaaa", Text), C("bbbb", Text), C("cccc", Text), C("dddd", Text), C(strings.Repeat("e", 10), Text)}}
	// 4*4 + 10 = 26 de columnas; con huecos de 3 son 38, de 2 son 34.
	term := &Term{}
	term.SetWidth(34 + len(Margin)*2)
	lines := term.Table(cols, rows)
	if !strings.Contains(lines[0], "CCCC") || !strings.Contains(lines[1], "aaaa  bbbb  cccc") {
		t.Fatalf("con huecos de dos entra todo:\n%s", strings.Join(lines, "\n"))
	}
	// Con lugar de sobra, tres espacios.
	term.SetWidth(100)
	if lines = term.Table(cols, rows); !strings.Contains(lines[1], "aaaa   bbbb") {
		t.Fatalf("con lugar, huecos de tres:\n%s", strings.Join(lines, "\n"))
	}
}

// Sin consola y sin ancho fijado, la tabla no se recorta (X-38); COLUMNS
// fija el ancho.
func TestTableSinConsolaNoRecorta(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "salida-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	t.Setenv("COLUMNS", "")
	term := &Term{out: f}
	largo := strings.Repeat("x", 300)
	cols := []Col{{Title: "A"}, {Title: "B", Flex: true, Middle: true}}
	rows := [][]Cell{{C("1", Text), C(largo, Text)}}
	if out := strings.Join(term.Table(cols, rows), "\n"); !strings.Contains(out, largo) {
		t.Fatal("sin consola se recortó la columna")
	}
	t.Setenv("COLUMNS", "60")
	if term.Width() != 60 {
		t.Fatalf("COLUMNS=60 y el ancho es %d", term.Width())
	}
	if out := strings.Join(term.Table(cols, rows), "\n"); strings.Contains(out, largo) || !strings.Contains(out, "…") {
		t.Fatal("con COLUMNS se recorta a ese ancho")
	}
}

// Sin consola, solo la última columna va entera: las demás se disponen en
// 180 columnas a lo sumo (antes, una DIRECCIÓN de cien columnas estiraba todas las
// filas).
func TestTableSinConsolaSoloLaUltimaEntera(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "salida-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	t.Setenv("COLUMNS", "")
	term := &Term{out: f}
	cols := []Col{{Title: "A"}, {Title: "DIR", Drop: 1}, {Title: "CMD", Flex: true, Middle: true}}
	rows := [][]Cell{
		{C("1", Text), C(strings.Repeat("d", 200), Text), C(strings.Repeat("c", 300), Text)},
		{C("2", Text), C("x", Text), C("corto", Text)},
	}
	lines := term.Table(cols, rows)
	if !strings.Contains(lines[1], strings.Repeat("c", 300)) || strings.Contains(lines[1], strings.Repeat("d", 200)) || Width(lines[2]) > maxWidth {
		t.Fatalf("tabla sin consola:\n%s", strings.Join(lines, "\n"))
	}
}
