//go:build windows

package win

// safefile.go: el archivo por el que el proceso ELEVADO de killport le
// devuelve su resultado al proceso común que lo lanzó.
//
// La regla: un proceso con token de administrador que escribe en una ruta que
// eligió un proceso sin privilegios tiene que escribir exactamente ahí y en
// ningún otro lado. Entre que el padre arma la ruta y el hijo la abre, la
// carpeta pudo cambiar (lo que hay en una carpeta del usuario lo puede tocar
// cualquier programa del usuario). Por eso el archivo se crea:
//
//   - NUEVO (CREATE_NEW): nunca se abre ni se trunca algo que ya exista;
//   - sin seguir un punto de reanálisis en el último tramo
//     (FILE_FLAG_OPEN_REPARSE_POINT): si ahí hay un enlace, CREATE_NEW choca
//     con él en vez de crear su destino;
//   - y antes de escribir un byte se pide la ruta FINAL del archivo abierto
//     (GetFinalPathNameByHandleW, que resuelve cualquier desvío en las
//     carpetas intermedias): si no es la pedida, se lo borra por el mismo
//     handle (FileDispositionInfo) y se devuelve error. Como el archivo era
//     nuevo, borrarlo no deja rastro.

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

var (
	procCreateFileW                        = modkernel32.NewProc("CreateFileW")
	procGetFinalPathNameByHandleW          = modkernel32.NewProc("GetFinalPathNameByHandleW")
	procSetFileInformationByHandle         = modkernel32.NewProc("SetFileInformationByHandle")
	errSafeFileRedirected                  = errors.New("la ruta del resultado no lleva a donde decía; no escribo ahí")
	invalidHandleValue                     = ^uintptr(0)
	fileDispositionInfoClass       uintptr = 4 // FILE_INFO_BY_HANDLE_CLASS FileDispositionInfo
)

const (
	genericWrite             = 0x40000000
	accessDelete             = 0x00010000
	fileShareAll             = 0x7 // READ | WRITE | DELETE
	createNew                = 1
	openExisting             = 3
	fileAttributeNormal      = 0x80
	fileFlagOpenReparsePoint = 0x00200000
	fileFlagBackupSemantics  = 0x02000000 // necesario para abrir una CARPETA
)

// CreateNewExact crea path como archivo nuevo y lo devuelve abierto para
// escribir, garantizando que el archivo creado está exactamente en path (ver
// el comentario del archivo). path tiene que venir en forma canónica: la que
// devuelve FinalPath de su carpeta más el nombre.
func CreateNewExact(path string) (*os.File, error) {
	h, e := call(procCreateFileW, uintptr(unsafe.Pointer(UTF16Ptr(path))), genericWrite|accessDelete, 0, 0,
		createNew, fileAttributeNormal|fileFlagOpenReparsePoint, 0)
	if h == invalidHandleValue {
		return nil, fmt.Errorf("no pude crear %s: %w", path, lastError(e))
	}
	final, err := finalPathOfHandle(h)
	if err != nil || !strings.EqualFold(final, path) {
		// Borrar por el mismo handle: FILE_DISPOSITION_INFO{DeleteFile: TRUE}.
		del := uint32(1)
		call(procSetFileInformationByHandle, h, fileDispositionInfoClass, uintptr(unsafe.Pointer(&del)), 4)
		closeHandle(h)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w (%s ≠ %s)", errSafeFileRedirected, final, path)
	}
	return os.NewFile(h, path), nil
}

// FinalPath devuelve la ruta canónica de un archivo o carpeta existente: sin
// nombres cortos 8.3 (C:\Users\MARIAG~1 → C:\Users\maria.gomez), con los enlaces
// resueltos y en formato DOS.
func FinalPath(path string) (string, error) {
	h, e := call(procCreateFileW, uintptr(unsafe.Pointer(UTF16Ptr(path))), 0, fileShareAll, 0,
		openExisting, fileFlagBackupSemantics, 0)
	if h == invalidHandleValue {
		return "", fmt.Errorf("no pude abrir %s: %w", path, lastError(e))
	}
	defer closeHandle(h)
	return finalPathOfHandle(h)
}

// finalPathOfHandle pide la ruta final (FILE_NAME_NORMALIZED, VOLUME_NAME_DOS)
// y le quita el prefijo de ruta larga: "\\?\C:\x" → "C:\x",
// "\\?\UNC\srv\share" → "\\srv\share".
func finalPathOfHandle(h uintptr) (string, error) {
	buf := make([]uint16, 1024)
	for {
		n, e := call(procGetFinalPathNameByHandleW, h, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
		if n == 0 {
			return "", fmt.Errorf("no pude leer la ruta final: %w", lastError(e))
		}
		if int(n) < len(buf) {
			return TrimLongPrefix(syscall.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n+1) // n es el tamaño necesario con el cero
	}
}
