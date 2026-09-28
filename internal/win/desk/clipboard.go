//go:build windows

package desk

// clipboard.go lee y escribe texto del portapapeles de Windows por la API
// nativa (CF_UNICODETEXT), sin dependencias. Reintenta abrir el portapapeles
// porque otro proceso puede tenerlo tomado un instante.

import (
	"errors"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

var (
	procOpenClipboard    = moduser32.NewProc("OpenClipboard")
	procCloseClipboard   = moduser32.NewProc("CloseClipboard")
	procGetClipboardData = moduser32.NewProc("GetClipboardData")
	procSetClipboardData = moduser32.NewProc("SetClipboardData")
	procEmptyClipboard   = moduser32.NewProc("EmptyClipboard")
	procIsFormatAvail    = moduser32.NewProc("IsClipboardFormatAvailable")

	procGlobalAlloc   = modkernel32.NewProc("GlobalAlloc")
	procGlobalFree    = modkernel32.NewProc("GlobalFree")
	procGlobalLock    = modkernel32.NewProc("GlobalLock")
	procGlobalUnlock  = modkernel32.NewProc("GlobalUnlock")
	procLstrlenW      = modkernel32.NewProc("lstrlenW")
	procRtlMoveMemory = modkernel32.NewProc("RtlMoveMemory")
)

const (
	cfUnicodeText = 13
	gmemMoveable  = 0x0002
	maxClipRunes  = 1 << 24 // tope de seguridad al recorrer memoria del portapapeles
)

// ErrClipboardEmpty se devuelve cuando no hay texto en el portapapeles.
var ErrClipboardEmpty = errors.New("el portapapeles no tiene texto")

// openClipboard intenta abrir el portapapeles con unos reintentos cortos.
func openClipboard() error {
	var last error
	for attempt := 0; attempt < 10; attempt++ {
		if r, _ := call(procOpenClipboard, 0); r != 0 {
			return nil
		}
		last = syscall.GetLastError()
		time.Sleep(15 * time.Millisecond)
	}
	if last == nil {
		last = errors.New("no pude abrir el portapapeles")
	}
	return last
}

// ClipboardText devuelve el texto Unicode del portapapeles.
func ClipboardText() (string, error) {
	if r, _ := call(procIsFormatAvail, cfUnicodeText); r == 0 {
		return "", ErrClipboardEmpty
	}
	if err := openClipboard(); err != nil {
		return "", err
	}
	defer call(procCloseClipboard)

	h, _ := call(procGetClipboardData, cfUnicodeText)
	if h == 0 {
		return "", ErrClipboardEmpty
	}
	src, _ := call(procGlobalLock, h)
	if src == 0 {
		return "", errors.New("no pude leer el portapapeles")
	}
	defer call(procGlobalUnlock, h)

	// Se copia la memoria fija de Win32 a un búfer de Go con RtlMoveMemory, en
	// vez de reinterpretar el puntero nativo como puntero de Go: así el copiado
	// es explícito y no hay conversión uintptr→unsafe.Pointer que malinterpretar.
	n, _ := call(procLstrlenW, src)
	if n == 0 {
		return "", nil
	}
	if n > maxClipRunes {
		n = maxClipRunes
	}
	buf := make([]uint16, n)
	call(procRtlMoveMemory, uintptr(unsafe.Pointer(&buf[0])), src, uintptr(n)*2)
	return string(utf16.Decode(buf)), nil
}

// SetClipboardText escribe texto Unicode en el portapapeles.
func SetClipboardText(text string) error {
	if err := openClipboard(); err != nil {
		return err
	}
	defer call(procCloseClipboard)
	call(procEmptyClipboard)

	utf := utf16.Encode([]rune(text))
	utf = append(utf, 0)
	size := uintptr(len(utf) * 2)
	h, _ := call(procGlobalAlloc, gmemMoveable, size)
	if h == 0 {
		return errors.New("sin memoria para el portapapeles")
	}
	dst, _ := call(procGlobalLock, h)
	if dst == 0 {
		call(procGlobalFree, h)
		return errors.New("no pude preparar el portapapeles")
	}
	// Copia explícita del búfer de Go a la memoria global, sin reinterpretar el
	// puntero nativo como puntero de Go.
	call(procRtlMoveMemory, dst, uintptr(unsafe.Pointer(&utf[0])), size)
	call(procGlobalUnlock, h)

	if r, _ := call(procSetClipboardData, cfUnicodeText, h); r == 0 {
		call(procGlobalFree, h)
		return errors.New("no pude escribir el portapapeles")
	}
	return nil // tras SetClipboardData, el sistema es dueño del handle
}
