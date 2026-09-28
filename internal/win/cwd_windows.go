//go:build windows

package win

// cwd_windows.go: las lecturas de verdad de la carpeta de trabajo de otro
// proceso (ver peb.go para la cadena y por qué).

import (
	"fmt"
	"unsafe"
)

var procReadProcessMemory = modkernel32.NewProc("ReadProcessMemory")

const (
	processVMRead           = 0x0010 // PROCESS_VM_READ
	processBasicInformation = 0      // PROCESSINFOCLASS: PROCESS_BASIC_INFORMATION (PebBaseAddress en +8)
	processWow64Information = 26     // PROCESSINFOCLASS: la dirección del PEB de 32 bits, o 0
)

// processCwd lee la carpeta actual del proceso del handle, que tiene que
// estar abierto con PROCESS_VM_READ además de QUERY_LIMITED. La devuelve como
// la guarda Windows (con la barra final).
func processCwd(h uintptr) (string, error) {
	read := func(addr uint64, n int) ([]byte, error) {
		buf := make([]byte, n)
		var got uintptr
		r, e := call(procReadProcessMemory, h, uintptr(addr), uintptr(unsafe.Pointer(&buf[0])), uintptr(n), uintptr(unsafe.Pointer(&got)))
		if r == 0 {
			return nil, lastError(e)
		}
		if int(got) != n {
			return nil, malformed("ReadProcessMemory leyó %d de %d bytes", got, n)
		}
		return buf, nil
	}
	// Un proceso WOW64 usa (y actualiza) su PEB de 32 bits: ese manda.
	var peb32addr uintptr
	var ret uint32
	r, _ := call(procNtQueryInformationProcess, h, processWow64Information,
		uintptr(unsafe.Pointer(&peb32addr)), unsafe.Sizeof(peb32addr), uintptr(unsafe.Pointer(&ret)))
	if uint32(r) == statusSuccess && peb32addr != 0 {
		return readCwd(read, uint64(peb32addr), peb32)
	}
	// PROCESS_BASIC_INFORMATION: ExitStatus, PebBaseAddress, AffinityMask,
	// BasePriority, UniqueProcessId, InheritedFromUniqueProcessId (6 × 8).
	var pbi [6]uintptr
	r, _ = call(procNtQueryInformationProcess, h, processBasicInformation,
		uintptr(unsafe.Pointer(&pbi[0])), unsafe.Sizeof(pbi), uintptr(unsafe.Pointer(&ret)))
	if uint32(r) != statusSuccess {
		return "", fmt.Errorf("no pude leer el PEB: NTSTATUS 0x%x", uint32(r))
	}
	return readCwd(read, uint64(pbi[1]), peb64)
}
