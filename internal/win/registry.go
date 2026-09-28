//go:build windows

package win

// registry.go: leer los valores de texto de una clave del registro de la
// máquina (HKLM). killport lo usa para las reglas de `netsh interface
// portproxy`, que viven en el registro tal cual las carga el IP Helper: leer
// la clave es leer la fuente, sin pasar por la salida traducida de netsh.
//
// Solo lectura, y solo lo que cualquier usuario puede leer. Se pide la vista
// de 64 bits explícitamente (KEY_WOW64_64KEY): la respuesta no depende de cómo
// se haya compilado killport.

import (
	"fmt"
	"unsafe"
)

var (
	procRegOpenKeyExW    = modadvapi32.NewProc("RegOpenKeyExW")
	procRegQueryInfoKeyW = modadvapi32.NewProc("RegQueryInfoKeyW")
	procRegEnumValueW    = modadvapi32.NewProc("RegEnumValueW")
	procRegCloseKey      = modadvapi32.NewProc("RegCloseKey")
)

const (
	hkeyLocalMachine = 0x80000002

	keyQueryValue = 0x0001
	keyWow6464Key = 0x0100

	regSZ       = 1
	regExpandSZ = 2

	// errFileNotFound (2) ya está en pipe.go.
	errMoreData    = 234
	errNoMoreItems = 259

	// Topes del registro: un nombre de valor mide hasta 16.383 caracteres; a
	// los datos se les pone un tope propio (una regla de reenvío son unas
	// decenas de caracteres).
	regMaxValueName = 16383
	regMaxValueData = 64 * 1024
	regMaxValues    = 4096
)

// RegValue es un valor de texto de una clave.
type RegValue struct {
	Name string
	Data string
}

// MachineValues devuelve los valores de TEXTO (REG_SZ y REG_EXPAND_SZ) de la
// clave path de HKEY_LOCAL_MACHINE, en el orden en que el registro los
// enumera. exists=false, sin error, si la clave no existe. Los valores de otro
// tipo se saltean. Errores: ErrAccessDenied, o el de Windows.
func MachineValues(path string) (values []RegValue, exists bool, err error) {
	var key uintptr
	// Las funciones del registro devuelven el código de error en el valor de
	// retorno (LSTATUS), no por GetLastError.
	if st, _ := call(procRegOpenKeyExW, hkeyLocalMachine, uintptr(unsafe.Pointer(UTF16Ptr(path))), 0,
		keyQueryValue|keyWow6464Key, uintptr(unsafe.Pointer(&key))); st != 0 {
		switch st {
		case errFileNotFound:
			return nil, false, nil
		case 5:
			return nil, false, ErrAccessDenied
		}
		return nil, false, fmt.Errorf("no pude abrir HKLM\\%s: código %d", path, st)
	}
	defer call(procRegCloseKey, key)

	var maxName, maxData uint32
	if st, _ := call(procRegQueryInfoKeyW, key, 0, 0, 0, 0, 0, 0,
		0, uintptr(unsafe.Pointer(&maxName)), uintptr(unsafe.Pointer(&maxData)), 0, 0); st != 0 {
		return nil, true, fmt.Errorf("no pude leer HKLM\\%s: código %d", path, st)
	}
	if maxName > regMaxValueName || maxData > regMaxValueData {
		return nil, true, fmt.Errorf("HKLM\\%s tiene valores demasiado grandes (%d caracteres de nombre, %d bytes de datos)", path, maxName, maxData)
	}
	name := make([]uint16, maxName+2)
	data := alignedBuffer(int(maxData) + 4)

	// La clave puede cambiar mientras se la recorre: se enumera hasta que el
	// registro dice que no hay más (no hasta la cuenta de recién), con un tope.
	for i := uint32(0); i < regMaxValues; i++ {
		nameLen, dataLen := uint32(len(name)), uint32(len(data))
		var typ uint32
		st, _ := call(procRegEnumValueW, key, uintptr(i), uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&nameLen)), 0,
			uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&dataLen)))
		switch st {
		case 0:
		case errNoMoreItems:
			return values, true, nil
		case errMoreData:
			// Un valor que creció después de medir: la clave cambió mientras se
			// la leía. Saltearlo en silencio devolvería media lista como si fuera
			// entera (para portproxy, "no hay regla en ese puerto" cuando sí la
			// hay); se dice que no se pudo, y quien llama decide.
			return values, true, fmt.Errorf("HKLM\\%s cambió mientras la leía: volvé a intentar", path)
		default:
			return values, true, fmt.Errorf("no pude leer HKLM\\%s: código %d", path, st)
		}
		if typ != regSZ && typ != regExpandSZ {
			continue
		}
		if int(nameLen) > len(name) || int(dataLen) > len(data) {
			return values, true, malformed("valor del registro: largos %d y %d", nameLen, dataLen)
		}
		values = append(values, RegValue{Name: utf16Decode(name[:nameLen]), Data: regString(data[:dataLen])})
	}
	return values, true, nil
}

// regString decodifica los datos de un REG_SZ: UTF-16 little-endian, con o
// sin el cero final (el registro guarda lo que le escribieron).
func regString(b []byte) string {
	b = b[:len(b)&^1]
	for i := 0; i+1 < len(b); i += 2 {
		if b[i] == 0 && b[i+1] == 0 {
			b = b[:i]
			break
		}
	}
	return utf16le(b)
}
