//go:build windows

// Package pipeio es la entrada y salida de un named pipe abierto OVERLAPPED,
// con la forma de un net.Conn: se lee y se escribe a la vez, cada sentido
// tiene su deadline, y Close destraba lo que esté pendiente. Lo usan el
// cliente (win.DialPipe) y el servidor de las pruebas (win/pipetest).
//
// ¿Por qué no os.NewFile, que desde Go 1.25 asocia un handle overlapped al
// puerto de completación del runtime y ya trae deadlines? Porque os.File
// lleva UN offset por archivo y lo actualiza después de cada operación, y en
// un pipe Read y Write lo hacen cada uno con su candado (el de lectura y el
// de escritura son distintos). Leer y escribir a la vez el mismo os.File —lo
// que hace cualquier cliente o servidor HTTP— es entonces una carrera de
// datos DENTRO de la biblioteca estándar (internal/poll.(*FD).addOffset;
// visto con Go 1.26.4). Es inofensiva, porque el kernel ignora el offset en
// un pipe, pero deja inservible al detector de carreras para todo lo que
// pase por el pipe: tapa las carreras de verdad. Acá no hay estado
// compartido entre los dos sentidos.
//
// Cómo funciona. Cada operación arma su OVERLAPPED con el evento de su
// sentido y llama a ReadFile o WriteFile. Si queda pendiente, la goroutine
// duerme en el kernel (WaitForMultipleObjects) hasta que la operación termine
// o se cierre la conexión: no sondea. El deadline es un temporizador que
// CANCELA la operación (CancelIoEx): la espera despierta sola, con el motivo.
// Una operación pendiente ocupa un hilo del sistema mientras espera; son a lo
// sumo dos por conexión, y killport abre un puñado de conexiones que viven
// milisegundos.
//
// Invariantes:
//   - el OVERLAPPED y el búfer de una operación no se sueltan hasta que el
//     kernel la dio por terminada (GetOverlappedResult con espera), también
//     si se la canceló: el kernel escribe en ellos hasta ese momento;
//   - el handle no se cierra con una operación en vuelo: Close avisa, espera
//     a que los dos sentidos terminen y recién entonces lo cierra (un handle
//     cerrado se recicla, y una operación tardía le pegaría a otro objeto);
//   - un temporizador viejo no cancela una operación nueva: cancela solo si
//     la operación pendiente sigue siendo la suya;
//   - el handle ES overlapped, y se comprueba al envolverlo: con uno síncrono
//     el kernel hace las operaciones de a una (una escritura esperaría a la
//     lectura pendiente) y la espera de acá no podría despertar a nadie.
package pipeio

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// kernel32 y ntdll son KnownDLL: Windows las carga siempre desde System32.
var (
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	ntdll                      = syscall.NewLazyDLL("ntdll.dll")
	procNtQueryInformationFile = ntdll.NewProc("NtQueryInformationFile")
	procReadFile               = kernel32.NewProc("ReadFile")
	procWriteFile              = kernel32.NewProc("WriteFile")
	procCreateEventW           = kernel32.NewProc("CreateEventW")
	procSetEvent               = kernel32.NewProc("SetEvent")
	procResetEvent             = kernel32.NewProc("ResetEvent")
	procCancelIoEx             = kernel32.NewProc("CancelIoEx")
	procGetOverlappedResult    = kernel32.NewProc("GetOverlappedResult")
	procWaitForMultipleObjects = kernel32.NewProc("WaitForMultipleObjects")
	procCloseHandle            = kernel32.NewProc("CloseHandle")
)

const (
	errHandleEOF        = syscall.Errno(38)
	errBrokenPipe       = syscall.Errno(109)
	errNoData           = syscall.Errno(232) // el otro extremo está cerrando
	errPipeNotConnected = syscall.Errno(233)
	errMoreData         = syscall.Errno(234) // pipe de mensajes: el mensaje no entró entero
	errOperationAborted = syscall.Errno(995) // la canceló CancelIoEx
	errIOPending        = syscall.Errno(997)

	waitObject0 = 0
	infinite    = 0xFFFFFFFF

	fileModeInformation      = 16   // FILE_INFORMATION_CLASS
	fileSynchronousIOAlert   = 0x10 // FILE_SYNCHRONOUS_IO_ALERT
	fileSynchronousIONonAlrt = 0x20 // FILE_SYNCHRONOUS_IO_NONALERT

	// ReadFile y WriteFile reciben el largo en un DWORD.
	maxChunk = 1 << 30
)

// call invoca un procedimiento. La directiva uintptrescapes manda al heap
// (que no se mueve) todo lo que se pasa convertido a uintptr y lo mantiene
// vivo hasta que la llamada vuelve; ver win.call.
//
//go:uintptrescapes
func call(p *syscall.LazyProc, args ...uintptr) (uintptr, syscall.Errno) {
	r, _, e := syscall.SyscallN(p.Addr(), args...)
	return r, e
}

// Conn es un extremo de un named pipe. Es un net.Conn.
type Conn struct {
	h       uintptr
	path    string
	closeEv uintptr // reset manual: Close lo señala y queda señalado

	closing  atomic.Bool
	once     sync.Once
	closeErr error

	rd, wr half
}

// half es un sentido (lectura o escritura) de la conexión.
type half struct {
	op sync.Mutex // una operación a la vez en este sentido; Close lo toma para esperar a la que esté en vuelo
	ev uintptr    // evento de reset manual del OVERLAPPED

	mu       sync.Mutex          // protege lo que sigue
	deadline time.Time           // cero = sin plazo
	timer    *time.Timer         // vencimiento de la operación pendiente
	pending  *syscall.Overlapped // la operación en vuelo (nil si no hay)
	expired  bool                // a la pendiente la cortó el deadline
}

// ErrNotOverlapped: el handle no se abrió con FILE_FLAG_OVERLAPPED.
var ErrNotOverlapped = errors.New("el handle no es overlapped")

// overlapped dice si el handle se abrió para E/S asíncrona. Lo contesta el
// kernel (el modo del objeto archivo): no hay que creerle a quien lo abrió.
func overlapped(h syscall.Handle) (bool, error) {
	var iosb [2]uintptr // IO_STATUS_BLOCK
	var mode uint32     // FILE_MODE_INFORMATION
	st, _ := call(procNtQueryInformationFile, uintptr(h), uintptr(unsafe.Pointer(&iosb)),
		uintptr(unsafe.Pointer(&mode)), unsafe.Sizeof(mode), fileModeInformation)
	if uint32(st) != 0 {
		return false, fmt.Errorf("NtQueryInformationFile: NTSTATUS 0x%x", uint32(st))
	}
	return mode&(fileSynchronousIOAlert|fileSynchronousIONonAlrt) == 0, nil
}

// New envuelve un handle de pipe abierto con FILE_FLAG_OVERLAPPED (si no lo
// es, ErrNotOverlapped). Si devuelve error, el handle sigue siendo de quien
// llamó; si no, es de la conexión (lo cierra Close).
func New(h syscall.Handle, path string) (*Conn, error) {
	if ok, err := overlapped(h); err != nil {
		return nil, fmt.Errorf("no pude consultar el modo de %s: %w", path, err)
	} else if !ok {
		return nil, fmt.Errorf("%s: %w", path, ErrNotOverlapped)
	}
	c := &Conn{h: uintptr(h), path: path}
	for _, ev := range [...]*uintptr{&c.closeEv, &c.rd.ev, &c.wr.ev} {
		// CreateEventW(NULL, manualReset=TRUE, initialState=FALSE, NULL).
		e, errno := call(procCreateEventW, 0, 1, 0, 0)
		if e == 0 {
			c.closeEvents()
			return nil, fmt.Errorf("no pude crear un evento para %s: %w", path, orInvalid(errno))
		}
		*ev = e
	}
	return c, nil
}

// Read lee lo que haya (hasta len(b)). io.EOF cuando el otro extremo cerró.
func (c *Conn) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if len(b) > maxChunk {
		b = b[:maxChunk]
	}
	for {
		n, err := c.transfer(&c.rd, procReadFile, "read", b)
		if n > 0 || err != nil {
			return n, err
		}
		// Cero bytes sin error: el otro extremo escribió un mensaje vacío. No
		// es fin de archivo ni un dato: se sigue leyendo.
	}
}

// Write escribe todo b, o devuelve por qué no pudo (con lo que llegó a salir).
func (c *Conn) Write(b []byte) (int, error) {
	total := 0
	for total < len(b) {
		chunk := b[total:]
		if len(chunk) > maxChunk {
			chunk = chunk[:maxChunk]
		}
		n, err := c.transfer(&c.wr, procWriteFile, "write", chunk)
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, c.fail("write", io.ErrShortWrite)
		}
	}
	return total, nil
}

// transfer hace UNA operación de un sentido y espera a que termine.
func (c *Conn) transfer(hf *half, proc *syscall.LazyProc, op string, b []byte) (int, error) {
	hf.op.Lock()
	defer hf.op.Unlock()
	if c.closing.Load() {
		return 0, c.fail(op, net.ErrClosed)
	}

	// El kernel escribe en el OVERLAPPED y en el búfer hasta que la operación
	// termina: los dos quedan clavados hasta entonces.
	ov := &syscall.Overlapped{HEvent: syscall.Handle(hf.ev)}
	data := unsafe.SliceData(b)
	var pin runtime.Pinner
	pin.Pin(ov)
	pin.Pin(data)
	defer pin.Unpin()

	hf.mu.Lock()
	if !hf.deadline.IsZero() && !time.Now().Before(hf.deadline) {
		hf.mu.Unlock()
		return 0, c.fail(op, os.ErrDeadlineExceeded)
	}
	hf.pending, hf.expired = ov, false
	hf.arm(c)
	hf.mu.Unlock()
	defer hf.disarm()

	call(procResetEvent, hf.ev)
	var n uint32
	r, e := call(proc, c.h, uintptr(unsafe.Pointer(data)), uintptr(len(b)), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(ov)))
	if r == 0 && e == errIOPending {
		// Si el deadline venció entre que se armó el temporizador y que la
		// operación quedó pendiente, su CancelIoEx no encontró nada que
		// cancelar: se cancela ahora. (Si vence después de esta pregunta, la
		// operación ya está pendiente y la cancela el temporizador.)
		hf.mu.Lock()
		late := hf.expired
		hf.mu.Unlock()
		if late {
			call(procCancelIoEx, c.h, uintptr(unsafe.Pointer(ov)))
		} else {
			hs := [2]uintptr{hf.ev, c.closeEv}
			w, _ := call(procWaitForMultipleObjects, 2, uintptr(unsafe.Pointer(&hs[0])), 0, infinite)
			if w != waitObject0 {
				// Close (o una espera que falló): cortar la operación.
				call(procCancelIoEx, c.h, uintptr(unsafe.Pointer(ov)))
			}
		}
		// Con espera: recién cuando el kernel la da por terminada se pueden
		// soltar el OVERLAPPED y el búfer. Una operación que llegó a
		// completarse antes de la cancelación cuenta como completa.
		r, e = call(procGetOverlappedResult, c.h, uintptr(unsafe.Pointer(ov)), uintptr(unsafe.Pointer(&n)), 1)
	}
	if r != 0 {
		return int(n), nil
	}
	switch e {
	case errMoreData:
		return int(n), nil // el resto del mensaje sale en la próxima lectura
	case errOperationAborted:
		hf.mu.Lock()
		expired := hf.expired
		hf.mu.Unlock()
		switch {
		case expired:
			return int(n), c.fail(op, os.ErrDeadlineExceeded)
		case c.closing.Load():
			return int(n), c.fail(op, net.ErrClosed)
		}
	case errBrokenPipe, errPipeNotConnected, errNoData, errHandleEOF:
		if op == "read" {
			return int(n), io.EOF
		}
	}
	return int(n), c.fail(op, orInvalid(e))
}

// arm programa el vencimiento de la operación pendiente según el deadline
// (con hf.mu tomado). Reemplaza al temporizador anterior.
func (hf *half) arm(c *Conn) {
	if hf.timer != nil {
		hf.timer.Stop()
		hf.timer = nil
	}
	if hf.pending == nil || hf.deadline.IsZero() {
		return
	}
	ov := hf.pending
	hf.timer = time.AfterFunc(time.Until(hf.deadline), func() { hf.expire(c, ov) })
}

// expire corta la operación ov si sigue siendo la pendiente. Corre en la
// goroutine del temporizador.
func (hf *half) expire(c *Conn, ov *syscall.Overlapped) {
	hf.mu.Lock()
	defer hf.mu.Unlock()
	if hf.pending != ov {
		return // esa operación ya terminó: no se toca a la que vino después
	}
	hf.expired = true
	call(procCancelIoEx, c.h, uintptr(unsafe.Pointer(ov)))
}

// disarm da por terminada la operación pendiente.
func (hf *half) disarm() {
	hf.mu.Lock()
	hf.pending = nil
	if hf.timer != nil {
		hf.timer.Stop()
		hf.timer = nil
	}
	hf.mu.Unlock()
}

// SetDeadline fija el plazo de los dos sentidos. Vale también para lo que ya
// está pendiente. El cero lo quita.
func (c *Conn) SetDeadline(t time.Time) error {
	if err := c.setDeadline(&c.rd, t); err != nil {
		return err
	}
	return c.setDeadline(&c.wr, t)
}

// SetReadDeadline fija el plazo de las lecturas.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.setDeadline(&c.rd, t) }

// SetWriteDeadline fija el plazo de las escrituras.
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.setDeadline(&c.wr, t) }

func (c *Conn) setDeadline(hf *half, t time.Time) error {
	if c.closing.Load() {
		return c.fail("set", net.ErrClosed)
	}
	hf.mu.Lock()
	hf.deadline = t
	hf.arm(c) // si hay una operación pendiente, su vencimiento pasa a ser este
	hf.mu.Unlock()
	return nil
}

// Close cierra la conexión y destraba lo que esté pendiente, que vuelve con
// net.ErrClosed. Espera a que los dos sentidos terminen antes de soltar el
// handle. Se puede llamar más de una vez y desde cualquier goroutine.
func (c *Conn) Close() error {
	c.once.Do(func() {
		c.closing.Store(true)
		call(procSetEvent, c.closeEv)
		c.rd.op.Lock()
		c.wr.op.Lock()
		if r, e := call(procCloseHandle, c.h); r == 0 {
			c.closeErr = c.fail("close", orInvalid(e))
		}
		c.closeEvents()
		c.wr.op.Unlock()
		c.rd.op.Unlock()
	})
	return c.closeErr
}

func (c *Conn) closeEvents() {
	for _, ev := range [...]*uintptr{&c.closeEv, &c.rd.ev, &c.wr.ev} {
		if *ev != 0 {
			call(procCloseHandle, *ev)
			*ev = 0
		}
	}
}

// LocalAddr y RemoteAddr completan net.Conn: un pipe no tiene direcciones de
// red; se informa su nombre.
func (c *Conn) LocalAddr() net.Addr  { return Addr(c.path) }
func (c *Conn) RemoteAddr() net.Addr { return Addr(c.path) }

// Addr es el nombre de un pipe como net.Addr.
type Addr string

func (a Addr) Network() string { return "pipe" }
func (a Addr) String() string  { return string(a) }

// fail envuelve un error con la operación y el pipe, como hace os.File. Los
// centinelas siguen siendo distinguibles con errors.Is, y el vencimiento del
// deadline conserva su Timeout() (lo mira net/http).
func (c *Conn) fail(op string, err error) error {
	return &os.PathError{Op: op, Path: c.path, Err: err}
}

// orInvalid garantiza un error aunque la API haya fallado sin dejar código.
func orInvalid(e syscall.Errno) error {
	if e == 0 {
		return syscall.EINVAL
	}
	return e
}
