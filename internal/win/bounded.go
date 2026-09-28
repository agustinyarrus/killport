//go:build windows

package win

// bounded.go: el EJECUTOR ACOTADO que comparten todos los comandos externos de
// killport (netsh, wsl.exe; Docker no: va por su API). Un comando externo es
// terreno ajeno: puede colgarse, escribir sin fin, abrir una ventana o dejar
// nietos vivos. Acá cada una de esas cosas tiene su tope:
//
//   - TIEMPO: un plazo total; al vencer se mata el árbol entero (ErrTimeout).
//   - VENTANAS: CREATE_NO_WINDOW (sin consola nueva) y SW_HIDE (sin ventana si
//     el hijo fuera gráfico). Además el Job lleva DIE_ON_UNHANDLED_EXCEPTION:
//     si el hijo revienta, muere en silencio en vez de abrir el diálogo de
//     Informe de errores de Windows.
//   - NADA COLGADO: el hijo nace SUSPENDIDO, se lo mete en un Job Object con
//     KILL_ON_JOB_CLOSE y recién ahí se lo deja correr. Todo lo que lance
//     queda en el mismo Job, y el Job muere con su último handle: aunque
//     killport mismo se cayera a mitad de camino, Windows mata el árbol al
//     cerrar el handle junto con el proceso. Suspenderlo primero cierra la
//     carrera de "lanzó un nieto antes de que lo metiera en el Job".
//   - SALIDA: la salida estándar tiene un tope; pasarlo mata el árbol y es
//     error (ErrOutputLimit), para no guardar en memoria lo que un hijo
//     descontrolado escupa. La de errores se recorta en silencio.
//
// Un código de salida distinto de cero NO es error acá: wsl.exe, por ejemplo,
// sale con -1 al listar sin distribuciones. Decide quien llama.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	procCreateJobObjectW         = modkernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = modkernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = modkernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = modkernel32.NewProc("TerminateJobObject")
	procNtResumeProcess          = modntdll.NewProc("NtResumeProcess")
)

const (
	createSuspended = 0x00000004
	createNoWindow  = 0x08000000

	jobObjectExtendedLimitInformation = 9      // JOBOBJECTINFOCLASS
	jobObjectLimitDieOnUnhandledExc   = 0x0400 // JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION
	jobObjectLimitKillOnJobClose      = 0x2000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE

	processSetQuota     = 0x0100 // PROCESS_SET_QUOTA: lo pide AssignProcessToJobObject
	processSuspendResum = 0x0800 // PROCESS_SUSPEND_RESUME

	defaultBoundedTimeout = 30 * time.Second
	defaultBoundedOutput  = 1 << 20
	boundedStderrMax      = 64 << 10
)

// jobBasicLimit es JOBOBJECT_BASIC_LIMIT_INFORMATION (64 bytes en x64; el
// relleno tras LimitFlags y ActiveProcessLimit lo pone Go al alinear los
// SIZE_T a 8). jobExtendedLimit es JOBOBJECT_EXTENDED_LIMIT_INFORMATION
// (144 bytes). Un test verifica los tamaños con unsafe.Sizeof.
type jobBasicLimit struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobExtendedLimit struct {
	Basic                 jobBasicLimit
	IoCounters            [6]uint64 // IO_COUNTERS
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// Bounded describe los topes de una ejecución.
type Bounded struct {
	Timeout   time.Duration // plazo total; cero = 30 s
	MaxOutput int           // bytes de salida estándar aceptados; cero = 1 MiB
	Env       []string      // variables agregadas al entorno heredado ("CLAVE=valor")
	Stdin     []byte        // lo que recibe por la entrada estándar (nil = nada: lee del dispositivo nulo)
}

// LaunchError es un programa que no se pudo lanzar. Dice cuál (el nombre, no
// la ruta dos veces) y por qué, sin la jerga de Go ("fork/exec RUTA: …"): el
// error de Windows con su número, y en castellano los que se ven en la
// práctica (un Job que no deja crear procesos, AppLocker).
type LaunchError struct {
	Exe string
	Err error
}

func (e *LaunchError) Unwrap() error { return e.Err }

func (e *LaunchError) Error() string {
	return "no pude lanzar " + filepath.Base(e.Exe) + ": " + motivoLanzar(e.Err)
}

// motivoLanzar traduce el error de CreateProcess.
func motivoLanzar(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		var pe *os.PathError
		if errors.As(err, &pe) {
			return pe.Err.Error()
		}
		return err.Error()
	}
	n := strconv.FormatUint(uint64(errno), 10)
	switch errno {
	case 1816: // ERROR_NOT_ENOUGH_QUOTA
		return "Windows no deja crear el proceso: cuota insuficiente (error 1816; ¿corre dentro de un Job que limita los procesos?)"
	case 5: // ERROR_ACCESS_DENIED
		return "acceso denegado (error 5; ¿una política como AppLocker?)"
	case 2, 3: // ERROR_FILE_NOT_FOUND, ERROR_PATH_NOT_FOUND
		return "no está (error " + n + ")"
	case 1455: // ERROR_COMMITMENT_LIMIT
		return "no alcanza la memoria virtual (error 1455)"
	case 1260: // ERROR_ACCESS_DISABLED_BY_POLICY
		return "lo bloquea una política del sistema (error 1260)"
	}
	return errno.Error() + " (error " + n + ")"
}

// BoundedResult es lo que dejó un comando que terminó dentro de sus topes.
type BoundedResult struct {
	Stdout   []byte
	Stderr   []byte // recortada a 64 KiB
	ExitCode uint32
	Elapsed  time.Duration
}

var (
	// ErrTimeout: el comando no terminó antes del plazo; se mató el árbol.
	ErrTimeout = errors.New("el comando no terminó a tiempo")
	// ErrOutputLimit: el comando escribió más de lo aceptado; se mató el árbol.
	ErrOutputLimit = errors.New("el comando escribió más de lo esperado")
)

// SystemExe es la ruta absoluta de un ejecutable de System32. Los comandos del
// sistema se lanzan por ruta completa: con el nombre pelado, la búsqueda
// empieza por la carpeta actual y un netsh.exe plantado ahí ganaría.
func SystemExe(name string) string { return systemDirectory + `\` + name }

// RunBounded corre exe con args dentro de los topes de b y devuelve su salida.
// Errores: ErrTimeout, ErrOutputLimit, ctx.Err() si se canceló (en los tres
// casos el árbol entero ya está muerto), o el error de lanzarlo.
func RunBounded(ctx context.Context, exe string, args []string, b Bounded) (BoundedResult, error) {
	if b.Timeout <= 0 {
		b.Timeout = defaultBoundedTimeout
	}
	if b.MaxOutput <= 0 {
		b.MaxOutput = defaultBoundedOutput
	}
	job, err := newKillJob()
	if err != nil {
		return BoundedResult{}, err
	}
	defer job.close() // KILL_ON_JOB_CLOSE: lo que siguiera vivo, muere acá

	runCtx, cancel := context.WithTimeout(ctx, b.Timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow | createSuspended}
	if len(b.Env) > 0 {
		cmd.Env = append(os.Environ(), b.Env...)
	}
	if b.Stdin != nil {
		cmd.Stdin = bytes.NewReader(b.Stdin)
	}
	stdout := &capped{max: b.MaxOutput, onOver: job.terminate}
	stderr := &capped{max: boundedStderrMax, silent: true}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Al vencer el plazo (o cancelar), matar el ÁRBOL, no solo al hijo: es lo
	// que el Job hace posible y lo que exec.CommandContext solo no haría.
	cmd.Cancel = func() error { job.terminate(); return nil }
	cmd.WaitDelay = time.Second

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return BoundedResult{}, &LaunchError{Exe: exe, Err: err}
	}
	if err := job.adopt(uint32(cmd.Process.Pid)); err != nil {
		cmd.Process.Kill() // nunca corrió: está suspendido
		cmd.Wait()
		return BoundedResult{}, err
	}
	waitErr := cmd.Wait()
	res := BoundedResult{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes(), Elapsed: time.Since(start)}
	if cmd.ProcessState != nil {
		res.ExitCode = uint32(cmd.ProcessState.ExitCode())
	}
	switch {
	case stdout.over:
		return res, ErrOutputLimit
	case ctx.Err() != nil:
		return res, ctx.Err()
	case runCtx.Err() != nil:
		return res, ErrTimeout
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return res, waitErr
	}
	return res, nil
}

// killJob es un Job Object que mata a todos sus procesos al cerrarse.
type killJob struct {
	mu sync.Mutex
	h  uintptr
}

func newKillJob() (*killJob, error) {
	h, e := call(procCreateJobObjectW, 0, 0)
	if h == 0 {
		return nil, fmt.Errorf("no pude crear el Job Object: %w", lastError(e))
	}
	info := jobExtendedLimit{Basic: jobBasicLimit{LimitFlags: jobObjectLimitKillOnJobClose | jobObjectLimitDieOnUnhandledExc}}
	if r, e := call(procSetInformationJobObject, h, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); r == 0 {
		closeHandle(h)
		return nil, fmt.Errorf("no pude configurar el Job Object: %w", lastError(e))
	}
	return &killJob{h: h}, nil
}

// adopt mete al proceso (recién creado y suspendido) en el Job y lo reanuda.
// El PID no se puede reciclar mientras tanto: exec.Cmd sostiene su handle.
func (j *killJob) adopt(pid uint32) error {
	h, err := openProcess(processSetQuota|processTerminate|processSuspendResum, pid)
	if err != nil {
		return fmt.Errorf("no pude abrir el proceso lanzado: %w", err)
	}
	defer closeHandle(h)
	if r, e := call(procAssignProcessToJobObject, j.h, h); r == 0 {
		return fmt.Errorf("no pude meter el proceso en el Job Object: %w", lastError(e))
	}
	// NtResumeProcess reanuda todos los hilos del proceso: exec.Cmd no expone
	// el handle del hilo principal que haría falta para ResumeThread.
	if st, _ := call(procNtResumeProcess, h); uint32(st) != statusSuccess {
		return fmt.Errorf("no pude reanudar el proceso lanzado: NTSTATUS 0x%x", uint32(st))
	}
	return nil
}

// terminate mata ya a todo el árbol (idempotente, seguro desde cualquier goroutine).
func (j *killJob) terminate() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.h != 0 {
		call(procTerminateJobObject, j.h, 1)
	}
}

func (j *killJob) close() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.h != 0 {
		closeHandle(j.h)
		j.h = 0
	}
}

// capped junta hasta max bytes. Al pasarse avisa (onOver) y descarta el
// resto sin bloquear al hijo. silent: recorta sin considerarlo un error.
type capped struct {
	buf    bytes.Buffer
	max    int
	over   bool
	silent bool
	onOver func()
}

func (c *capped) Write(p []byte) (int, error) {
	if c.over {
		return len(p), nil
	}
	if c.buf.Len()+len(p) > c.max {
		c.buf.Write(p[:c.max-c.buf.Len()])
		if !c.silent {
			c.over = true
			if c.onOver != nil {
				c.onOver()
			}
		}
		c.max = c.buf.Len() // lo que siga, se descarta
		return len(p), nil
	}
	return c.buf.Write(p)
}
