//go:build windows

package killport

import (
	"encoding/json"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/agustinyarrus/killport/internal/ports"
)

// sioAcquirePortReservation es _WSAIOW(IOC_VENDOR, 100): le pide a Windows
// una reserva de puertos que dura lo que dura el socket. No pide
// administrador, y aparece en `netsh … show excludedportrange` como una fila
// sin "*", igual que las de WinNAT: es la tercera clase de exclusión.
const sioAcquirePortReservation = 0x98000064

// reservarPuertos le pide a Windows n puertos TCP seguidos, donde él elija, y
// los tiene reservados hasta el final de la prueba (cerrar el socket los
// suelta; si la prueba muere, muere el socket).
func reservarPuertos(t *testing.T, n uint16) ports.Range {
	t.Helper()
	var d syscall.WSAData
	if err := syscall.WSAStartup(0x0202, &d); err != nil {
		t.Fatal(err)
	}
	s, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Closesocket(s) })
	pedido := struct{ Inicio, Cantidad uint16 }{0, n} // inicio 0: donde elija Windows
	var reserva struct {
		Inicio, Cantidad uint16 // el inicio viene en el orden de la red
		_                [4]byte
		Token            uint64
	}
	var got uint32
	if err := syscall.WSAIoctl(s, sioAcquirePortReservation, (*byte)(unsafe.Pointer(&pedido)), uint32(unsafe.Sizeof(pedido)),
		(*byte)(unsafe.Pointer(&reserva)), uint32(unsafe.Sizeof(reserva)), &got, nil, 0); err != nil {
		t.Skipf("Windows no dio una reserva de puertos: %v", err)
	}
	lo := reserva.Inicio<<8 | reserva.Inicio>>8
	if reserva.Cantidad != n || lo == 0 {
		t.Fatalf("reserva = %+v", reserva)
	}
	return ports.R(lo, lo+n-1)
}

// La reserva de un programa, de verdad: una fila sin "*" que no es de WinNAT
// (7 puertos, no 100). killport la tiene que decir por lo que es, y no puede
// ofrecer ninguna salida de WinNAT: ni --reserve (que lo detiene y no la
// saca) ni reiniciarlo. Antes decía "el bloque que WinNAT reservó para
// Hyper-V, WSL o Docker" y proponía las dos.
func TestE2EReservaDeUnPrograma(t *testing.T) {
	dir := binariosE2E(t)
	r := reservarPuertos(t, 7)
	x, ok := exclusionesReales(t).Find(ports.TCP, ports.IPv4, r.Lo)
	if !ok || x.Range != r || x.Administered {
		t.Fatalf("la reserva %v no figura como fila sin \"*\" en la tabla de netsh: %+v", r, x)
	}
	p := r.Lo + 3
	P := strconv.Itoa(int(p))

	// 1. El diagnóstico: reservado, de un programa, sin salidas.
	out, code := killportE2E(t, dir, nil, P, "--tcp", "--json")
	rep := leerJSON[reporteE2E](t, out)
	if len(rep.Puertos) != 1 || len(rep.Objetivos) != 0 {
		t.Fatalf("killport %s --json: código %d\n%s", P, code, out)
	}
	pr := rep.Puertos[0]
	if pr.Estado != "reservado" || pr.Reservado == nil || pr.Reservado.Inicio != r.Lo || pr.Reservado.Fin != r.Hi ||
		pr.Reservado.Origen != "programa" || len(pr.Remedios) != 0 ||
		!strings.Contains(pr.Motivo, "no es de WinNAT") || strings.Contains(pr.Motivo+pr.Consejo, "--reserve") || strings.Contains(pr.Motivo+pr.Consejo, "net stop") {
		t.Fatalf("killport %s (reserva %v de un programa) = %+v\n%s", P, r, pr, out)
	}
	out, code = killportE2E(t, dir, nil, P, "--tcp")
	if code != 3 || !strings.Contains(out, "lo reserva Windows") || !strings.Contains(out, "la tiene reservada un programa") ||
		strings.Contains(out, "WinNAT reservó") || strings.Contains(out, "--reserve") {
		t.Fatalf("killport %s: código %d\n%s", P, code, out)
	}
	t.Logf("salida real de killport %s:\n%s", P, out)

	// 2. --reserve: impedimento (un plan que no se puede hacer sale con 3,
	// también con --list), y WinNAT ni se nombra como paso.
	out, code = killportE2E(t, dir, nil, P, "--reserve", "--json", "--list")
	var plan jsonConfig
	if err := json.Unmarshal([]byte(out), &plan); err != nil || code != 3 || !plan.SoloPlan {
		t.Fatalf("killport %s --reserve --json --list: código %d (%v)\n%s", P, code, err, out)
	}
	if len(plan.Pasos) != 0 || !strings.Contains(strings.Join(plan.Impedimentos, " "), "cae en la exclusión "+r.String()+", que no es de WinNAT") {
		t.Fatalf("el plan de reservar el %s (reserva de un programa) = %+v", P, plan)
	}
	out, _ = killportE2E(t, dir, nil, P, "--reserve", "--list")
	t.Logf("salida real de killport %s --reserve --list:\n%s", P, out)

	// 3. --free desde el principio de la reserva: la saltea y dice por qué.
	out, code = killportE2E(t, dir, nil, "--free", strconv.Itoa(int(r.Lo)), "--json")
	var libre jsonLibre
	if err := json.Unmarshal([]byte(out), &libre); err != nil || len(libre.Salteados) == 0 {
		t.Fatalf("killport --free %d --json: código %d (%v)\n%s", r.Lo, code, err, out)
	}
	if s := libre.Salteados[0]; s.Inicio != r.Lo || s.Motivo != "programa" || s.Rango == nil || s.Rango.Origen != "programa" {
		t.Fatalf("--free saltea la reserva así: %+v\n%s", s, out)
	}
}
