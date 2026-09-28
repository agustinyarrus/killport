package reaper

import (
	"time"
)

// ID identifica un proceso SIN AMBIGÜEDAD. Un PID solo no alcanza: Windows los
// recicla apenas un proceso muere, así que el mismo número puede ser dos
// procesos distintos con minutos de diferencia. La hora de creación (con
// resolución de 100 ns) desempata. Todo lo que el motor decide y hace se
// refiere a identidades, nunca a PID pelados: entre que se lee la tabla y que
// se abre el handle, el PID pudo haberse reciclado, y actuar sobre el número
// sin confirmar la identidad sería matar a un inocente.
//
// ID se usa como clave de mapa y se compara con ==, y time.Time compara con ==
// también la ubicación y la lectura monotónica, no solo el instante: dos horas
// iguales armadas por caminos distintos (time.Unix, un .UTC(), un reloj con
// lectura monotónica) serían IDs distintos. Por eso las identidades se
// CANONIZAN (Canon: en UTC y sin lectura monotónica) al entrar al paquete:
// NewForest lo hace con toda la instantánea, y quien arme un ID a mano (el
// trabajo elevado que las recibe por argumentos) tiene que pasarlo por Canon.
type ID struct {
	PID     uint32
	Created time.Time
}

// Canon devuelve la forma canónica de la identidad: la hora en UTC, que en Go
// además descarta la lectura monotónica. Dos IDs del mismo proceso son == si
// y solo si sus formas canónicas lo son.
func (id ID) Canon() ID {
	if id.Created.IsZero() {
		return ID{PID: id.PID}
	}
	return ID{PID: id.PID, Created: id.Created.UTC()}
}

// Zero dice si la identidad es la nula (PID 0 sin hora). El PID 0 es el
// "System Idle Process": no es un proceso de verdad y jamás se toca.
func (id ID) Zero() bool { return id.PID == 0 && id.Created.IsZero() }

// Proc es una foto de un proceso en el instante de la instantánea. Es un valor:
// copiarlo es barato y no comparte estado.
type Proc struct {
	ID
	Parent    uint32 // PID del padre según el sistema (InheritedFromUniqueProcessId)
	Name      string // nombre de la imagen ("node.exe"); "" si no se supo
	Path      string // ruta completa; "" si no había permiso o no aplica
	SessionID uint32 // 0 = servicios/sistema; >0 = sesión interactiva
	// Critical es true solo si se CONFIRMÓ que el proceso está marcado como
	// crítico (ProcessBreakOnTermination): matarlo tira un bugcheck. Que sea
	// false no garantiza que no lo sea si no se pudo consultar; por eso la
	// clasificación de "crítico" mira también la lista de nombres conocidos.
	Critical bool
}

// Forest es el árbol de procesos de una instantánea. En rigor es un BOSQUE:
// hay varias raíces (procesos cuyo padre ya murió), y por eso el nombre.
//
// La arista hijo→padre es VÁLIDA solo si el padre existe en la instantánea con
// Created <= hijo.Created. Si el "padre" que informa el sistema nació DESPUÉS
// que el hijo, no puede ser su padre: el PID del padre real se recicló y ahora
// lo usa un impostor. En ese caso el hijo se toma como raíz. Esta es la regla
// que hace que el bosque nunca mienta por un PID reciclado.
//
// Construcción O(n): un mapa PID→índice y una pasada validando aristas. Como
// un PID puede repetirse en la instantánea (reciclado dentro de la misma foto,
// rarísimo pero posible), el mapa guarda el proceso MÁS NUEVO con ese PID, que
// es el único que puede seguir vivo y ser padre de algo.
type Forest struct {
	procs   []Proc         // en el orden de la instantánea
	byIDidx map[ID]int     // identidad → índice en procs (clave única)
	byPID   map[uint32]int // PID → índice del proceso más nuevo con ese PID
	kids    map[int][]int  // índice → índices de sus hijos (arista válida)
	parent  []int          // índice → índice del padre válido, o -1 si es raíz
}

// NewForest arma el bosque a partir de una instantánea. O(n) tiempo y memoria.
// Copia la instantánea con las identidades canonizadas (ver ID): el llamador
// conserva la suya intacta.
func NewForest(snapshot []Proc) *Forest {
	procs := make([]Proc, len(snapshot))
	for i, p := range snapshot {
		p.ID = p.ID.Canon()
		procs[i] = p
	}
	f := &Forest{
		procs:   procs,
		byIDidx: make(map[ID]int, len(procs)),
		byPID:   make(map[uint32]int, len(procs)),
		kids:    make(map[int][]int),
		parent:  make([]int, len(procs)),
	}
	for i, p := range procs {
		f.byIDidx[p.ID] = i
		// Se queda con el más nuevo por PID: un padre tuvo que existir ANTES
		// que su hijo, así que el candidato a padre es siempre el proceso vivo
		// con ese PID, que es el de creación más reciente.
		if j, ok := f.byPID[p.PID]; !ok || procs[j].Created.Before(p.Created) {
			f.byPID[p.PID] = i
		}
	}
	for i := range procs {
		f.parent[i] = f.validParent(i)
		if pi := f.parent[i]; pi >= 0 {
			f.kids[pi] = append(f.kids[pi], i)
		}
	}
	return f
}

// validParent devuelve el índice del padre real de procs[i], o -1. El padre
// real es el proceso con el PID que informa el sistema, PERO solo si nació en
// el mismo instante o antes que el hijo (Created <= hijo.Created). Un proceso
// nunca es su propio padre (PID 0 se apunta a sí mismo en la instantánea).
func (f *Forest) validParent(i int) int {
	p := f.procs[i]
	if p.Parent == 0 || p.Parent == p.PID {
		return -1
	}
	pi, ok := f.byPID[p.Parent]
	if !ok || pi == i {
		return -1
	}
	if f.procs[pi].Created.After(p.Created) {
		return -1 // el "padre" es más nuevo: PID reciclado, es un impostor
	}
	return pi
}

// index busca por identidad canonizada: las consultas aceptan IDs armados por
// cualquier camino (ver ID) y todas pasan por acá.
func (f *Forest) index(id ID) (int, bool) {
	i, ok := f.byIDidx[id.Canon()]
	return i, ok
}

// Len es la cantidad de procesos.
func (f *Forest) Len() int { return len(f.procs) }

// All recorre todos los procesos.
func (f *Forest) All() []Proc { return f.procs }

// Lookup busca un proceso por identidad exacta.
func (f *Forest) Lookup(id ID) (Proc, bool) {
	if i, ok := f.index(id); ok {
		return f.procs[i], true
	}
	return Proc{}, false
}

// RawChildren devuelve los procesos cuyo campo Parent (crudo, sin validar) es
// pid, hayan nacido cuando hayan nacido. Es lo que hace falta para los
// fantasmas: cuando un dueño murió, sus hijos figuran en la instantánea como
// raíces (su arista al padre se descartó por inválida), pero su Parent SIGUE
// apuntando al PID muerto. Con `since` no cero, se filtran los que nacieron
// después de ese instante: solo esos pudieron heredar un socket creado
// entonces (la herencia del handle ocurre al spawnearse el hijo). O(n).
func (f *Forest) RawChildren(pid uint32, since time.Time) []Proc {
	var out []Proc
	for _, p := range f.procs {
		if p.Parent == pid && p.PID != pid && (since.IsZero() || !p.Created.Before(since)) {
			out = append(out, p)
		}
	}
	return out
}

// Heirs devuelve los procesos que pueden haber HEREDADO un socket que creó
// pid en el instante since, cuando ese dueño ya murió: los hijos crudos del
// PID muerto (RawChildren) que además NO tienen padre válido en el bosque.
//
// Esa última condición es la que separa al heredero del inocente. Si Windows
// ya le dio el PID del muerto a otro proceso B, los hijos de B también figuran
// con Parent == pid, y nacieron después del socket; pero su arista a B es
// válida (B nació antes que ellos), así que son hijos de B y no pudieron
// heredar nada del muerto: cuando nacieron, el PID ya era de B. El heredero
// de verdad nació mientras el muerto vivía, o sea ANTES que B, y por eso su
// arista al PID es inválida (es raíz). O(n).
//
// No alcanza para decidir que se lo puede cerrar: si B también murió dejando
// un hijo C, nada en el bosque distingue a C de un heredero (le pasa a cada
// csrss y winlogon, hijos del smss transitorio de su sesión). Quien los vuelve
// objetivo tiene que pasarlos por las negativas de siempre.
func (f *Forest) Heirs(pid uint32, since time.Time) []Proc {
	var out []Proc
	for i, p := range f.procs {
		if p.Parent == pid && p.PID != pid && f.parent[i] < 0 && (since.IsZero() || !p.Created.Before(since)) {
			out = append(out, p)
		}
	}
	return out
}

// Holding es quién sostiene un socket de la tabla según la instantánea.
type Holding struct {
	// Owner es el dueño real, si vive: el proceso con ese PID que nació a más
	// tardar cuando el socket (Live). Uno más nuevo que el socket no pudo
	// crearlo: reusó el PID de un muerto.
	Owner Proc
	Live  bool
	// Heirs, si el dueño real ya no está, son los que pueden sostener el
	// socket porque lo heredaron (ver Heirs). Vacío: nadie; el kernel lo
	// suelta solo en un momento.
	Heirs []Proc
}

// Hold es LA respuesta a "¿de quién es este socket?" (el PID que informa la
// tabla y la hora de creación del socket; since cero = no se sabe, y se le
// cree al PID). La usan la planificación, la verificación, la espera, los
// huérfanos y el listado: todos tienen que contestar lo mismo, o el listado
// nombra a un proceso y la acción cierra a otro. O(n) solo si hace falta
// buscar herederos.
func (f *Forest) Hold(pid uint32, since time.Time) Holding {
	if p, ok := f.LivePID(pid); ok && (since.IsZero() || !p.Created.After(since)) {
		return Holding{Owner: p, Live: true}
	}
	return Holding{Heirs: f.Heirs(pid, since)}
}

// LivePID devuelve el proceso vivo con ese PID (el más nuevo). Sirve para
// resolver un PID de la tabla de sockets a una identidad. Si no hay ninguno, el
// dueño es un fantasma: murió y su socket lo sostiene un hijo que lo heredó.
// Ojo: el que tiene el PID hoy puede no ser quien creó un socket; para eso,
// Hold.
func (f *Forest) LivePID(pid uint32) (Proc, bool) {
	if i, ok := f.byPID[pid]; ok {
		return f.procs[i], true
	}
	return Proc{}, false
}

// Parent devuelve el padre válido de un proceso.
func (f *Forest) Parent(id ID) (Proc, bool) {
	i, ok := f.index(id)
	if !ok || f.parent[i] < 0 {
		return Proc{}, false
	}
	return f.procs[f.parent[i]], true
}

// Children devuelve los hijos directos (aristas válidas). O(hijos).
func (f *Forest) Children(id ID) []Proc {
	i, ok := f.index(id)
	if !ok {
		return nil
	}
	out := make([]Proc, 0, len(f.kids[i]))
	for _, k := range f.kids[i] {
		out = append(out, f.procs[k])
	}
	return out
}

// Ancestors devuelve la cadena de ancestros desde el padre hacia la raíz (sin
// incluir al propio proceso). O(profundidad); nunca cicla porque cada padre es
// estrictamente más viejo que su hijo. El primero es el padre, el último la raíz.
func (f *Forest) Ancestors(id ID) []Proc {
	i, ok := f.index(id)
	if !ok {
		return nil
	}
	var out []Proc
	for j := f.parent[i]; j >= 0; j = f.parent[j] {
		out = append(out, f.procs[j])
	}
	return out
}

// Subtree devuelve el subárbol de id en orden BFS: primero la raíz, después sus
// hijos, después los nietos. Ese orden importa para matar: un supervisor
// (la raíz) tiene que morir ANTES que sus hijos, o los relanza. O(nodos del
// subárbol). No cicla: las aristas van siempre de más viejo a más nuevo.
func (f *Forest) Subtree(id ID) []Proc {
	i, ok := f.index(id)
	if !ok {
		return nil
	}
	order := []int{i}
	for head := 0; head < len(order); head++ {
		order = append(order, f.kids[order[head]]...)
	}
	out := make([]Proc, len(order))
	for k, idx := range order {
		out[k] = f.procs[idx]
	}
	return out
}

// depth cuenta cuántas aristas hay de i a la raíz. O(profundidad).
func (f *Forest) depth(i int) int {
	d := 0
	for j := f.parent[i]; j >= 0; j = f.parent[j] {
		d++
	}
	return d
}

// IsAncestor dice si a es ancestro (propio o impropio) de b: subir desde b
// tiene que pasar por a. Con a == b devuelve true (todo nodo es ancestro
// impropio de sí mismo). O(profundidad de b).
func (f *Forest) IsAncestor(a, b ID) bool {
	ia, ok := f.index(a)
	if !ok {
		return false
	}
	ib, ok := f.index(b)
	if !ok {
		return false
	}
	for j := ib; j >= 0; j = f.parent[j] {
		if j == ia {
			return true
		}
	}
	return false
}

// LCA es el ancestro común más cercano de dos procesos DE ESTE bosque, o
// (Proc{}, false) si están en árboles distintos (no comparten raíz). Se sube el
// más profundo hasta igualar alturas y después los dos a la par hasta
// encontrarse. O(profundidad). Todo nodo es ancestro impropio de sí mismo, así
// que LCA(x, descendiente-de-x) = x.
func (f *Forest) LCA(a, b ID) (Proc, bool) {
	ia, ok := f.index(a)
	if !ok {
		return Proc{}, false
	}
	ib, ok := f.index(b)
	if !ok {
		return Proc{}, false
	}
	da, db := f.depth(ia), f.depth(ib)
	for da > db {
		ia = f.parent[ia]
		da--
	}
	for db > da {
		ib = f.parent[ib]
		db--
	}
	for ia != ib {
		ia, ib = f.parent[ia], f.parent[ib]
		if ia < 0 || ib < 0 {
			return Proc{}, false // raíces distintas: sin ancestro común
		}
	}
	return f.procs[ia], true
}

// CrossLCA encuentra al SUPERVISOR de un respawn: quién relanzó un proceso.
// old es la víctima (que ya murió; vive en el bosque de ANTES, f) y nw es el
// proceso que retomó el puerto (vive en el bosque de DESPUÉS, after). Se sube
// por los ancestros de la víctima en el bosque viejo y por los del nuevo dueño
// en el nuevo, y se busca la primera IDENTIDAD que aparezca en las dos cadenas:
// ese proceso sobrevivió a los dos instantes y es el que está relanzando (un
// nodemon, un pm2, services.exe...). Se une por identidad, no por PID, para no
// confundir un PID reciclado con el supervisor real.
//
// Devuelve la identidad común más cercana al nuevo dueño. O(profundidades).
func CrossLCA(before *Forest, old ID, after *Forest, nw ID) (ID, bool) {
	// Cadena de la víctima (incluyéndola) en el bosque de antes.
	oldChain := map[ID]bool{}
	if i, ok := before.index(old); ok {
		for j := i; j >= 0; j = before.parent[j] {
			oldChain[before.procs[j].ID] = true
		}
	}
	// Subiendo desde el nuevo dueño en el bosque de después, el primero que
	// también esté en la cadena vieja es el supervisor.
	if i, ok := after.index(nw); ok {
		for j := i; j >= 0; j = after.parent[j] {
			if id := after.procs[j].ID; oldChain[id] {
				return id, true
			}
		}
	}
	return ID{}, false
}
