package win

// Tests de bytes sintéticos para el parseo de SYSTEM_PROCESS_INFORMATION:
// cadena de NextEntryOffset, nombres UTF-16, entrada con nombre vacío = System
// Idle, buffer corrupto → error sin pánico ni lectura fuera de rango.

import (
	"encoding/binary"
	"testing"
	"time"
)

// procEntry arma una entrada SYSTEM_PROCESS_INFORMATION seguida de su nombre.
// nameAt es el offset del nombre dentro del BÚFER GLOBAL (para el puntero
// relativo a base); si name=="" el puntero queda NULL y Length 0.
func procEntry(base uintptr, entryOffset int, next uint32, pid, ppid, session uint32, created time.Time, name string) []byte {
	nameBytes := len(name) * 2
	buf := make([]byte, spiSize+nameBytes)
	binary.LittleEndian.PutUint32(buf[spiNextEntryOffset:], next)
	binary.LittleEndian.PutUint32(buf[spiNumberOfThreads:], 7)
	binary.LittleEndian.PutUint64(buf[spiCreateTime:], uint64(toFileTime(created)))
	binary.LittleEndian.PutUint64(buf[spiUniqueProcessID:], uint64(pid))
	binary.LittleEndian.PutUint64(buf[spiInheritedFrom:], uint64(ppid))
	binary.LittleEndian.PutUint32(buf[spiHandleCount:], 33)
	binary.LittleEndian.PutUint32(buf[spiSessionID:], session)
	if name != "" {
		binary.LittleEndian.PutUint16(buf[spiImageName:], uint16(nameBytes))   // Length
		binary.LittleEndian.PutUint16(buf[spiImageName+2:], uint16(nameBytes)) // MaximumLength
		// Buffer apunta justo después de la estructura, en coordenadas de base.
		binary.LittleEndian.PutUint64(buf[spiImageName+8:], uint64(base)+uint64(entryOffset)+spiSize)
		for i, r := range name {
			binary.LittleEndian.PutUint16(buf[spiSize+2*i:], uint16(r))
		}
	}
	return buf
}

func TestParseProcessInfoCadena(t *testing.T) {
	base := uintptr(0x40000)
	when := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	// Tres procesos encadenados. El primero es System Idle (PID 0, sin nombre).
	e0 := procEntry(base, 0, 0, 0, 0, 0, time.Time{}, "")
	off1 := len(e0)
	e1 := procEntry(base, off1, 0, 1234, 1000, 1, when, "node.exe")
	off2 := off1 + len(e1)
	e2 := procEntry(base, off2, 0, 5678, 1234, 1, when, "chrome.exe")
	// Encadenar: NextEntryOffset de e0 → off1, de e1 → salto a e2, de e2 → 0.
	binary.LittleEndian.PutUint32(e0[spiNextEntryOffset:], uint32(off1))
	binary.LittleEndian.PutUint32(e1[spiNextEntryOffset:], uint32(off2-off1))

	b := append(append(append([]byte{}, e0...), e1...), e2...)
	procs, err := parseProcessInfo(b, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 3 {
		t.Fatalf("esperaba 3 procesos, hay %d", len(procs))
	}
	if procs[0].PID != 0 || procs[0].Image != "" {
		t.Errorf("proc0 debería ser System Idle (PID 0, sin nombre); es pid=%d img=%q", procs[0].PID, procs[0].Image)
	}
	if procs[1].PID != 1234 || procs[1].PPID != 1000 || procs[1].Image != "node.exe" || procs[1].SessionID != 1 {
		t.Errorf("proc1 = %+v", procs[1])
	}
	if !procs[1].Created.UTC().Equal(when) {
		t.Errorf("created = %v, esperaba %v", procs[1].Created.UTC(), when)
	}
	if procs[2].Image != "chrome.exe" || procs[2].PPID != 1234 {
		t.Errorf("proc2 = %+v", procs[2])
	}
	if procs[1].Threads != 7 || procs[1].Handles != 33 {
		t.Errorf("hilos/handles = %d/%d, esperaba 7/33", procs[1].Threads, procs[1].Handles)
	}
}

func TestParseProcessInfoSaltoInvalido(t *testing.T) {
	// NextEntryOffset menor que la entrada: se solaparía / no avanzaría. Error.
	base := uintptr(0x40000)
	e := procEntry(base, 0, 16, 10, 1, 0, time.Time{}, "x.exe") // 16 < spiSize
	if _, err := parseProcessInfo(e, base); !isMalformed(err) {
		t.Errorf("salto inválido: err = %v, esperaba ErrMalformed", err)
	}
}

func TestParseProcessInfoTruncado(t *testing.T) {
	// La última entrada dice tener un salto pero el búfer se corta: error, no
	// pánico ni lectura fuera de rango.
	base := uintptr(0x40000)
	e0 := procEntry(base, 0, uint32(spiSize), 1, 0, 0, time.Time{}, "a.exe")
	b := e0[:spiSize+2] // el nombre queda a medias y no hay segunda entrada
	if _, err := parseProcessInfo(b, base); err == nil {
		t.Error("un búfer truncado tras el salto tiene que dar error")
	}
}

func TestParseProcessInfoNombreFueraDeRango(t *testing.T) {
	base := uintptr(0x40000)
	e := procEntry(base, 0, 0, 10, 1, 0, time.Time{}, "x.exe")
	// Corromper el puntero del nombre para que apunte lejísimos.
	binary.LittleEndian.PutUint64(e[spiImageName+8:], uint64(base)+1<<20)
	if _, err := parseProcessInfo(e, base); !isMalformed(err) {
		t.Errorf("nombre fuera de rango: err = %v, esperaba ErrMalformed", err)
	}
}

func TestParseProcessInfoUnaSolaEntrada(t *testing.T) {
	base := uintptr(0x40000)
	e := procEntry(base, 0, 0, 4, 0, 0, time.Time{}, "System")
	procs, err := parseProcessInfo(e, base)
	if err != nil || len(procs) != 1 || procs[0].Image != "System" {
		t.Errorf("una sola entrada: procs=%v err=%v", procs, err)
	}
}
