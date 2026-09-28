//go:build windows

// serviciofalso es un SERVICIO DE WINDOWS de prueba para killport: uno "de
// terceros" (su ejecutable no está en la carpeta de Windows) que escucha un
// puerto, que es el caso de un MySQL o un PostgreSQL instalado como servicio.
// Lo usa la prueba manual con administrador (_oracle/admin.ps1), que lo
// registra con `sc create`, corre killport contra su puerto y lo borra.
//
// Habla con el administrador de servicios por la API de siempre
// (StartServiceCtrlDispatcherW, RegisterServiceCtrlHandlerExW,
// SetServiceStatus), sin dependencias: acepta la orden de detenerse, suelta el
// puerto y sale.
//
// Lanzado a mano (no por el administrador de servicios) no es un servicio:
// escucha el puerto como un programa cualquiera, escribe "PUERTO n" y queda
// vivo hasta que lo maten. Así se lo puede probar sin ser administrador.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	advapi             = syscall.NewLazyDLL(os.Getenv("SystemRoot") + `\System32\advapi32.dll`)
	procDispatcher     = advapi.NewProc("StartServiceCtrlDispatcherW")
	procRegisterCtrlEx = advapi.NewProc("RegisterServiceCtrlHandlerExW")
	procSetStatus      = advapi.NewProc("SetServiceStatus")
)

const (
	win32OwnProcess = 0x00000010 // SERVICE_WIN32_OWN_PROCESS

	stopped      = 1 // SERVICE_STOPPED
	startPending = 2 // SERVICE_START_PENDING
	stopPending  = 3 // SERVICE_STOP_PENDING
	running      = 4 // SERVICE_RUNNING

	acceptStop  = 0x00000001 // SERVICE_ACCEPT_STOP
	controlStop = 1          // SERVICE_CONTROL_STOP

	// ERROR_FAILED_SERVICE_CONTROLLER_CONNECT: el proceso no lo lanzó el
	// administrador de servicios.
	errNoEsServicio = syscall.Errno(1063)
)

// serviceStatus es SERVICE_STATUS.
type serviceStatus struct {
	serviceType, currentState, controlsAccepted uint32
	win32ExitCode, serviceSpecificExitCode      uint32
	checkPoint, waitHint                        uint32
}

// serviceTableEntry es SERVICE_TABLE_ENTRYW.
type serviceTableEntry struct {
	name *uint16
	proc uintptr
}

var (
	puerto  int
	handle  uintptr
	parar   = make(chan struct{})
	pararUn sync.Once
	vacio   = [1]uint16{0} // el nombre "" (un servicio con proceso propio no lo usa)
)

// informar le dice al administrador de servicios en qué estado está.
func informar(estado uint32) {
	st := serviceStatus{serviceType: win32OwnProcess, currentState: estado}
	switch estado {
	case running:
		st.controlsAccepted = acceptStop
	case startPending, stopPending:
		st.waitHint = 3000
	}
	procSetStatus.Call(handle, uintptr(unsafe.Pointer(&st)))
}

// controlador atiende las órdenes del administrador de servicios.
func controlador(control, _, _, _ uintptr) uintptr {
	if control == controlStop {
		informar(stopPending)
		pararUn.Do(func() { close(parar) })
	}
	return 0 // NO_ERROR
}

// principal es el ServiceMain: corre en un hilo del despachador.
func principal(_, _ uintptr) uintptr {
	handle, _, _ = procRegisterCtrlEx.Call(uintptr(unsafe.Pointer(&vacio[0])), syscall.NewCallback(controlador), 0)
	if handle == 0 {
		return 0
	}
	informar(startPending)
	l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", puerto))
	if err != nil {
		informar(stopped)
		return 0
	}
	informar(running)
	<-parar
	l.Close()
	informar(stopped)
	return 0
}

func main() {
	flag.IntVar(&puerto, "puerto", 47997, "el puerto que escucha (en 127.0.0.1)")
	flag.Parse()

	tabla := [2]serviceTableEntry{{name: &vacio[0], proc: syscall.NewCallback(principal)}, {}}
	r, _, e := procDispatcher.Call(uintptr(unsafe.Pointer(&tabla[0])))
	switch {
	case r != 0:
		return // fue un servicio, y ya se detuvo
	case e != errNoEsServicio:
		fmt.Println("ERROR", e)
		os.Exit(1)
	}

	// A mano: un programa cualquiera que escucha.
	l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", puerto))
	if err != nil {
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
	defer l.Close()
	fmt.Printf("PUERTO %d\n", l.Addr().(*net.TCPAddr).Port)
	for {
		time.Sleep(time.Hour)
	}
}
