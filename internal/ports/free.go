package ports

import (
	"slices"
)

// Puertos libres de verdad.
//
// "Dame un puerto libre" parece trivial y en Windows no lo es. Un puerto que
// netstat no muestra ocupado puede estar:
//
//   - tomado en OTRA dirección: un servidor en 127.0.0.1:3000 no le impide a
//     un bind común a 0.0.0.0:3000 atarse (la tabla de Microsoft: específica
//     primero y comodín después = éxito); el segundo servidor arranca sin
//     error y el navegador sigue llegando al primero;
//   - reservado por WinNAT para Hyper-V, WSL o Docker, por http.sys mientras
//     lo atiende, o por un programa: bind da "acceso denegado" y, salvo el de
//     http.sys, nadie figura como dueño;
//   - en el rango dinámico: Windows lo reparte como puerto de salida, y en
//     cualquier momento lo toma una conexión saliente;
//   - apartado a mano (una exclusión administrada, la de `killport --reserve`):
//     un programa que lo pide lo puede usar, pero alguien lo reservó para algo.
//
// FindFree da los primeros N puertos que no caen en nada de eso y que además
// se dejan atar en EXCLUSIVA en las dos familias (0.0.0.0, y [::] solo IPv6)
// en cada protocolo pedido: la confirmación de que ahora mismo nadie los
// tiene, en ninguna dirección. Entre "lo encontré" y "lo usó el servidor" hay
// una carrera que ningún buscador puede cerrar (otro proceso lo puede tomar
// en el medio): se documenta, no se esconde.
//
// El recorrido es por intervalos: los candidatos son Within menos la unión de
// lo que bloquea (Set.Minus), así que un bloque de 100 puertos de WinNAT o el
// rango dinámico entero se saltean de un paso. Solo se sondean candidatos, en
// orden, hasta juntar N. Con k filas que bloquean: O(k log k) para armar los
// conjuntos, más un sondeo de microsegundos por candidato mirado.

// FreeQuery es lo que se busca.
type FreeQuery struct {
	Within Range // dónde buscar; se recorre en orden creciente
	// Explicit: el usuario escribió el rango de punta a punta (3000-3999).
	// Entonces vale también lo que caiga en el rango dinámico, porque lo pidió.
	// Con solo un inicio (--free 3000) el final es implícito y el dinámico se
	// saltea: es de Windows, que lo reparte a las conexiones salientes. Salvo
	// que el inicio sea 49152 o más (--free 55000): es pedir, a sabiendas, un
	// puerto de la zona dinámica y privada de la IANA, y saltearla entera
	// sería contestar "no hay" a una pregunta que tiene respuesta. Un inicio
	// más abajo que cae en un rango dinámico CORRIDO (1025–5000, el de Windows
	// XP) no cuenta: ahí el usuario pidió su puerto de siempre y es Windows el
	// que está mal configurado.
	Explicit bool
	Protos   []Proto // tiene que estar libre en TODOS; vacío = TCP (lo que usa un servidor de desarrollo)
	Count    int     // cuántos; menos de 1 cuenta como 1
}

// FreeWorld es lo que se leyó del sistema para buscar.
type FreeWorld struct {
	Sockets  []Socket // las tablas completas, en todos los estados
	Excluded Exclusions
	// Dynamic: el rango dinámico de cada protocolo y familia. El que falte
	// (netsh no contestó) se toma como el de fábrica, 49152–65535: saltear de
	// más no le hace mal a nadie; ofrecer un puerto que Windows reparte, sí.
	Dynamic DynamicRanges
}

// Prober sondea un bind exclusivo de un puerto en la dirección comodín de una
// familia. Lo implementa quien llama: internal/win, o un doble.
type Prober func(proto Proto, fam Family, port uint16) BindResult

// SkipWhy es por qué se salteó un puerto. Los motivos van en orden de
// prioridad: si un puerto cae en varios, se cuenta el primero.
type SkipWhy uint8

const (
	SkipTaken        SkipWhy = iota + 1 // un socket de la tabla lo tiene
	SkipReserved                        // cae en una exclusión sin "*": de WinNAT, de http.sys o de un programa (Excluded.Origin)
	SkipAdministered                    // cae en una exclusión administrada ("*": apartado a mano)
	SkipDynamic                         // cae en el rango dinámico (y el rango no se pidió explícito)
	SkipProbe                           // no figura en nada, pero el bind exclusivo no lo ató
)

// Skip es un tramo salteado, con su motivo y lo que hace falta para contarlo.
type Skip struct {
	Range
	Why SkipWhy
	// SkipTaken: un socket que lo tiene (si hay uno que escucha, ese; si no,
	// una conexión que lo usa de puerto local). Los puertos seguidos del mismo
	// proceso van en un solo tramo.
	Socket   Socket
	Excluded ExcludedRange // SkipReserved y SkipAdministered: la fila entera
	Dynamic  DynamicRange  // SkipDynamic: el rango entero
	// SkipProbe: dónde y cómo falló el sondeo.
	Proto  Proto
	Family Family
	Bind   BindResult
}

// FreeResult es lo que encontró FindFree.
type FreeResult struct {
	Found []uint16 // en orden creciente; menos que Count si no hubo más
	// Skips explica lo salteado entre Within.Lo y el último puerto mirado (el
	// último encontrado, o el final si faltaron): tramos ordenados y disjuntos
	// que, junto con Found, cubren exactamente ese intervalo.
	Skips  []Skip
	Probes int  // cuántos sondeos de bind se hicieron
	Capped bool // se dejó de buscar por el tope de sondeos fallidos
}

// freeProbeBudget acota los candidatos que fallan el sondeo. En una máquina
// sana casi no hay (lo que no se deja atar figura en alguna tabla); si el
// sondeo falla siempre —Winsock roto, un entorno que niega todo bind— la
// búsqueda termina en vez de sondear miles de puertos para nada.
const freeProbeBudget = 512

// families son las dos familias, en el orden en que se sondean.
var families = [...]Family{IPv4, IPv6}

// FindFree busca los primeros q.Count puertos usables de q.Within. Pura: el
// mundo viene leído y el sondeo es una función.
func FindFree(q FreeQuery, w FreeWorld, probe Prober) FreeResult {
	protos := q.Protos
	if len(protos) == 0 {
		protos = []Proto{TCP}
	}
	want := max(q.Count, 1)
	span := NewSet(q.Within).Minus(Of(0)) // el 0 no es un puerto
	bl := blockersOf(protos, q.Explicit, q.Within.Lo, w)

	var res FreeResult
	var fails []Skip
	end, looked := uint16(0), false
	failed := 0
	for p := range span.Minus(bl.all).All() {
		end, looked = p, true
		if f, ok := probeAll(protos, p, probe, &res.Probes); !ok {
			fails = append(fails, f)
			if failed++; failed >= freeProbeBudget {
				res.Capped = true
				break
			}
			continue
		}
		if res.Found = append(res.Found, p); len(res.Found) == want {
			break
		}
	}
	// Lo que se explica: de Within.Lo al último puerto mirado, o a Within.Hi
	// si se recorrió todo sin juntar los pedidos.
	if len(res.Found) < want && !res.Capped {
		end, looked = q.Within.Hi, true
	}
	if lo, ok := span.Min(); ok && looked && end >= lo {
		res.Skips = bl.explain(span.Intersect(NewSet(Range{lo, end})), fails)
	}
	return res
}

// probeAll sondea p en cada protocolo y familia, en orden, hasta el primer
// fallo. ok=false trae el tramo que lo explica.
func probeAll(protos []Proto, p uint16, probe Prober, n *int) (Skip, bool) {
	for _, proto := range protos {
		for _, fam := range families {
			*n++
			if r := probe(proto, fam, p); r != BindFree {
				return Skip{Range: Range{p, p}, Why: SkipProbe, Proto: proto, Family: fam, Bind: r}, false
			}
		}
	}
	return Skip{}, true
}

// blocker es algo que bloquea un tramo, ya con el motivo con que se contaría.
type blocker struct {
	set  Set
	skip Skip // la plantilla: Why y lo que hace falta para contarlo (sin Range)
}

// blockers son todos los bloqueos, agrupados por motivo en orden de prioridad.
type blockers struct {
	byWhy [SkipDynamic + 1][]blocker
	all   Set
}

func (b *blockers) add(why SkipWhy, s Set, skip Skip) {
	if s.IsEmpty() {
		return
	}
	skip.Why = why
	b.byWhy[why] = append(b.byWhy[why], blocker{set: s, skip: skip})
	b.all = b.all.Union(s)
}

// blockersOf arma lo que bloquea para esos protocolos. Un socket bloquea en
// cualquier estado —una conexión que usa el puerto de local tampoco deja
// atarlo en exclusiva— salvo los restos del PID 0 (TIME_WAIT y compañía): un
// servidor común ya los puede pisar, y ahí decide el sondeo. El rango
// dinámico bloquea salvo que la búsqueda sea explícita o arranque en la zona
// dinámica de la IANA (start ≥ 49152; ver FreeQuery.Explicit).
func blockersOf(protos []Proto, explicit bool, start uint16, w FreeWorld) blockers {
	var b blockers

	// Tomados: por puerto, el socket más útil para nombrar al dueño; después,
	// tramos de puertos seguidos del mismo proceso.
	best := map[uint16]Socket{}
	for _, s := range w.Sockets {
		if !slices.Contains(protos, s.Proto) || s.Port() == 0 {
			continue
		}
		if s.Proto == TCP && s.PID == 0 && s.State != Listen {
			continue // un resto sin dueño: no bloquea un bind común
		}
		if cur, ok := best[s.Port()]; !ok || betterOwner(s, cur) {
			best[s.Port()] = s
		}
	}
	taken := make([]uint16, 0, len(best))
	for p := range best {
		taken = append(taken, p)
	}
	slices.Sort(taken)
	for i := 0; i < len(taken); {
		j, s := i, best[taken[i]]
		for j+1 < len(taken) && int(taken[j+1]) == int(taken[j])+1 && best[taken[j+1]].PID == s.PID {
			j++
		}
		b.add(SkipTaken, NewSet(Range{taken[i], taken[j]}), Skip{Socket: s})
		i = j + 1
	}

	// Excluidos: lo que reserva Windows (WinNAT, http.sys, un programa) antes
	// que las exclusiones a mano.
	for _, admin := range [...]bool{false, true} {
		why := SkipReserved
		if admin {
			why = SkipAdministered
		}
		for _, x := range w.Excluded {
			if x.Administered == admin && slices.Contains(protos, x.Proto) {
				b.add(why, NewSet(x.Range), Skip{Excluded: x})
			}
		}
	}

	if !explicit && start < DynamicDefaultStart {
		for _, proto := range protos {
			for _, fam := range families {
				d, ok := w.Dynamic.Find(proto, fam)
				if !ok {
					d = DynamicRange{Proto: proto, Family: fam, Start: DynamicDefaultStart, Count: DynamicDefaultCount}
				}
				b.add(SkipDynamic, NewSet(d.Range()), Skip{Dynamic: d})
			}
		}
	}
	return b
}

// betterOwner dice si s nombra mejor que cur al dueño de un puerto: primero lo
// que escucha (un servidor) antes que una conexión, después un PID de verdad
// antes que el 0. Ante empate queda el primero de la tabla: determinista.
func betterOwner(s, cur Socket) bool {
	listens := func(x Socket) bool { return x.Proto == UDP || x.State == Listen }
	if listens(s) != listens(cur) {
		return listens(s)
	}
	return cur.PID == 0 && s.PID != 0
}

// explain reparte span (lo mirado, sin lo encontrado) en tramos con motivo:
// cada motivo, en orden de prioridad, se queda con lo que ningún motivo
// anterior explicó. Los fallos del sondeo cubren lo que queda (son
// candidatos, fuera de todo bloqueo). O(b·k) con b los bloqueos y k los
// tramos ya explicados.
func (b *blockers) explain(span Set, fails []Skip) []Skip {
	var out []Skip
	var covered Set
	for why := SkipTaken; why <= SkipDynamic; why++ {
		for _, bk := range b.byWhy[why] {
			piece := bk.set.Intersect(span).Minus(covered)
			for _, r := range piece.Ranges() {
				s := bk.skip
				s.Range = r
				out = append(out, s)
			}
			covered = covered.Union(piece)
		}
	}
	// Los fallos del sondeo, fundidos cuando son seguidos y por lo mismo.
	for _, f := range fails {
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.Why == SkipProbe && int(last.Hi)+1 == int(f.Lo) && last.Proto == f.Proto && last.Family == f.Family && last.Bind == f.Bind {
				last.Hi = f.Hi
				continue
			}
		}
		out = append(out, f)
	}
	slices.SortFunc(out, func(x, y Skip) int { return int(x.Lo) - int(y.Lo) })
	return out
}
