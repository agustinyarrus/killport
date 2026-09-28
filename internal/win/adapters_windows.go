//go:build windows

package win

import (
	"fmt"
	"unsafe"
)

var procGetAdaptersAddresses = modiphlpapi.NewProc("GetAdaptersAddresses")

const (
	afUnspec          = 0
	gaaSkipAnycast    = 0x0002
	gaaSkipMulticast  = 0x0004
	gaaSkipDNSServer  = 0x0008
	errBufferOverflow = 111 // ERROR_BUFFER_OVERFLOW: el búfer quedó chico
	errNoData         = 232 // ERROR_NO_DATA: no hay adaptadores
)

// Interfaces lee las interfaces de red con sus direcciones unicast, IPv4 e
// IPv6. No pide permisos. Bucle con ERROR_BUFFER_OVERFLOW: entre que el
// sistema informa el tamaño y que se lo vuelve a pedir puede aparecer una
// interfaz (un adaptador de WSL que se levanta), así que se reintenta con lo
// informado más holgura.
func Interfaces() ([]Interface, error) {
	size := uint32(16 << 10) // 16 KiB: lo que recomienda la documentación para no llamar dos veces
	for attempt := 0; attempt < 6; attempt++ {
		buf := alignedBuffer(int(size))
		r, _ := call(procGetAdaptersAddresses, afUnspec, gaaSkipAnycast|gaaSkipMulticast|gaaSkipDNSServer, 0,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
		switch r {
		case 0:
			return parseAdapters(buf, uintptr(unsafe.Pointer(&buf[0])))
		case errNoData:
			return nil, nil
		case errBufferOverflow:
			size += size / 4
		default:
			return nil, fmt.Errorf("no pude leer las interfaces de red: error %d", r)
		}
	}
	return nil, fmt.Errorf("no pude leer las interfaces de red: la lista no dejó de crecer")
}
