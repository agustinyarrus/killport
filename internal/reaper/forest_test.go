package reaper

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

var epoch = time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)

func at(ms int) time.Time { return epoch.Add(time.Duration(ms) * time.Millisecond) }

// naïve es un bosque "obvio": todo por recorrido lineal, sin índices. Es el
// oráculo contra el que se contrasta Forest en bosques aleatorios.
type naïve struct{ procs []Proc }

func (n naïve) byPID(pid uint32) (Proc, bool) {
	// Con PID únicos dentro de la instantánea (el caso real) hay a lo sumo uno.
	for _, p := range n.procs {
		if p.PID == pid {
			return p, true
		}
	}
	return Proc{}, false
}

func (n naïve) parent(p Proc) (Proc, bool) {
	if p.Parent == 0 || p.Parent == p.PID {
		return Proc{}, false
	}
	par, ok := n.byPID(p.Parent)
	if !ok || par.Created.After(p.Created) {
		return Proc{}, false
	}
	return par, true
}

func (n naïve) ancestors(p Proc) []ID {
	var out []ID
	seen := map[ID]bool{p.ID: true}
	for {
		par, ok := n.parent(p)
		if !ok || seen[par.ID] {
			break
		}
		out = append(out, par.ID)
		seen[par.ID] = true
		p = par
	}
	return out
}

func (n naïve) children(id ID) []ID {
	var out []ID
	for _, c := range n.procs {
		if par, ok := n.parent(c); ok && par.ID == id {
			out = append(out, c.ID)
		}
	}
	return out
}

func (n naïve) subtree(id ID) []ID {
	order := []ID{id}
	for head := 0; head < len(order); head++ {
		order = append(order, n.children(order[head])...)
	}
	return order
}

func (n naïve) lca(a, b ID) (ID, bool) {
	pa, oka := n.byID(a)
	pb, okb := n.byID(b)
	if !oka || !okb {
		return ID{}, false
	}
	chain := map[ID]bool{a: true}
	for _, x := range n.ancestors(pa) {
		chain[x] = true
	}
	for cur, ok := pb, true; ok; {
		if chain[cur.ID] {
			return cur.ID, true
		}
		cur, ok = n.parent(cur)
	}
	return ID{}, false
}

func (n naïve) byID(id ID) (Proc, bool) {
	for _, p := range n.procs {
		if p.ID == id {
			return p, true
		}
	}
	return Proc{}, false
}

// randomForest arma un bosque con PID ÚNICOS (una instantánea real no repite
// PID), horas de creación al azar y padres que apuntan a cualquier PID: a veces
// a uno más nuevo, para disparar la regla del "padre impostor" (PID reciclado).
func randomForest(rng *rand.Rand) []Proc {
	n := 1 + rng.IntN(30)
	pids := rng.Perm(2000)[:n] // únicos
	procs := make([]Proc, n)
	for i := range procs {
		procs[i] = Proc{
			ID:     ID{PID: uint32(pids[i]) + 1, Created: at(rng.IntN(1000))},
			Parent: uint32(pids[rng.IntN(n)]) + 1, // apunta a algún PID del set
			Name:   "p.exe",
		}
		if rng.IntN(6) == 0 {
			procs[i].Parent = 0 // huérfano explícito
		}
	}
	return procs
}

func TestForestContraNaive(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 7))
	for iter := 0; iter < 500; iter++ {
		procs := randomForest(rng)
		f := NewForest(procs)
		nv := naïve{procs}

		for _, p := range procs {
			// Ancestros.
			var got []ID
			for _, a := range f.Ancestors(p.ID) {
				got = append(got, a.ID)
			}
			if want := nv.ancestors(p); !slices.Equal(got, want) {
				t.Fatalf("iter %d: Ancestors(%v) = %v, esperaba %v", iter, p.ID, got, want)
			}
			// Subárbol: mismos elementos (el orden BFS puede diferir del naïve,
			// pero el conjunto y la raíz-primero no).
			var sub []ID
			for _, d := range f.Subtree(p.ID) {
				sub = append(sub, d.ID)
			}
			if len(sub) == 0 || sub[0] != p.ID {
				t.Fatalf("iter %d: Subtree(%v) no arranca por la raíz: %v", iter, p.ID, sub)
			}
			if !sameSet(sub, nv.subtree(p.ID)) {
				t.Fatalf("iter %d: Subtree(%v) = %v, esperaba el conjunto %v", iter, p.ID, sub, nv.subtree(p.ID))
			}
			// Todo ancestro es ancestro; la raíz del subárbol es ancestro de todos.
			for _, d := range sub {
				if !f.IsAncestor(p.ID, d) {
					t.Fatalf("iter %d: %v debería ser ancestro de %v", iter, p.ID, d)
				}
			}
		}

		// LCA contra el naïve para todos los pares.
		for i := range procs {
			for j := range procs {
				a, b := procs[i].ID, procs[j].ID
				gotID, gotOK := ID{}, false
				if lca, ok := f.LCA(a, b); ok {
					gotID, gotOK = lca.ID, true
				}
				wantID, wantOK := nv.lca(a, b)
				if gotOK != wantOK || gotID != wantID {
					t.Fatalf("iter %d: LCA(%v,%v) = %v,%v; esperaba %v,%v", iter, a, b, gotID, gotOK, wantID, wantOK)
				}
				// El LCA, si existe, es ancestro de los dos.
				if gotOK && (!f.IsAncestor(gotID, a) || !f.IsAncestor(gotID, b)) {
					t.Fatalf("iter %d: LCA(%v,%v)=%v no es ancestro de ambos", iter, a, b, gotID)
				}
			}
		}
	}
}

func sameSet(a, b []ID) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[ID]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, c := range m {
		if c != 0 {
			return false
		}
	}
	return true
}

// TestPadreImpostor: un hijo cuyo Parent apunta a un PID que en la instantánea
// pertenece a un proceso más NUEVO que él. Ese "padre" no puede serlo (nació
// después): el hijo es raíz.
func TestPadreImpostor(t *testing.T) {
	procs := []Proc{
		{ID: ID{PID: 100, Created: at(500)}, Name: "hijo.exe", Parent: 200},
		{ID: ID{PID: 200, Created: at(900)}, Name: "impostor.exe"}, // reusó el PID 200 después
	}
	f := NewForest(procs)
	if _, ok := f.Parent(procs[0].ID); ok {
		t.Error("el hijo no debería tener padre: su 'padre' nació después que él")
	}
	if len(f.Ancestors(procs[0].ID)) != 0 {
		t.Error("sin padre válido, sin ancestros")
	}
	// Y con un padre legítimo (nacido antes), sí.
	procs[1].Created = at(100)
	f = NewForest(procs)
	if p, ok := f.Parent(procs[0].ID); !ok || p.PID != 200 {
		t.Error("con el padre nacido antes, la arista es válida")
	}
}

// TestRawChildrenFantasma: los hijos que heredaron el socket de un dueño
// muerto figuran con Parent = PID muerto y nacieron después del socket.
func TestRawChildrenFantasma(t *testing.T) {
	socketDesde := at(300)
	procs := []Proc{
		{ID: ID{PID: 50, Created: at(400)}, Name: "heredero.exe", Parent: 999}, // nació tras el socket
		{ID: ID{PID: 51, Created: at(100)}, Name: "viejo.exe", Parent: 999},    // nació antes: no heredó
		{ID: ID{PID: 52, Created: at(500)}, Name: "otro.exe", Parent: 12345},   // otro padre
	}
	f := NewForest(procs)
	heirs := f.RawChildren(999, socketDesde)
	if len(heirs) != 1 || heirs[0].PID != 50 {
		t.Fatalf("herederos = %v, esperaba solo el PID 50", heirs)
	}
	// Sin filtro de tiempo, ambos hijos del PID 999.
	if got := f.RawChildren(999, time.Time{}); len(got) != 2 {
		t.Errorf("sin filtro de tiempo esperaba 2, dio %d", len(got))
	}
}

// TestCrossLCA: descubrir al supervisor de un respawn. La víctima V vivía bajo
// el supervisor S (bosque de ANTES); tras matarla, S relanzó N con otro PID
// (bosque de DESPUÉS). El ancestro común, por IDENTIDAD, es S.
func TestCrossLCA(t *testing.T) {
	sup := ID{PID: 10, Created: at(0)} // sobrevive a los dos instantes
	before := NewForest([]Proc{
		{ID: sup, Name: "nodemon.exe"},
		{ID: ID{PID: 20, Created: at(100)}, Name: "node.exe", Parent: 10}, // víctima V
	})
	after := NewForest([]Proc{
		{ID: sup, Name: "nodemon.exe"},
		{ID: ID{PID: 21, Created: at(900)}, Name: "node.exe", Parent: 10}, // respawn N (PID nuevo)
	})
	victim := ID{PID: 20, Created: at(100)}
	nw := ID{PID: 21, Created: at(900)}
	if got, ok := CrossLCA(before, victim, after, nw); !ok || got != sup {
		t.Fatalf("CrossLCA = %v,%v; esperaba %v", got, ok, sup)
	}
	// services.exe como supervisor: mismo mecanismo.
	if _, ok := CrossLCA(before, victim, after, ID{PID: 999, Created: at(1)}); ok {
		t.Error("un dueño nuevo sin ancestro común no debería dar supervisor")
	}
}
