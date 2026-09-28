package reaper

import "time"

// EventKind enumera todo lo que el motor informa mientras trabaja. La UI en
// vivo se suscribe a los eventos (por eso son tipados, no líneas de texto: el
// núcleo no arma la interfaz) y dibuja el spinner, la cuenta regresiva y las
// líneas de estado a partir de ellos.
type EventKind uint8

const (
	EvOpened           EventKind = iota // se abrió el handle de una víctima
	EvOpenFailed                        // no se pudo abrir (ver Err/Outcome)
	EvServiceStopping                   // se está deteniendo un servicio
	EvServiceStopped                    // el servicio quedó detenido
	EvServiceFailed                     // no se pudo detener el servicio
	EvCtrlC                             // se le mandó Ctrl+C a una consola
	EvCtrlCSkipped                      // NO se mandó Ctrl+C (ver Detail: consola propia/compartida)
	EvClose                             // se le mandó WM_CLOSE a las ventanas
	EvExited                            // una víctima salió (Method dice con qué, Elapsed cuánto tardó)
	EvTerminated                        // se terminó una víctima en seco
	EvTerminateFailed                   // no se pudo terminar (acceso denegado/protegido)
	EvVerifying                         // arranca la verificación posterior
	EvPortFreed                         // un puerto pedido quedó libre
	EvPortStuck                         // un puerto pedido sigue ocupado (Detail dice por qué)
	EvCanceled                          // el usuario cortó con Ctrl+C
	EvWaiting                           // arranca una espera con plazo (Deadline): la UI dibuja la cuenta regresiva
	EvExternalStopping                  // se pidió detener un objetivo externo (External; Method = MethodTerminate si va en seco, por --force)
	EvExternalStopped                   // un externo se detuvo o ya no estaba (Method, Elapsed)
	EvExternalFailed                    // no se pudo detener un externo (Err)
)

var eventNames = [...]string{
	EvOpened: "abierto", EvOpenFailed: "no se abrió",
	EvServiceStopping: "deteniendo servicio", EvServiceStopped: "servicio detenido", EvServiceFailed: "servicio no detenido",
	EvCtrlC: "ctrl+c", EvCtrlCSkipped: "ctrl+c omitido", EvClose: "wm_close",
	EvExited: "salió", EvTerminated: "terminado", EvTerminateFailed: "no se terminó",
	EvVerifying: "verificando", EvPortFreed: "puerto libre", EvPortStuck: "puerto ocupado",
	EvCanceled: "cancelado", EvWaiting: "esperando",
	EvExternalStopping: "deteniendo externo", EvExternalStopped: "externo detenido", EvExternalFailed: "externo no detenido",
}

func (k EventKind) String() string {
	if int(k) < len(eventNames) && eventNames[k] != "" {
		return eventNames[k]
	}
	return "evento"
}

// Event es una novedad de la escalada. Es un valor: se manda por el callback
// Emit sin compartir estado. Los campos que no aplican quedan en cero.
type Event struct {
	Kind    EventKind
	Time    time.Time     // hora (virtual en tests) en que ocurrió
	Victim  ID            // la víctima involucrada, si aplica
	Name    string        // nombre del proceso o del servicio
	Method  Method        // en EvExited: con qué salió
	Elapsed time.Duration // en EvExited: cuánto tardó desde la señal
	Port    uint16        // en EvPortFreed / EvPortStuck
	Detail  string        // motivo/consejo en español, para mostrar tal cual
	Err     error         // el error crudo, cuando lo hubo
	// Deadline (en EvWaiting): hasta cuándo se espera. Con Method
	// MethodTerminate es la espera de algo que se terminó en seco; con
	// MethodEngine, la de los objetivos externos que su dominio está
	// deteniendo; si no, el plazo del cierre prolijo (--timeout).
	Deadline time.Time
	// External (en EvExternal*): el objetivo externo involucrado; Outcome, cómo terminó.
	External External
	Outcome  Outcome
	// En EvCtrlC: Members son las víctimas de esa consola (a todas les llegó);
	// Also, los que NO son víctimas y también lo recibieron: los ancestros que
	// comparten la consola (el npm o el supervisor que lanzó al servidor en
	// la misma ventana). Apretar Ctrl+C en esa ventana haría lo mismo, pero el
	// usuario tiene que saberlo.
	Members []ID
	Also    []Proc
}

// emitter reparte eventos al callback; con nil no hace nada (el núcleo no
// depende de que haya UI escuchando).
type emitter func(Event)

func (e emitter) emit(ev Event) {
	if e != nil {
		e(ev)
	}
}
