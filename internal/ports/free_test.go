package ports

import (
	"fmt"
	"math/rand/v2"
	"net/netip"
	"slices"
	"testing"
)

// sondeoLibre es un sondeo que ata todo.
func sondeoLibre(Proto, Family, uint16) BindResult { return BindFree }

// sondeoTabla es un sondeo guionado: lo que no figura, se ata.
type sondeoTabla map[bindCase]BindResult

type bindCase struct {
	proto Proto
	fam   Family
	port  uint16
}

func (s sondeoTabla) probe(proto Proto, fam Family, p uint16) BindResult {
	if r, ok := s[bindCase{proto, fam, p}]; ok {
		return r
	}
	return BindFree
}

// dinamicoDeFabrica son los cuatro rangos dinámicos de fábrica.
func dinamicoDeFabrica() DynamicRanges {
	var out DynamicRanges
	for _, proto := range []Proto{TCP, UDP} {
		for _, fam := range families {
			out = append(out, DynamicRange{Proto: proto, Family: fam, Start: DynamicDefaultStart, Count: DynamicDefaultCount})
		}
	}
	return out
}

func TestFindFreeCasos(t *testing.T) {
	tcp4 := func(local string, st State, pid uint32) Socket { return sock(TCP, local, st, pid) }
	winnat := ExcludedRange{Proto: TCP, Family: IPv4, Range: Range{3050, 3149}}
	admin := ExcludedRange{Proto: TCP, Family: IPv4, Range: Range{3001, 3001}, Administered: true}
	bajo := DynamicRange{Proto: TCP, Family: IPv4, Start: 1025, Count: 3976} // el de Windows XP: 1025–5000

	casos := []struct {
		nombre string
		q      FreeQuery
		w      FreeWorld
		probe  sondeoTabla
		found  []uint16
		skips  []string // "lo-hi motivo"
		capped bool
	}{
		{
			nombre: "el primero libre",
			q:      FreeQuery{Within: Range{3000, 3010}},
			w:      FreeWorld{Sockets: []Socket{tcp4("0.0.0.0:3000", Listen, 812)}, Dynamic: dinamicoDeFabrica()},
			found:  []uint16{3001},
			skips:  []string{"3000 tomado:812"},
		},
		{
			// La trampa que motiva todo: un servidor en 127.0.0.1 no le impide
			// a un bind común a 0.0.0.0 atarse. Acá cuenta igual.
			nombre: "tomado en loopback cuenta",
			q:      FreeQuery{Within: Range{3000, 3010}},
			w:      FreeWorld{Sockets: []Socket{tcp4("127.0.0.1:3000", Listen, 5), sock(TCP, "[::1]:3001", Listen, 6)}},
			found:  []uint16{3002},
			skips:  []string{"3000 tomado:5", "3001 tomado:6"},
		},
		{
			nombre: "una conexión que lo usa de puerto local lo toma",
			q:      FreeQuery{Within: Range{3000, 3010}},
			w: FreeWorld{Sockets: []Socket{{Proto: TCP, Local: mustAP("192.168.0.2:3000"), Remote: mustAP("1.2.3.4:443"),
				State: Established, PID: 77}}},
			found: []uint16{3001},
			skips: []string{"3000 tomado:77"},
		},
		{
			// Un resto del PID 0 no bloquea de antemano: decide el sondeo
			// exclusivo, que en unos segundos lo va a dejar atar.
			nombre: "TIME_WAIT del PID 0: decide el sondeo",
			q:      FreeQuery{Within: Range{3000, 3010}},
			w:      FreeWorld{Sockets: []Socket{tcp4("127.0.0.1:3000", TimeWait, 0)}},
			probe:  sondeoTabla{{TCP, IPv4, 3000}: BindInUse},
			found:  []uint16{3001},
			skips:  []string{"3000 sondeo:TCP/IPv4/en uso"},
		},
		{
			nombre: "el mismo resto con el sondeo a favor",
			q:      FreeQuery{Within: Range{3000, 3010}},
			w:      FreeWorld{Sockets: []Socket{tcp4("127.0.0.1:3000", TimeWait, 0)}},
			found:  []uint16{3000},
		},
		{
			nombre: "un socket UDP no molesta a TCP",
			q:      FreeQuery{Within: Range{3000, 3010}},
			w:      FreeWorld{Sockets: []Socket{sock(UDP, "0.0.0.0:3000", 0, 9)}},
			found:  []uint16{3000},
		},
		{
			nombre: "con los dos protocolos, cuenta el UDP",
			q:      FreeQuery{Within: Range{3000, 3010}, Protos: []Proto{TCP, UDP}},
			w:      FreeWorld{Sockets: []Socket{sock(UDP, "0.0.0.0:3000", 0, 9)}},
			found:  []uint16{3001},
			skips:  []string{"3000 tomado:9"},
		},
		{
			nombre: "bloque de WinNAT y exclusión administrada",
			q:      FreeQuery{Within: Range{3000, 3200}, Count: 2},
			w: FreeWorld{Sockets: []Socket{tcp4("0.0.0.0:3000", Listen, 812)},
				Excluded: Exclusions{admin, winnat}},
			found: []uint16{3002, 3003},
			skips: []string{"3000 tomado:812", "3001 administrada:3001-3001"},
		},
		{
			// Una reserva a mano adentro de un bloque de WinNAT: el bloque manda.
			nombre: "prioridad: WinNAT antes que una exclusión administrada",
			q:      FreeQuery{Within: Range{3140, 3200}},
			w:      FreeWorld{Excluded: Exclusions{{Proto: TCP, Family: IPv6, Range: Range{3145, 3160}, Administered: true}, winnat}},
			found:  []uint16{3161},
			skips:  []string{"3140-3149 winnat:3050-3149", "3150-3160 administrada:3145-3160"},
		},
		{
			nombre: "un bloque entero se saltea de un paso",
			q:      FreeQuery{Within: Range{3049, 3200}},
			w:      FreeWorld{Sockets: []Socket{tcp4("0.0.0.0:3049", Listen, 1)}, Excluded: Exclusions{winnat}},
			found:  []uint16{3150},
			skips:  []string{"3049 tomado:1", "3050-3149 winnat:3050-3149"},
		},
		{
			// Lo que cae en dos cosas se cuenta por la primera: el socket.
			nombre: "prioridad: tomado antes que WinNAT",
			q:      FreeQuery{Within: Range{3050, 3200}},
			w:      FreeWorld{Sockets: []Socket{tcp4("0.0.0.0:3060", Listen, 4)}, Excluded: Exclusions{winnat}},
			found:  []uint16{3150},
			skips:  []string{"3050-3059 winnat:3050-3149", "3060 tomado:4", "3061-3149 winnat:3050-3149"},
		},
		{
			nombre: "el rango dinámico se saltea si el final es implícito",
			q:      FreeQuery{Within: Range{49000, 65535}},
			w:      FreeWorld{Sockets: []Socket{tcp4("0.0.0.0:49000", Listen, 3)}, Excluded: Exclusions{{Proto: TCP, Family: IPv4, Range: Range{49001, 49151}}}, Dynamic: dinamicoDeFabrica()},
			skips:  []string{"49000 tomado:3", "49001-49151 winnat:49001-49151", "49152-65535 dinamico:49152-65535"},
		},
		{
			// Arrancar en la zona dinámica de la IANA es pedirla: vale.
			nombre: "el inicio cae en el rango dinámico de fábrica",
			q:      FreeQuery{Within: Range{55140, 65535}, Count: 2},
			w:      FreeWorld{Sockets: []Socket{tcp4("0.0.0.0:55140", Listen, 3)}, Dynamic: dinamicoDeFabrica()},
			found:  []uint16{55141, 55142},
			skips:  []string{"55140 tomado:3"},
		},
		{
			// Arranca abajo: el dinámico se saltea aunque el rango lo cruce.
			nombre: "el rango dinámico se saltea aunque la búsqueda lo cruce",
			q:      FreeQuery{Within: Range{49151, 65535}, Count: 2},
			w:      FreeWorld{Sockets: []Socket{tcp4("0.0.0.0:49151", Listen, 3)}, Dynamic: dinamicoDeFabrica()},
			skips:  []string{"49151 tomado:3", "49152-65535 dinamico:49152-65535"},
		},
		{
			nombre: "sin rango dinámico leído, el de fábrica",
			q:      FreeQuery{Within: Range{49150, 49160}, Count: 3},
			w:      FreeWorld{},
			found:  []uint16{49150, 49151},
			skips:  []string{"49152-49160 dinamico:49152-65535"},
		},
		{
			nombre: "pedido explícito: el dinámico vale",
			q:      FreeQuery{Within: Range{50000, 50010}, Explicit: true},
			w:      FreeWorld{Dynamic: dinamicoDeFabrica()},
			found:  []uint16{50000},
		},
		{
			nombre: "un rango dinámico corrido tapa los puertos de desarrollo",
			q:      FreeQuery{Within: Range{3000, 6000}},
			w:      FreeWorld{Dynamic: DynamicRanges{bajo}},
			found:  []uint16{5001},
			skips:  []string{"3000-5000 dinamico:1025-5000"},
		},
		{
			nombre: "el v6 no se deja atar",
			q:      FreeQuery{Within: Range{3000, 3010}},
			probe:  sondeoTabla{{TCP, IPv6, 3000}: BindInUse, {TCP, IPv6, 3001}: BindInUse, {TCP, IPv4, 3002}: BindForbidden},
			found:  []uint16{3003},
			skips:  []string{"3000-3001 sondeo:TCP/IPv6/en uso", "3002 sondeo:TCP/IPv4/prohibido"},
		},
		{
			nombre: "no alcanzan: explica hasta el final",
			q:      FreeQuery{Within: Range{3000, 3003}, Count: 3},
			w:      FreeWorld{Sockets: []Socket{tcp4("0.0.0.0:3001", Listen, 2), tcp4("0.0.0.0:3002", Listen, 2)}},
			found:  []uint16{3000, 3003},
			skips:  []string{"3001-3002 tomado:2"},
		},
		{
			nombre: "puertos seguidos de procesos distintos van por separado",
			q:      FreeQuery{Within: Range{3000, 3010}},
			w:      FreeWorld{Sockets: []Socket{tcp4("0.0.0.0:3000", Listen, 2), tcp4("0.0.0.0:3001", Listen, 3)}},
			found:  []uint16{3002},
			skips:  []string{"3000 tomado:2", "3001 tomado:3"},
		},
		{
			// El que escucha nombra mejor al dueño que una conexión saliente.
			nombre: "el dueño que se nombra es el que escucha",
			q:      FreeQuery{Within: Range{3000, 3010}},
			w: FreeWorld{Sockets: []Socket{{Proto: TCP, Local: mustAP("10.0.0.2:3000"), Remote: mustAP("1.2.3.4:443"), State: Established, PID: 50},
				tcp4("127.0.0.1:3000", Listen, 60)}},
			found: []uint16{3001},
			skips: []string{"3000 tomado:60"},
		},
		{
			nombre: "el puerto 0 no existe",
			q:      FreeQuery{Within: Range{0, 2}},
			found:  []uint16{1},
		},
		{
			nombre: "nada libre",
			q:      FreeQuery{Within: Range{3050, 3060}},
			w:      FreeWorld{Excluded: Exclusions{winnat}},
			skips:  []string{"3050-3060 winnat:3050-3149"},
		},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			probe := sondeoLibre
			if c.probe != nil {
				probe = c.probe.probe
			}
			r := FindFree(c.q, c.w, probe)
			if !slices.Equal(r.Found, c.found) {
				t.Errorf("encontró %v, esperaba %v", r.Found, c.found)
			}
			var got []string
			for _, s := range r.Skips {
				got = append(got, textoSkip(s))
			}
			if !slices.Equal(got, c.skips) {
				t.Errorf("salteados:\n  %q\nesperaba:\n  %q", got, c.skips)
			}
			if r.Capped != c.capped {
				t.Errorf("tope = %v", r.Capped)
			}
		})
	}
}

func mustAP(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

// textoSkip escribe un tramo para comparar en las tablas.
func textoSkip(s Skip) string {
	rango := s.Range.String()
	rango = rangoASCII(rango)
	switch s.Why {
	case SkipTaken:
		return fmt.Sprintf("%s tomado:%d", rango, s.Socket.PID)
	case SkipWinNAT:
		return fmt.Sprintf("%s winnat:%d-%d", rango, s.Excluded.Lo, s.Excluded.Hi)
	case SkipAdministered:
		return fmt.Sprintf("%s administrada:%d-%d", rango, s.Excluded.Lo, s.Excluded.Hi)
	case SkipDynamic:
		r := s.Dynamic.Range()
		return fmt.Sprintf("%s dinamico:%d-%d", rango, r.Lo, r.Hi)
	case SkipProbe:
		return fmt.Sprintf("%s sondeo:%s/%s/%s", rango, s.Proto, s.Family, s.Bind)
	}
	return rango + " ?"
}

func rangoASCII(s string) string {
	out := []rune{}
	for _, r := range s {
		if r == '–' {
			r = '-'
		}
		out = append(out, r)
	}
	return string(out)
}

// El tope: si el sondeo falla siempre, la búsqueda termina.
func TestFindFreeTopeDeSondeos(t *testing.T) {
	falla := func(Proto, Family, uint16) BindResult { return BindError }
	r := FindFree(FreeQuery{Within: Range{1024, 49151}}, FreeWorld{}, falla)
	if !r.Capped || len(r.Found) != 0 || r.Probes != freeProbeBudget {
		t.Fatalf("tope=%v encontrados=%v sondeos=%d", r.Capped, r.Found, r.Probes)
	}
	if len(r.Skips) != 1 || r.Skips[0].Range != (Range{1024, 1024 + freeProbeBudget - 1}) || r.Skips[0].Why != SkipProbe {
		t.Fatalf("salteados: %+v", r.Skips)
	}
}

// PROPIEDAD contra un oráculo ingenuo que mira puerto por puerto (sin
// conjuntos ni intervalos): mismos encontrados, mismos sondeos, y cada puerto
// mirado está explicado exactamente una vez, por el motivo de más prioridad.
func TestFindFreeContraBitmap(t *testing.T) {
	for seed := uint64(1); seed <= 400; seed++ {
		rng := rand.New(rand.NewPCG(seed, 0xF4EE))
		q, w, pr := mundoAlAzar(rng)
		probe := pr.probe
		got := FindFree(q, w, probe)
		want := findFreeIngenuo(q, w, probe)
		if !slices.Equal(got.Found, want.found) || got.Probes != want.probes || got.Capped != want.capped {
			t.Fatalf("semilla %d: encontró %v (%d sondeos, tope %v); el oráculo %v (%d, %v)\nq=%+v",
				seed, got.Found, got.Probes, got.Capped, want.found, want.probes, want.capped, q)
		}
		// Cobertura exacta de [Within.Lo (sin el 0), último mirado].
		cubierto := map[uint16]SkipWhy{}
		prev := -1
		for _, s := range got.Skips {
			if int(s.Lo) <= prev {
				t.Fatalf("semilla %d: tramos desordenados o solapados: %+v", seed, got.Skips)
			}
			prev = int(s.Hi)
			for p := int(s.Lo); p <= int(s.Hi); p++ {
				cubierto[uint16(p)] = s.Why
				if s.Why == SkipTaken && !tieneElPuerto(w.Sockets, s.Socket, uint16(p)) {
					t.Fatalf("semilla %d: el tramo %v nombra a un socket que no tiene el %d", seed, s.Range, p)
				}
			}
		}
		for p, why := range want.why {
			if cubierto[p] != why {
				t.Fatalf("semilla %d: el %d se explica con %v y el oráculo dice %v", seed, p, cubierto[p], why)
			}
			delete(cubierto, p)
		}
		if len(cubierto) > 0 {
			t.Fatalf("semilla %d: tramos de más: %v", seed, cubierto)
		}
	}
}

func tieneElPuerto(socks []Socket, s Socket, p uint16) bool {
	for _, x := range socks {
		if x.PID == s.PID && x.Port() == p {
			return true
		}
	}
	return false
}

type resultadoIngenuo struct {
	found  []uint16
	probes int
	capped bool
	why    map[uint16]SkipWhy // motivo de cada puerto mirado y no encontrado
}

// findFreeIngenuo es FindFree escrito de la manera más directa posible.
func findFreeIngenuo(q FreeQuery, w FreeWorld, probe Prober) resultadoIngenuo {
	protos := q.Protos
	if len(protos) == 0 {
		protos = []Proto{TCP}
	}
	want := max(q.Count, 1)
	res := resultadoIngenuo{why: map[uint16]SkipWhy{}}
	motivo := func(p uint16) SkipWhy {
		for _, s := range w.Sockets {
			if slices.Contains(protos, s.Proto) && s.Port() == p && !(s.Proto == TCP && s.PID == 0 && s.State != Listen) {
				return SkipTaken
			}
		}
		for _, admin := range []bool{false, true} {
			for _, x := range w.Excluded {
				if x.Administered == admin && slices.Contains(protos, x.Proto) && x.Contains(p) {
					if admin {
						return SkipAdministered
					}
					return SkipWinNAT
				}
			}
		}
		if !q.Explicit {
			for _, proto := range protos {
				for _, fam := range []Family{IPv4, IPv6} {
					d, ok := w.Dynamic.Find(proto, fam)
					if !ok {
						d = DynamicRange{Start: 49152, Count: 16384}
					}
					if d.Range().Contains(p) && q.Within.Lo < 49152 {
						return SkipDynamic
					}
				}
			}
		}
		return 0
	}
	fallos := 0
	for p := int(q.Within.Lo); p <= int(q.Within.Hi); p++ {
		if p == 0 {
			continue
		}
		if m := motivo(uint16(p)); m != 0 {
			res.why[uint16(p)] = m
			continue
		}
		ok := true
	sondeo:
		for _, proto := range protos {
			for _, fam := range []Family{IPv4, IPv6} {
				res.probes++
				if probe(proto, fam, uint16(p)) != BindFree {
					ok = false
					break sondeo
				}
			}
		}
		if !ok {
			res.why[uint16(p)] = SkipProbe
			if fallos++; fallos == freeProbeBudget {
				res.capped = true
				break
			}
			continue
		}
		res.found = append(res.found, uint16(p))
		if len(res.found) == want {
			break
		}
	}
	return res
}

// mundoAlAzar arma un mundo con sockets, exclusiones, rangos dinámicos y
// sondeos guionados, concentrado en una ventana chica para que choquen.
func mundoAlAzar(rng *rand.Rand) (FreeQuery, FreeWorld, sondeoTabla) {
	base := 1 + rng.IntN(65000)
	if rng.IntN(8) == 0 {
		base = 65400 + rng.IntN(100) // contra el borde de arriba
	}
	ancho := 1 + rng.IntN(300)
	lo := base
	hi := min(65535, base+ancho)
	if rng.IntN(10) == 0 {
		lo = 0 // con el 0 adentro
	}
	en := func() uint16 { return uint16(min(65535, lo+rng.IntN(hi-lo+2))) }
	protoAlAzar := func() Proto { return Proto(1 + rng.IntN(2)) }
	famAlAzar := func() Family { return Family(1 + rng.IntN(2)) }

	var w FreeWorld
	for range rng.IntN(60) {
		s := Socket{Proto: protoAlAzar(), PID: uint32(rng.IntN(5))}
		addr := "0.0.0.0"
		if famAlAzar() == IPv6 {
			addr = "[::1]"
		}
		s.Local = netip.MustParseAddrPort(fmt.Sprintf("%s:%d", addr, en()))
		if s.Proto == TCP {
			s.State = []State{Listen, Established, TimeWait, CloseWait}[rng.IntN(4)]
		}
		w.Sockets = append(w.Sockets, s)
	}
	for range rng.IntN(8) {
		a, b := en(), en()
		x := ExcludedRange{Proto: protoAlAzar(), Family: famAlAzar(), Range: R(a, b), Administered: rng.IntN(3) == 0}
		w.Excluded = append(w.Excluded, x)
		if rng.IntN(3) == 0 { // una reserva a mano adentro de un bloque de WinNAT (o al revés)
			x.Administered = !x.Administered
			x.Range = R(en(), a)
			w.Excluded = append(w.Excluded, x)
		}
	}
	for _, proto := range []Proto{TCP, UDP} {
		for _, fam := range []Family{IPv4, IPv6} {
			if rng.IntN(4) == 0 {
				continue // sin leer: vale el de fábrica
			}
			start := en()
			if start == 0 {
				start = 1
			}
			w.Dynamic = append(w.Dynamic, DynamicRange{Proto: proto, Family: fam, Start: start, Count: 1 + rng.IntN(max(1, 65536-int(start)))})
		}
	}
	pr := sondeoTabla{}
	for range rng.IntN(40) {
		pr[bindCase{protoAlAzar(), famAlAzar(), en()}] = BindResult(1 + rng.IntN(3))
	}
	q := FreeQuery{Within: Range{uint16(lo), uint16(hi)}, Explicit: rng.IntN(2) == 0, Count: 1 + rng.IntN(8)}
	switch rng.IntN(3) {
	case 0:
		q.Protos = []Proto{UDP}
	case 1:
		q.Protos = []Proto{TCP, UDP}
	}
	return q, w, pr
}
