//go:build windows

package win

// pipe.go: el lado CLIENTE de un named pipe como net.Conn, para hablar HTTP
// con un motor de contenedores (Docker Desktop y Podman exponen su API en
// \\.\pipe\docker_engine y compañía) con la biblioteca estándar.
//
// La clave es abrir el pipe OVERLAPPED. Con un handle síncrono no hay forma
// confiable de cortar un ReadFile trabado (cerrar el handle desde otro hilo
// no lo destraba), y un motor colgado colgaría a killport. Con un handle
// asíncrono cada operación se puede cancelar (CancelIoEx): los deadlines y el
// cierre funcionan de verdad, y el peor caso es un error a tiempo. La lectura
// y la escritura están en el paquete pipeio (ahí se explica por qué no se
// usa os.NewFile).
//
// Seguridad: el pipe se abre con SECURITY_SQOS_PRESENT|SECURITY_IDENTIFICATION.
// Sin eso, el SERVIDOR del pipe puede suplantar al cliente con su token
// completo; y un proceso cualquiera del usuario puede crear un pipe con el
// nombre del motor antes que el motor (squatting). Si killport corre elevado,
// eso sería regalarle un token de administrador. Con IDENTIFICATION, el
// servidor solo puede saber quién es el cliente, no actuar como él.

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"github.com/agustinyarrus/killport/internal/win/pipeio"
)

var (
	procWaitNamedPipeW              = modkernel32.NewProc("WaitNamedPipeW")
	procGetNamedPipeServerProcessId = modkernel32.NewProc("GetNamedPipeServerProcessId")
)

const (
	genericRead            = 0x80000000
	fileFlagOverlapped     = 0x40000000
	securitySqosPresent    = 0x00100000
	securityIdentification = 0x00010000 // SecurityIdentification << 16

	errFileNotFound = 2
	errPathNotFound = 3
	errPipeBusy     = 231
	errSemTimeout   = 121
)

// ErrPipeNotFound: no hay ningún servidor escuchando en ese pipe (el motor no
// está corriendo). Es la respuesta normal en una máquina sin Docker.
var ErrPipeNotFound = errors.New("no hay nadie escuchando en ese pipe")

// PipeConn es una conexión a un named pipe. Es un net.Conn con deadlines: se
// puede leer y escribir a la vez, y Close destraba lo pendiente.
type PipeConn struct {
	*pipeio.Conn
	server uint32
}

// ServerPID es el PID del proceso que sirve el pipe (0 si no se pudo saber):
// sirve para mostrar QUIÉN contestó en nombre del motor.
func (c *PipeConn) ServerPID() uint32 { return c.server }

// DialPipe abre el named pipe path (\\.\pipe\nombre) para leer y escribir.
// Si todas las instancias están ocupadas espera (WaitNamedPipeW) hasta el
// deadline de ctx (o 2 s si ctx no tiene). Errores distinguibles:
// ErrPipeNotFound (nadie escucha), ErrAccessDenied (el pipe existe pero su
// ACL no deja: en Docker Desktop, no estar en el grupo docker-users).
func DialPipe(ctx context.Context, path string) (*PipeConn, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(2 * time.Second)
	}
	name := UTF16Ptr(path)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, e := call(procCreateFileW, uintptr(unsafe.Pointer(name)), genericRead|genericWrite, 0, 0,
			openExisting, fileFlagOverlapped|securitySqosPresent|securityIdentification, 0)
		if h != invalidHandleValue {
			var server uint32
			if r, _ := call(procGetNamedPipeServerProcessId, h, uintptr(unsafe.Pointer(&server))); r == 0 {
				server = 0
			}
			conn, err := pipeio.New(syscall.Handle(h), path)
			if err != nil {
				closeHandle(h)
				return nil, err
			}
			return &PipeConn{Conn: conn, server: server}, nil
		}
		switch e {
		case errFileNotFound, errPathNotFound:
			return nil, ErrPipeNotFound
		case 5: // ERROR_ACCESS_DENIED
			return nil, ErrAccessDenied
		case errPipeBusy:
			// Todas las instancias ocupadas: esperar a que se libere una, sin
			// sondear (WaitNamedPipe duerme en el kernel), hasta el deadline.
			ms := time.Until(deadline).Milliseconds()
			if ms <= 0 {
				return nil, fmt.Errorf("el pipe %s está ocupado: %w", path, context.DeadlineExceeded)
			}
			if r, e := call(procWaitNamedPipeW, uintptr(unsafe.Pointer(name)), uintptr(ms)); r == 0 {
				if e == errSemTimeout {
					return nil, fmt.Errorf("el pipe %s está ocupado: %w", path, context.DeadlineExceeded)
				}
				if e == errFileNotFound {
					return nil, ErrPipeNotFound
				}
			}
			// Se liberó una instancia (o el servidor la recreó): reintentar.
		default:
			return nil, fmt.Errorf("no pude abrir %s: %w", path, lastError(e))
		}
	}
}
