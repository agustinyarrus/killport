package ports

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// bitmap es el oráculo ingenuo: un booleano por puerto. Lento y obviamente
// correcto; el Set tiene que coincidir con él en los 65.536 puertos.
type bitmap [65536]bool

func (b *bitmap) add(r Range) {
	for p := int(r.Lo); p <= int(r.Hi); p++ {
		b[p] = true
	}
}

// runs arma los intervalos maximales del bitmap: la forma canónica esperada,
// calculada por un camino que no comparte código con NewSet.
func (b *bitmap) runs() []Range {
	var out []Range
	for p := 0; p < 65536; {
		if !b[p] {
			p++
			continue
		}
		q := p
		for q+1 < 65536 && b[q+1] {
			q++
		}
		out = append(out, Range{uint16(p), uint16(q)})
		p = q + 1
	}
	return out
}

// checkCanonical verifica la invariante del tipo, no solo el resultado.
func checkCanonical(t *testing.T, s Set) {
	t.Helper()
	for i, r := range s.rs {
		if r.Lo > r.Hi {
			t.Fatalf("intervalo %d dado vuelta: %v", i, r)
		}
		if i > 0 && int(s.rs[i-1].Hi)+1 >= int(r.Lo) {
			t.Fatalf("intervalos %d y %d solapados o adyacentes: %v %v", i-1, i, s.rs[i-1], r)
		}
	}
}

// randomRanges genera rangos con perfiles distintos: pocos y anchos, muchos y
// angostos, y pegados a los bordes 0 y 65535, donde vive el desborde de uint16.
func randomRanges(rng *rand.Rand) []Range {
	n := rng.IntN(40)
	rs := make([]Range, n)
	profile := rng.IntN(4)
	for i := range rs {
		var a, b int
		switch profile {
		case 0: // anchos
			a, b = rng.IntN(65536), rng.IntN(65536)
		case 1: // angostos y densos: muchas adyacencias
			a = rng.IntN(300)
			b = a + rng.IntN(4)
		case 2: // pegados a los bordes
			if rng.IntN(2) == 0 {
				a, b = rng.IntN(5), rng.IntN(5)
			} else {
				a, b = 65535-rng.IntN(5), 65535-rng.IntN(5)
			}
		default: // mezcla
			a = rng.IntN(65536)
			b = min(65535, a+rng.IntN(2000))
		}
		rs[i] = Range{uint16(a), uint16(b)} // a veces dado vuelta a propósito
	}
	return rs
}

func TestSetContraBitmap(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 1))
	for iter := 0; iter < 300; iter++ {
		var ba, bb bitmap
		ra, rb := randomRanges(rng), randomRanges(rng)
		for _, r := range ra {
			ba.add(R(r.Lo, r.Hi))
		}
		for _, r := range rb {
			bb.add(R(r.Lo, r.Hi))
		}
		a, b := NewSet(ra...), NewSet(rb...)
		checkCanonical(t, a)

		// La forma canónica es ÚNICA: coincide intervalo a intervalo con las
		// corridas del bitmap.
		if !slices.Equal(a.rs, ba.runs()) {
			t.Fatalf("iter %d: NewSet(%v) = %v, esperaba %v", iter, ra, a.rs, ba.runs())
		}
		count := 0
		for p := 0; p < 65536; p++ {
			if a.Contains(uint16(p)) != ba[p] {
				t.Fatalf("iter %d: Contains(%d) = %v, bitmap %v (set %v)", iter, p, !ba[p], ba[p], a)
			}
			if ba[p] {
				count++
			}
		}
		if a.Len() != count {
			t.Fatalf("iter %d: Len = %d, bitmap %d", iter, a.Len(), count)
		}
		var seq []uint16
		for p := range a.All() {
			seq = append(seq, p)
		}
		if len(seq) != count || !slices.IsSorted(seq) {
			t.Fatalf("iter %d: All dio %d puertos (esperaba %d) o desordenados", iter, len(seq), count)
		}

		// Álgebra de conjuntos contra las operaciones bit a bit.
		var bu, bi, bm bitmap
		for p := range 65536 {
			bu[p] = ba[p] || bb[p]
			bi[p] = ba[p] && bb[p]
			bm[p] = ba[p] && !bb[p]
		}
		for name, got := range map[string]struct {
			s    Set
			want *bitmap
		}{
			"Union": {a.Union(b), &bu}, "Intersect": {a.Intersect(b), &bi}, "Minus": {a.Minus(b), &bm},
		} {
			checkCanonical(t, got.s)
			if !slices.Equal(got.s.rs, got.want.runs()) {
				t.Fatalf("iter %d: %s = %v, esperaba %v", iter, name, got.s.rs, got.want.runs())
			}
		}
		if !a.Union(b).Equal(b.Union(a)) || !a.Intersect(b).Equal(b.Intersect(a)) {
			t.Fatalf("iter %d: unión o intersección no conmutan", iter)
		}

		// El texto vuelve a entrar por el parser (sin el 0, que no es un
		// puerto que se pueda pedir).
		if a.IsEmpty() || a.Contains(0) {
			continue
		}
		back, err := ParseSpec(a.String())
		if err != nil || !back.Equal(a) {
			t.Fatalf("iter %d: ParseSpec(%q) = %v, %v; esperaba %v", iter, a.String(), back, err, a)
		}
	}
}

func TestSetBordes(t *testing.T) {
	// 65535 y 0 no son adyacentes: con aritmética uint16, 65535+1 = 0 los fusionaría.
	s := NewSet(Range{65535, 65535}, Range{0, 0})
	if len(s.rs) != 2 || s.String() != "0, 65535" {
		t.Errorf("bordes = %v", s.rs)
	}
	full := NewSet(Range{0, 65535})
	if full.Len() != 65536 || !full.Contains(0) || !full.Contains(65535) {
		t.Errorf("conjunto lleno mal: %v, Len %d", full, full.Len())
	}
	if !full.Minus(full).IsEmpty() {
		t.Error("lleno menos lleno tendría que ser vacío")
	}
	var empty Set
	if empty.Contains(0) || empty.Len() != 0 || empty.String() != "" {
		t.Error("el valor cero tendría que ser el conjunto vacío")
	}
	if got := NewSet(R(5003, 5000), Range{3000, 3000}, Range{3001, 3002}).String(); got != "3000–3002, 5000–5003" {
		t.Errorf("String = %q", got)
	}
	if m, ok := Of(8080, 3000).Min(); !ok || m != 3000 {
		t.Errorf("Min = %d, %v", m, ok)
	}
}
