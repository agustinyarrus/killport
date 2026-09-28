package win

// procinfo.go interpreta el búfer de NtQuerySystemInformation con la clase
// SystemProcessInformation (5): TODOS los procesos del sistema en una sola
// llamada, incluidos los protegidos y los de otras sesiones, sin abrir
// ninguno (abrirlos uno por uno sería N llamadas y fallaría con los que no
// son nuestros). Sin build tag: se prueba con bytes sintéticos.
//
// El búfer es una lista enlazada por desplazamientos relativos:
//
//	entrada 0 ──NextEntryOffset──▶ entrada 1 ──▶ … ──▶ entrada k (NextEntryOffset = 0)
//
// y cada entrada es una SYSTEM_PROCESS_INFORMATION (0x100 bytes en 64 bits)
// seguida de sus hilos (SYSTEM_THREAD_INFORMATION, 0x50 cada uno) y del nombre
// de la imagen, al que apunta ImageName.Buffer DENTRO del mismo búfer.
//
// Invariantes que garantizan terminar sin leer fuera del búfer, aun si viene
// corrupto: cada salto es estrictamente positivo y la entrada de destino tiene
// que caber entera, así que el recorrido avanza siempre y hace a lo sumo
// len(b)/0x100 pasos. O(entradas).

import (
	"time"
)

// ProcessInfo es un proceso tal como figura en la foto del sistema.
type ProcessInfo struct {
	PID  uint32
	PPID uint32 // InheritedFromUniqueProcessId: quién lo creó. El padre puede haber muerto y su PID estar reusado: compararlo con Created antes de creerle.
	// Created es la hora de creación del proceso (la misma que GetProcessTimes,
	// al tick de 100 ns): junto con PID forma la identidad del proceso. Cero
	// para el proceso inactivo (PID 0) y System (PID 4) en algunas versiones.
	Created   time.Time
	SessionID uint32
	// Image es el nombre de la imagen sin ruta ("node.exe"). El proceso
	// inactivo (PID 0) no tiene nombre: queda "" (Task Manager y WMI lo
	// llaman "System Idle Process").
	Image      string
	Threads    uint32
	Handles    uint32
	WorkingSet uint64 // bytes de memoria física en uso
	Private    uint64 // bytes de memoria privada comprometida (PrivatePageCount)
}

// Desplazamientos de SYSTEM_PROCESS_INFORMATION en 64 bits (amd64 y arm64).
// Verificados en esta máquina contra Win32_Process (ver _oracle).
const (
	spiNextEntryOffset = 0x00 // ULONG
	spiNumberOfThreads = 0x04 // ULONG
	spiCreateTime      = 0x20 // LARGE_INTEGER (FILETIME)
	spiImageName       = 0x38 // UNICODE_STRING (16 bytes)
	spiUniqueProcessID = 0x50 // HANDLE
	spiInheritedFrom   = 0x58 // HANDLE
	spiHandleCount     = 0x60 // ULONG
	spiSessionID       = 0x64 // ULONG
	spiWorkingSetSize  = 0x90 // SIZE_T
	spiPrivatePages    = 0xC8 // SIZE_T PrivatePageCount (en bytes, pese al nombre)
	spiSize            = 0x100
)

// parseProcessInfo recorre la lista. base es la dirección de b[0] cuando el
// sistema llenó el búfer: los nombres se resuelven relativos a ella.
func parseProcessInfo(b []byte, base uintptr) ([]ProcessInfo, error) {
	var out []ProcessInfo
	for off := 0; ; {
		e, err := window(b, off, spiSize, "proceso")
		if err != nil {
			return nil, err
		}
		p, err := parseProcessEntry(b, base, off, e)
		if err != nil {
			return nil, err
		}
		out = append(out, p)

		next := int(le32(e, spiNextEntryOffset))
		if next == 0 {
			return out, nil
		}
		if next < spiSize {
			// Un salto menor que la entrada la solaparía consigo misma (0 ya
			// fue el fin de la lista): es basura, y además sin esta regla un
			// salto chico podría no avanzar nunca.
			return nil, malformed("proceso %d: NextEntryOffset %d menor que la entrada (%d)", p.PID, next, spiSize)
		}
		off += next // sin desborde: off < len(b) y next < 2³², en int de 64 bits
	}
}

func parseProcessEntry(b []byte, base uintptr, off int, e []byte) (ProcessInfo, error) {
	pid, ppid := le64(e, spiUniqueProcessID), le64(e, spiInheritedFrom)
	if pid > 0xFFFF_FFFF || ppid > 0xFFFF_FFFF {
		// Los PID son DWORD aunque viajen en un HANDLE: más de 32 bits es basura.
		return ProcessInfo{}, malformed("proceso en %d: PID 0x%x / padre 0x%x fuera de rango", off, pid, ppid)
	}
	name, err := unicodeString(b, base, off+spiImageName, "nombre del proceso")
	if err != nil {
		return ProcessInfo{}, err
	}
	return ProcessInfo{
		PID:        uint32(pid),
		PPID:       uint32(ppid),
		Created:    fileTime(int64(le64(e, spiCreateTime))),
		SessionID:  le32(e, spiSessionID),
		Image:      name,
		Threads:    le32(e, spiNumberOfThreads),
		Handles:    le32(e, spiHandleCount),
		WorkingSet: le64(e, spiWorkingSetSize),
		Private:    le64(e, spiPrivatePages),
	}, nil
}
