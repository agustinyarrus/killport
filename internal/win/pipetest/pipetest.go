//go:build windows

// Package pipetest es un SERVIDOR de named pipe mínimo para las pruebas: un
// net.Listener sobre \\.\pipe\… que sirve igual que un socket (http.Serve
// encima hace de motor de contenedores falso). Solo lo importan archivos
// _test.go: no entra en ningún ejecutable.
//
// Cada Accept crea una instancia nueva del pipe (OVERLAPPED, como las del
// motor real) y espera a que un cliente se conecte sin bloquear un hilo para
// siempre: la espera incluye un evento que Close señala, así que cerrar el
// listener destraba un Accept pendiente al instante. La conexión aceptada lee
// y escribe con pipeio, igual que el cliente.
package pipetest

import (
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/agustinyarrus/killport/internal/win/pipeio"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	advapi32                  = syscall.NewLazyDLL("advapi32.dll")
	procCreateNamedPipeW      = kernel32.NewProc("CreateNamedPipeW")
	procConnectNamedPipe      = kernel32.NewProc("ConnectNamedPipe")
	procCreateEventW          = kernel32.NewProc("CreateEventW")
	procSetEvent              = kernel32.NewProc("SetEvent")
	procWaitForMultipleObject = kernel32.NewProc("WaitForMultipleObjects")
	procCancelIoEx            = kernel32.NewProc("CancelIoEx")
	procGetOverlappedResult   = kernel32.NewProc("GetOverlappedResult")
	procLocalFree             = kernel32.NewProc("LocalFree")
	procConvertSDDL           = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
)

const (
	pipeAccessDuplex      = 0x00000003
	fileFlagOverlapped    = 0x40000000
	fileFlagFirstInstance = 0x00080000
	pipeTypeByte          = 0x00000000
	pipeUnlimited         = 255
	errIOPending          = 997
	errPipeConnected      = 535
	waitObject0           = 0
	infinite              = 0xFFFFFFFF
)

// Options ajusta el servidor.
type Options struct {
	MaxInstances int    // instancias simultáneas (0 = sin límite): 1 sirve para provocar ERROR_PIPE_BUSY
	SDDL         string // descriptor de seguridad del pipe ("" = el de siempre): "D:(D;;GA;;;WD)" niega a todos
}

// Listener es un net.Listener sobre un named pipe.
type Listener struct {
	path    string
	opts    Options
	closed  chan struct{}
	closeEv uintptr
	once    sync.Once
	mu      sync.Mutex
	first   bool
	next    uintptr        // la instancia ya creada que espera al próximo cliente
	pending sync.WaitGroup // Accept en curso: Close los espera antes de soltar el evento
}

var seq atomic.Int64

// Name devuelve un nombre de pipe único para una prueba.
func Name(tag string) string {
	return fmt.Sprintf(`\\.\pipe\killport-test-%s-%d-%d`, tag, os.Getpid(), seq.Add(1))
}

// Listen empieza a escuchar en path. La primera instancia se crea ya (con
// FILE_FLAG_FIRST_PIPE_INSTANCE: si el nombre existía, falla), así el pipe
// existe para los clientes desde que Listen vuelve; y cada Accept que
// conecta deja creada la siguiente, como un servidor de verdad (si no, entre
// dos Accept el nombre no existiría y un cliente vería "no hay nadie").
func Listen(path string, o Options) (*Listener, error) {
	ev, _, err := procCreateEventW.Call(0, 1, 0, 0)
	if ev == 0 {
		return nil, err
	}
	l := &Listener{path: path, opts: o, closed: make(chan struct{}), closeEv: ev, first: true}
	h, err2 := l.instance()
	if err2 != nil {
		syscall.CloseHandle(syscall.Handle(ev))
		return nil, err2
	}
	l.next = h
	return l, nil
}

// Addr es el nombre del pipe.
func (l *Listener) Addr() net.Addr { return pipeio.Addr(l.path) }

// Close deja de aceptar, destraba un Accept pendiente y, cuando ya no queda
// ninguno, libera el evento.
func (l *Listener) Close() error {
	l.once.Do(func() {
		l.mu.Lock() // ningún Accept se anota después de esto
		close(l.closed)
		l.mu.Unlock()
		procSetEvent.Call(l.closeEv)
		l.pending.Wait()
		syscall.CloseHandle(syscall.Handle(l.closeEv))
		l.mu.Lock()
		if l.next != 0 {
			syscall.CloseHandle(syscall.Handle(l.next))
			l.next = 0
		}
		l.mu.Unlock()
	})
	return nil
}

// Accept crea una instancia del pipe y espera a un cliente.
func (l *Listener) Accept() (net.Conn, error) {
	l.mu.Lock()
	select {
	case <-l.closed:
		l.mu.Unlock()
		return nil, net.ErrClosed
	default:
	}
	l.pending.Add(1)
	h := l.next
	l.next = 0
	l.mu.Unlock()
	defer l.pending.Done()
	if h == 0 {
		var err error
		if h, err = l.instance(); err != nil {
			return nil, err
		}
	}
	ev, _, e := procCreateEventW.Call(0, 1, 0, 0)
	if ev == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return nil, e
	}
	defer syscall.CloseHandle(syscall.Handle(ev))
	ov := new(syscall.Overlapped) // en el heap: el kernel lo escribe al completar
	ov.HEvent = syscall.Handle(ev)
	r, _, e := procConnectNamedPipe.Call(h, uintptr(unsafe.Pointer(ov)))
	if r == 0 {
		switch e.(syscall.Errno) {
		case errPipeConnected: // el cliente llegó entre la creación y el Connect
		case errIOPending:
			arr := [2]uintptr{ev, l.closeEv}
			w, _, _ := procWaitForMultipleObject.Call(2, uintptr(unsafe.Pointer(&arr[0])), 0, infinite)
			if w != waitObject0 {
				// Close: cancelar la operación y esperar a que el kernel la
				// dé por terminada ANTES de soltar la memoria de ov.
				procCancelIoEx.Call(h, uintptr(unsafe.Pointer(ov)))
				var n uint32
				procGetOverlappedResult.Call(h, uintptr(unsafe.Pointer(ov)), uintptr(unsafe.Pointer(&n)), 1)
				syscall.CloseHandle(syscall.Handle(h))
				return nil, net.ErrClosed
			}
			var n uint32
			if r, _, e := procGetOverlappedResult.Call(h, uintptr(unsafe.Pointer(ov)), uintptr(unsafe.Pointer(&n)), 0); r == 0 {
				syscall.CloseHandle(syscall.Handle(h))
				return nil, e
			}
		default:
			syscall.CloseHandle(syscall.Handle(h))
			return nil, e
		}
	}
	// La próxima instancia, ya: el nombre nunca queda sin nadie escuchando.
	// (Con MaxInstances alcanzado falla, y el próximo Accept la reintenta.)
	if nh, err := l.instance(); err == nil {
		l.mu.Lock()
		select {
		case <-l.closed:
			syscall.CloseHandle(syscall.Handle(nh))
		default:
			l.next = nh
		}
		l.mu.Unlock()
	}
	c, err := pipeio.New(syscall.Handle(h), l.path)
	if err != nil {
		syscall.CloseHandle(syscall.Handle(h))
		return nil, err
	}
	return c, nil
}

// instance crea una instancia del pipe con las opciones del listener.
func (l *Listener) instance() (uintptr, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	max := l.opts.MaxInstances
	if max <= 0 || max > pipeUnlimited {
		max = pipeUnlimited
	}
	flags := uintptr(pipeAccessDuplex | fileFlagOverlapped)
	if l.first {
		flags |= fileFlagFirstInstance
	}
	var sa *syscall.SecurityAttributes
	if l.opts.SDDL != "" {
		var sd uintptr
		p, _ := syscall.UTF16PtrFromString(l.opts.SDDL)
		if r, _, e := procConvertSDDL.Call(uintptr(unsafe.Pointer(p)), 1, uintptr(unsafe.Pointer(&sd)), 0); r == 0 {
			return 0, e
		}
		defer procLocalFree.Call(sd)
		sa = &syscall.SecurityAttributes{SecurityDescriptor: sd}
		sa.Length = uint32(unsafe.Sizeof(*sa))
	}
	name, _ := syscall.UTF16PtrFromString(l.path)
	h, _, e := procCreateNamedPipeW.Call(uintptr(unsafe.Pointer(name)), flags, pipeTypeByte, uintptr(max),
		64<<10, 64<<10, 0, uintptr(unsafe.Pointer(sa)))
	if syscall.Handle(h) == syscall.InvalidHandle {
		return 0, fmt.Errorf("CreateNamedPipe %s: %w", l.path, e)
	}
	l.first = false
	return h, nil
}
