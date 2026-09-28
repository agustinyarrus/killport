//go:build windows

package win

// imagepath.go: la ruta COMPLETA del ejecutable de cualquier proceso, sin
// abrirlo. OpenProcess + QueryFullProcessImageNameW se niega con acceso
// denegado en casi todos los servicios (medido: 50 de 51 sin admin), así que
// no sirve para clasificar "esto es de Windows / esto es de terceros".
// NtQuerySystemInformation con SystemProcessIdInformation (88) le pregunta al
// kernel la ruta de UN PID sin pedir ningún permiso sobre el proceso: 380 de
// 381 procesos en 4,8 ms en esta máquina (el que falta es el PID 0, que no es
// un proceso: STATUS_INVALID_CID).
//
// La ruta viene en formato NT (\Device\HarddiskVolume3\...); se traduce a la
// de siempre (C:\...) con la tabla de dispositivos de las unidades
// (QueryDosDeviceW), que se arma una vez por corrida.

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"
)

var (
	procQueryDosDeviceW  = modkernel32.NewProc("QueryDosDeviceW")
	procGetLogicalDrives = modkernel32.NewProc("GetLogicalDrives")
)

const (
	systemProcessIDInformation = 88         // SYSTEM_INFORMATION_CLASS
	statusInvalidCID           = 0xC000000B // el PID no existe
)

// systemProcessIDInfo es SYSTEM_PROCESS_ID_INFORMATION (24 bytes en x64): el
// PID y un UNICODE_STRING cuyo búfer pone QUIEN LLAMA (MaximumLength dice
// cuánto entra). Si no entra, el kernel devuelve STATUS_INFO_LENGTH_MISMATCH.
type systemProcessIDInfo struct {
	processID uintptr
	length    uint16
	maxLength uint16
	_         uint32
	buffer    *uint16
}

// ImagePath devuelve la ruta del ejecutable del proceso pid, en formato DOS
// ("C:\..."). "" sin error para los pseudo-procesos sin archivo (Registry,
// MemCompression). ErrGone si el PID no existe.
func ImagePath(pid uint32) (string, error) {
	for _, units := range [...]int{260, 32767} { // MAX_PATH; y el tope de UNICODE_STRING
		buf := make([]uint16, units)
		info := systemProcessIDInfo{processID: uintptr(pid), maxLength: uint16(units * 2), buffer: &buf[0]}
		r, _ := call(procNtQuerySystemInformation, systemProcessIDInformation,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
		runtime.KeepAlive(buf)
		switch uint32(r) {
		case statusSuccess:
			n := min(int(info.length)/2, len(buf))
			nt := string(utf16.Decode(buf[:n]))
			if !strings.Contains(nt, `\`) {
				// Pseudo-procesos del kernel (Registry, MemCompression, Secure
				// System): el "nombre" no es un archivo. Sin ruta, sin inventar.
				return "", nil
			}
			return dosPath(nt, deviceTable(), WindowsDirectory()), nil
		case statusInfoLengthMismatch:
			continue
		case statusInvalidCID:
			return "", ErrGone
		default:
			return "", fmt.Errorf("no pude leer la ruta del proceso %d: NTSTATUS 0x%x", pid, uint32(r))
		}
	}
	return "", fmt.Errorf("no pude leer la ruta del proceso %d: no entra en 32.767 caracteres", pid)
}

// deviceTable arma, una vez, la tabla dispositivo NT → letra de unidad.
// Una unidad que aparezca después (un pendrive a mitad de corrida) queda
// fuera: su ruta se muestra en formato NT, fiel aunque menos linda.
var deviceTable = sync.OnceValue(func() []devicePrefix {
	mask, _ := call(procGetLogicalDrives)
	var out []devicePrefix
	target := make([]uint16, 1024)
	for i := 0; i < 26; i++ {
		if mask&(1<<i) == 0 {
			continue
		}
		drive := string(rune('A'+i)) + ":"
		n, _ := call(procQueryDosDeviceW, uintptr(unsafe.Pointer(UTF16Ptr(drive))),
			uintptr(unsafe.Pointer(&target[0])), uintptr(len(target)))
		if n == 0 {
			continue
		}
		// La respuesta es una lista de cadenas terminadas en cero: la primera
		// es el dispositivo actual (las demás, mapeos anteriores de SUBST).
		dev := UTF16ToString(&target[0], int(n))
		if dev != "" {
			out = append(out, devicePrefix{device: dev, drive: drive})
		}
	}
	return out
})

// WindowsDirectory es la carpeta de Windows (el padre de System32).
func WindowsDirectory() string {
	if i := strings.LastIndexByte(systemDirectory, '\\'); i > 0 {
		return systemDirectory[:i]
	}
	return `C:\Windows`
}
