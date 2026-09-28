//go:build windows

package win

// La espera de un servicio (detener y arrancar comparten el mismo bucle) se
// prueba sin un servicio de verdad: la consulta se inyecta.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// consultas devuelve una consulta que va entregando los estados de a uno (el
// último se repite) y cuenta cuántas veces la llamaron.
func consultas(estados ...uint32) (func() (uint32, uint32, error), *int) {
	n := 0
	return func() (uint32, uint32, error) {
		i := min(n, len(estados)-1)
		n++
		return estados[i], 0, nil
	}, &n
}

func TestEsperaServicioLlega(t *testing.T) {
	q, n := consultas(serviceStopPending, serviceStopPending, serviceStopped)
	err := waitService(context.Background(), q, time.Now().Add(5*time.Second),
		func(st uint32) (bool, error) { return st == serviceStopped, nil })
	if err != nil || *n != 3 {
		t.Fatalf("err=%v consultas=%d (esperaba nil y 3)", err, *n)
	}
}

func TestEsperaServicioNoVaALlegar(t *testing.T) {
	// Arrancar un servicio que se detiene enseguida: el veredicto corta con su
	// error, sin agotar el plazo.
	q, _ := consultas(serviceStopped)
	malo := errors.New("arrancó y se detuvo enseguida")
	inicio := time.Now()
	err := waitService(context.Background(), q, time.Now().Add(5*time.Second),
		func(st uint32) (bool, error) {
			if st == serviceStopped {
				return true, malo
			}
			return st == serviceRunning, nil
		})
	if err != malo || time.Since(inicio) > time.Second {
		t.Fatalf("err=%v tras %v (esperaba el error del veredicto, al instante)", err, time.Since(inicio))
	}
}

func TestEsperaServicioVence(t *testing.T) {
	q, _ := consultas(serviceStopPending)
	err := waitService(context.Background(), q, time.Now().Add(150*time.Millisecond),
		func(st uint32) (bool, error) { return st == serviceStopped, nil })
	if err != ErrServiceTimeout {
		t.Fatalf("err=%v (esperaba ErrServiceTimeout)", err)
	}
}

func TestEsperaServicioCtrlC(t *testing.T) {
	q, _ := consultas(serviceStopPending)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	inicio := time.Now()
	err := waitService(ctx, q, time.Now().Add(time.Minute),
		func(st uint32) (bool, error) { return st == serviceStopped, nil })
	if !errors.Is(err, context.Canceled) || time.Since(inicio) > time.Second {
		t.Fatalf("err=%v tras %v (esperaba context.Canceled enseguida)", err, time.Since(inicio))
	}
}
