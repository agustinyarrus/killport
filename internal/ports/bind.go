package ports

// BindResult es lo que dio un sondeo de bind (bind sin listen, con uso
// EXCLUSIVO: SO_EXCLUSIVEADDRUSE, y en IPv6 sin doble pila) de un puerto en
// la dirección comodín de una familia. Es vocabulario de puertos, no de
// quien sondea: lo producen internal/win (el sondeo de verdad) y los dobles
// de las pruebas, y lo leen el diagnóstico de puertos sin dueño
// (internal/reaper) y la búsqueda de puertos libres (FindFree).
//
// Con uso exclusivo, BindForbidden es AMBIGUO: lo da un rango que Windows
// excluyó (WinNAT, Hyper-V, WSL, Docker) y también un socket ajeno que no se
// deja compartir. Por eso la tabla de sockets es la fuente primaria y el
// sondeo solo desempata.
type BindResult uint8

const (
	BindFree      BindResult = iota // se pudo atar: libre en todas las direcciones de esa familia
	BindInUse                       // WSAEADDRINUSE: alguien lo tiene atado, en la dirección que sea
	BindForbidden                   // WSAEACCES: rango excluido, reservado, o tomado por alguien que no comparte
	BindError                       // otro error al sondear: no se sabe
)

func (b BindResult) String() string {
	switch b {
	case BindFree:
		return "libre"
	case BindInUse:
		return "en uso"
	case BindForbidden:
		return "prohibido"
	}
	return "sin sondear"
}
