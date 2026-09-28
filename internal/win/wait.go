//go:build windows

package win

// wait.go: esperar la salida de N procesos SIN polling (WaitAny). El kernel
// avisa; no se pregunta en un bucle. WaitForMultipleObjects duerme hasta que
// un handle se señala (un proceso muere cuando su objeto se señala) o hasta un
// deadline, y un CreateEvent extra que se señala desde el context.Context
// despierta la espera al instante cuando el usuario cancela.
//
// Trampa de Windows: WaitForMultipleObjects admite a lo sumo MAXIMUM_WAIT_OBJECTS
// = 64 handles. Como uno es el evento de cancelación, con más de 63 procesos
// WaitAny los reparte en tandas que esperan a la vez, cada una en su
// goroutine, y vuelve con la PRIMERA salida de cualquiera (ver WaitAny).

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unsafe"
)

var (
	procCreateEventW           = modkernel32.NewProc("CreateEventW")
	procSetEvent               = modkernel32.NewProc("SetEvent")
	procWaitForMultipleObjects = modkernel32.NewProc("WaitForMultipleObjects")
)

const (
	maxWaitObjects = 64
	waitObject0    = 0x00000000
	waitTimeout    = 0x00000102
	waitFailed     = 0xFFFFFFFF
	infinite       = 0xFFFFFFFF
)

// Event es un evento de sincronización de Windows (auto-reset en off), usado
// como despertador de la espera.
type Event struct{ h uintptr }

// NewEvent crea un evento con reset manual, sin señalar.
func NewEvent() (*Event, error) {
	// CreateEventW(NULL, manualReset=TRUE, initialState=FALSE, NULL).
	h, e := call(procCreateEventW, 0, 1, 0, 0)
	if h == 0 {
		return nil, lastError(e)
	}
	return &Event{h: h}, nil
}

// Signal señala el evento (despierta cualquier espera que lo incluya).
func (ev *Event) Signal() error {
	if r, e := call(procSetEvent, ev.h); r == 0 {
		return lastError(e)
	}
	return nil
}

// Close libera el evento.
func (ev *Event) Close() {
	if ev.h != 0 {
		closeHandle(ev.h)
		ev.h = 0
	}
}

// WaitResult es por qué volvió WaitAny.
type WaitResult uint8

const (
	WaitExited   WaitResult = iota // salió uno (el índice devuelto)
	WaitDeadline                   // venció el plazo
	WaitCanceled                   // se canceló el contexto
)

// WaitAny espera a que salga CUALQUIERA de los handles, hasta deadline
// (cero = sin límite) o hasta que se cancele ctx, y devuelve el índice del que
// salió. Es la primitiva que usa el motor de killport: espera, registra la
// salida, quita ese handle y vuelve a esperar a los demás. Sin handles es una
// espera pura hasta el plazo (o la cancelación).
//
// Sin polling y sin punto ciego pasados los 63: cada tanda de hasta 63
// procesos duerme en su propio WaitForMultipleObjects junto con un evento
// DESPERTADOR compartido. La primera tanda que ve una salida señala el
// despertador y las demás vuelven al instante; la cancelación de ctx también
// lo señala. Con una sola tanda (el caso de siempre) no se lanza ninguna
// goroutine aparte de la que traduce ctx en el evento. Si en el mismo
// instante salieron varios, gana el de índice más bajo (determinista).
func WaitAny(ctx context.Context, handles []*Handle, deadline time.Time) (int, WaitResult, error) {
	if ctx.Err() != nil {
		return -1, WaitCanceled, nil
	}
	wake, err := NewEvent()
	if err != nil {
		return -1, WaitDeadline, err
	}
	defer wake.Close()

	stop := make(chan struct{})
	var watcher sync.WaitGroup
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		select {
		case <-ctx.Done():
			wake.Signal()
		case <-stop:
		}
	}()
	defer func() {
		close(stop)
		watcher.Wait()
	}()

	// Tandas de hasta 63 handles (+ el despertador = 64, el tope del kernel).
	const per = maxWaitObjects - 1
	batches := [][2]int{{0, 0}} // [desde, hasta) sobre handles; sin handles, una tanda vacía
	if len(handles) > 0 {
		batches = batches[:0]
		for lo := 0; lo < len(handles); lo += per {
			batches = append(batches, [2]int{lo, min(lo+per, len(handles))})
		}
	}

	type verdict struct {
		index  int // índice global del que salió, o -1
		failed error
	}
	waitBatch := func(b [2]int) verdict {
		raw := make([]uintptr, 0, b[1]-b[0]+1)
		for _, h := range handles[b[0]:b[1]] {
			raw = append(raw, h.Raw())
		}
		raw = append(raw, wake.h)
		r, e := call(procWaitForMultipleObjects, uintptr(len(raw)), uintptr(unsafe.Pointer(&raw[0])), 0, uintptr(timeoutMillis(deadline)))
		switch {
		case r == waitFailed:
			return verdict{index: -1, failed: lastError(e)}
		case r < uintptr(len(raw)-1): // WAIT_OBJECT_0 + i: salió handles[b[0]+i]
			wake.Signal() // que las otras tandas dejen de esperar
			return verdict{index: b[0] + int(r)}
		}
		return verdict{index: -1} // el despertador o el plazo
	}

	verdicts := make([]verdict, len(batches))
	if len(batches) == 1 {
		verdicts[0] = waitBatch(batches[0])
	} else {
		var wg sync.WaitGroup
		for i, b := range batches {
			wg.Add(1)
			go func() {
				defer wg.Done()
				verdicts[i] = waitBatch(b)
			}()
		}
		wg.Wait()
	}

	best := -1
	var failed error
	for _, v := range verdicts {
		if v.index >= 0 && (best < 0 || v.index < best) {
			best = v.index
		}
		if v.failed != nil {
			failed = v.failed
		}
	}
	switch {
	case best >= 0:
		return best, WaitExited, nil
	case ctx.Err() != nil:
		return -1, WaitCanceled, nil
	case failed != nil:
		return -1, WaitDeadline, fmt.Errorf("no pude esperar a los procesos: %w", failed)
	}
	return -1, WaitDeadline, nil
}

// timeoutMillis traduce un deadline a los milisegundos que espera Windows.
// Deadline cero = INFINITE (esperar sin límite). Un deadline ya vencido = 0
// (no dormir). El redondeo hacia arriba evita despertar un instante antes.
func timeoutMillis(deadline time.Time) uint32 {
	if deadline.IsZero() {
		return infinite
	}
	d := time.Until(deadline)
	if d <= 0 {
		return 0
	}
	ms := (d + time.Millisecond - 1) / time.Millisecond
	if ms >= infinite {
		return infinite - 1
	}
	return uint32(ms)
}
