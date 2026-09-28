//go:build windows

package win

// Tests que dependen de tipos definidos en archivos //go:build windows: el
// tamaño de SHELLEXECUTEINFOW (que el sistema valida por cbSize) y el parseo
// del búfer de servicios con cadenas resueltas por puntero.

import (
	"encoding/binary"
	"testing"
	"unsafe"
)

func TestShellExecuteInfoSize(t *testing.T) {
	// En amd64, SHELLEXECUTEINFOW mide 112 bytes. Si el layout se desalinea, el
	// sistema rechazaría cbSize y RunElevated fallaría en silencio.
	if got := unsafe.Sizeof(shellExecuteInfo{}); got != 112 {
		t.Errorf("sizeof(SHELLEXECUTEINFOW) = %d, esperaba 112 en x64", got)
	}
}

func TestParseServices(t *testing.T) {
	// Dos entradas ENUM_SERVICE_STATUS_PROCESSW seguidas, y sus nombres
	// (cadenas UTF-16 terminadas en cero) al final del mismo búfer.
	base := uintptr(0x8000)
	const n = 2
	names := []string{"W32Time", "wuauserv"}
	disps := []string{"Hora de Windows", "Windows Update"}
	pids := []uint32{0, 4321} // el primero detenido (sin proceso)
	types := []uint32{serviceTypeShared, 0x10}
	states := []uint32{serviceStopped, serviceRunning}

	// Zona de cadenas después de las n entradas.
	stringsOff := n * essEntrySize
	var blob []byte
	putStr := func(s string) uint64 {
		ptr := uint64(base) + uint64(stringsOff) + uint64(len(blob))
		for _, r := range s {
			blob = binary.LittleEndian.AppendUint16(blob, uint16(r))
		}
		blob = append(blob, 0, 0) // terminador
		return ptr
	}
	entries := make([]byte, n*essEntrySize)
	for i := 0; i < n; i++ {
		e := entries[i*essEntrySize:]
		binary.LittleEndian.PutUint64(e[essServiceName:], putStr(names[i]))
		binary.LittleEndian.PutUint64(e[essDisplayName:], putStr(disps[i]))
		binary.LittleEndian.PutUint32(e[essServiceType:], types[i])
		binary.LittleEndian.PutUint32(e[essCurrentState:], states[i])
		binary.LittleEndian.PutUint32(e[essPID:], pids[i])
	}
	b := append(entries, blob...)

	svcs, err := parseServices(b, base, n)
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 2 {
		t.Fatalf("esperaba 2 servicios, hay %d", len(svcs))
	}
	if svcs[0].Name != "W32Time" || svcs[0].DisplayName != "Hora de Windows" || !svcs[0].Shared || svcs[0].State != serviceStopped {
		t.Errorf("svc0 = %+v", svcs[0])
	}
	if svcs[1].Name != "wuauserv" || svcs[1].PID != 4321 || svcs[1].Shared || svcs[1].State != serviceRunning {
		t.Errorf("svc1 = %+v", svcs[1])
	}
}

func TestParseOwnerModule(t *testing.T) {
	// TCPIP_OWNER_MODULE_BASIC_INFO: dos punteros a cadenas en el mismo búfer.
	base := uintptr(0x9000)
	name := "Dhcp"
	path := `C:\Windows\system32\svchost.exe`
	header := make([]byte, ownerModuleBasicSize)
	var blob []byte
	put := func(s string) uint64 {
		ptr := uint64(base) + uint64(ownerModuleBasicSize) + uint64(len(blob))
		for _, r := range s {
			blob = binary.LittleEndian.AppendUint16(blob, uint16(r))
		}
		blob = append(blob, 0, 0)
		return ptr
	}
	binary.LittleEndian.PutUint64(header[0:], put(name))
	binary.LittleEndian.PutUint64(header[8:], put(path))
	b := append(header, blob...)
	m, err := parseOwnerModule(b, base)
	if err != nil || m.Name != name || m.Path != path {
		t.Errorf("parseOwnerModule = %+v, %v; esperaba name=%q path=%q", m, err, name, path)
	}
}
