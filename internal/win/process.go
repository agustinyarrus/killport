//go:build windows

package win

// process.go: la foto de todos los procesos (NtQuerySystemInformation), el
// detalle best-effort de uno solo, y el HANDLE con identidad que el motor
// sostiene desde que identifica una víctima hasta que la ve morir. El layout
// del búfer se interpreta en procinfo.go, sin build tag.

import (
	"fmt"
	"time"
	"unsafe"
)

var (
	procNtQuerySystemInformation  = modntdll.NewProc("NtQuerySystemInformation")
	procNtQueryInformationProcess = modntdll.NewProc("NtQueryInformationProcess")
	procOpenProcess               = modkernel32.NewProc("OpenProcess")
	procGetProcessTimes           = modkernel32.NewProc("GetProcessTimes")
	procGetExitCodeProcess        = modkernel32.NewProc("GetExitCodeProcess")
	procTerminateProcess          = modkernel32.NewProc("TerminateProcess")
	procQueryFullProcessImageName = modkernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle               = modkernel32.NewProc("CloseHandle")
	procGetCurrentProcessId       = modkernel32.NewProc("GetCurrentProcessId")
	procLookupAccountSidW         = modadvapi32.NewProc("LookupAccountSidW")
	// procOpenProcessToken y procGetTokenInformation ya están en elevate.go.
)

const tokenUser = 1 // TOKEN_INFORMATION_CLASS TokenUser

// CurrentProcessID devuelve el PID de este proceso.
func CurrentProcessID() uint32 {
	r, _ := call(procGetCurrentProcessId)
	return uint32(r)
}

const (
	systemProcessInformation = 5          // SYSTEM_INFORMATION_CLASS
	statusInfoLengthMismatch = 0xC0000004 // el búfer quedó chico: reintentar más grande
	statusSuccess            = 0

	processQueryLimitedInfo = 0x1000 // PROCESS_QUERY_LIMITED_INFORMATION
	processTerminate        = 0x0001 // PROCESS_TERMINATE
	synchronize             = 0x00100000

	processCommandLineInformation = 60 // PROCESSINFOCLASS
	processBreakOnTermination     = 29 // proceso "crítico": tumbarlo cuelga el sistema
	processProtectionInformation  = 61 // PS_PROTECTION: 0 = no protegido

	stillActive = 259 // STILL_ACTIVE: GetExitCodeProcess mientras el proceso vive
)

// AllProcesses devuelve la foto de todos los procesos del sistema en una sola
// llamada. Bucle con STATUS_INFO_LENGTH_MISMATCH: entre que el sistema informa
// el tamaño y que se lo vuelve a pedir pueden nacer procesos, así que se pide
// lo informado más holgura y se reintenta.
func AllProcesses() ([]ProcessInfo, error) {
	size := uint32(1 << 18) // 256 KiB: alcanza para ~600 procesos sin reintentar
	for attempt := 0; attempt < 8; attempt++ {
		buf := alignedBuffer(int(size))
		var needed uint32
		r, _ := call(procNtQuerySystemInformation, systemProcessInformation,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&needed)))
		switch uint32(r) {
		case statusSuccess:
			return parseProcessInfo(buf, uintptr(unsafe.Pointer(&buf[0])))
		case statusInfoLengthMismatch:
			if needed > size {
				size = needed + size/4
			} else {
				size += size / 2
			}
		default:
			return nil, fmt.Errorf("no pude leer la lista de procesos: NTSTATUS 0x%x", uint32(r))
		}
	}
	return nil, fmt.Errorf("no pude leer la lista de procesos: la tabla no dejó de crecer")
}

// ProcessDetail junta lo que se puede averiguar de un proceso sin ser su
// dueño: los campos vacíos significan "no se pudo", no "no tiene".
type ProcessDetail struct {
	Path        string // ruta completa del ejecutable ("" si no hay permiso)
	CommandLine string // línea de comandos ("" si no hay permiso o no aplica)
	User        string // dueño del token, "DOMINIO\usuario" ("" si no se pudo)
	Critical    bool   // proceso crítico (tumbarlo cuelga Windows)
	CriticalOK  bool   // si se pudo LEER el flag Critical (si no, Critical es dudoso)
	// Cwd es la carpeta de trabajo ACTUAL del proceso, sin la barra final ("" si
	// no se pudo: hace falta leer su memoria, y eso se concede para los
	// procesos del mismo usuario, no para los de otro ni los protegidos). Es
	// una pista, no una verdad: si el proceso hizo chdir, es la nueva.
	Cwd string
}

// QueryDetail abre el proceso con permiso mínimo (LIMITED) y averigua ruta,
// línea de comandos, usuario, si es crítico y su carpeta de trabajo, todo
// best-effort. Si wantCreated no es cero, antes de leer nada confirma que el
// proceso abierto sea ESE (misma hora de creación): si el PID se recicló
// devuelve ErrRecycled, para no mostrar la línea de comandos de otro.
// Errores: ErrGone, ErrRecycled, ErrAccessDenied (no se pudo abrir ni con
// LIMITED); los permisos que falten para un dato puntual se reflejan en
// campos vacíos. Es QueryDetailWith leyendo siempre la carpeta.
func QueryDetail(pid uint32, wantCreated time.Time) (ProcessDetail, error) {
	return QueryDetailWith(pid, wantCreated, func(string, bool) bool { return true })
}

// QueryDetailWith es QueryDetail decidiendo, con lo que ya se leyó (la ruta
// de la imagen y si es crítico), si vale la pena leer la carpeta de trabajo.
// Leerla pide PROCESS_VM_READ y leer memoria del proceso (su PEB): sobre
// lsass, wininit o services esa apertura (0x1010) es la firma del robo de
// credenciales que vigilan Defender (ASR), Sysmon (evento 10) y los EDR. Una
// herramienta de puertos no tiene nada que buscar ahí: la carpeta solo sirve
// para decir de qué proyecto es un proceso del usuario. Por eso primero se
// abre con LIMITED y, solo si conCarpeta dice que sí, se abre aparte con
// VM_READ (confirmando la identidad otra vez).
func QueryDetailWith(pid uint32, wantCreated time.Time, conCarpeta func(path string, critical bool) bool) (ProcessDetail, error) {
	h, err := openProcess(processQueryLimitedInfo, pid)
	if err != nil {
		return ProcessDetail{}, err
	}
	defer closeHandle(h)
	created := handleCreated(h)
	if !wantCreated.IsZero() && !sameInstant(created, wantCreated) {
		return ProcessDetail{}, ErrRecycled
	}

	var d ProcessDetail
	d.Path = queryFullImageName(h)
	d.CommandLine = queryCommandLine(h)
	d.User = queryTokenUser(h)
	if crit, ok := queryCritical(h); ok {
		d.Critical, d.CriticalOK = crit, true
	}
	if conCarpeta != nil && conCarpeta(d.Path, d.Critical) {
		if vm, err := openProcess(processQueryLimitedInfo|processVMRead, pid); err == nil {
			// El mismo proceso: mientras h está abierto el PID no se recicla,
			// pero se confirma igual (cinturón y tiradores).
			if sameInstant(handleCreated(vm), created) {
				if cwd, err := processCwd(vm); err == nil {
					d.Cwd = CleanDir(cwd)
				}
			}
			closeHandle(vm)
		}
	}
	return d, nil
}

// queryTokenUser devuelve el dueño del proceso como "DOMINIO\usuario", o ""
// si no se pudo (sin permiso para abrir el token, o la cuenta no se resuelve).
// Abre el token con TOKEN_QUERY, saca el SID del usuario (TokenUser) y lo
// traduce con LookupAccountSidW.
func queryTokenUser(h uintptr) string {
	var token uintptr
	if r, _ := call(procOpenProcessToken, h, tokenQuery, uintptr(unsafe.Pointer(&token))); r == 0 {
		return ""
	}
	defer closeHandle(token)

	// GetTokenInformation(TokenUser): primero el tamaño, después el búfer.
	var needed uint32
	call(procGetTokenInformation, token, tokenUser, 0, 0, uintptr(unsafe.Pointer(&needed)))
	if needed == 0 {
		return ""
	}
	buf := alignedBuffer(int(needed))
	if r, _ := call(procGetTokenInformation, token, tokenUser,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&needed))); r == 0 {
		return ""
	}
	// TOKEN_USER = SID_AND_ATTRIBUTES{ PSID Sid; DWORD Attributes }: el primer
	// puntero es el SID (dentro del mismo búfer).
	sid := le64(buf, 0)
	if sid == 0 {
		return ""
	}
	name := make([]uint16, 256)
	domain := make([]uint16, 256)
	nameLen, domainLen := uint32(len(name)), uint32(len(domain))
	var use uint32
	r, _ := call(procLookupAccountSidW, 0, uintptr(sid),
		uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&nameLen)),
		uintptr(unsafe.Pointer(&domain[0])), uintptr(unsafe.Pointer(&domainLen)),
		uintptr(unsafe.Pointer(&use)))
	if r == 0 {
		return ""
	}
	d, n := utf16Decode(domain[:domainLen]), utf16Decode(name[:nameLen])
	if d == "" {
		return n
	}
	return d + `\` + n
}

// queryFullImageName devuelve la ruta del ejecutable (formato Win32, no NT).
func queryFullImageName(h uintptr) string {
	buf := make([]uint16, 32768) // tope de una ruta larga en Windows (\\?\ …)
	size := uint32(len(buf))
	if r, _ := call(procQueryFullProcessImageName, h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
		return ""
	}
	return string(utf16Decode(buf[:size]))
}

// queryCommandLine lee ProcessCommandLineInformation: NtQueryInformationProcess
// devuelve un UNICODE_STRING seguido de sus caracteres en el mismo búfer.
// Alcanza con PROCESS_QUERY_LIMITED_INFORMATION (Windows 8.1+).
func queryCommandLine(h uintptr) string {
	var needed uint32
	// Primera llamada: pedir el tamaño (STATUS_INFO_LENGTH_MISMATCH con needed).
	r, _ := call(procNtQueryInformationProcess, h, processCommandLineInformation, 0, 0, uintptr(unsafe.Pointer(&needed)))
	if uint32(r) != statusInfoLengthMismatch || needed < unicodeStringSize {
		return ""
	}
	buf := alignedBuffer(int(needed))
	r, _ = call(procNtQueryInformationProcess, h, processCommandLineInformation,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&needed)))
	if uint32(r) != statusSuccess {
		return ""
	}
	s, err := unicodeString(buf, uintptr(unsafe.Pointer(&buf[0])), 0, "línea de comandos")
	if err != nil {
		return ""
	}
	return s
}

// queryCritical lee ProcessBreakOnTermination (bool de 4 bytes). ok=false si
// no se pudo (falta permiso): entonces no se sabe si es crítico.
func queryCritical(h uintptr) (critical, ok bool) {
	var flag uint32
	var ret uint32
	r, _ := call(procNtQueryInformationProcess, h, processBreakOnTermination,
		uintptr(unsafe.Pointer(&flag)), 4, uintptr(unsafe.Pointer(&ret)))
	if uint32(r) != statusSuccess {
		return false, false
	}
	return flag != 0, true
}

// Errores distinguibles al abrir un handle con identidad.
var (
	// ErrGone: el proceso ya no existe (nada que cerrar).
	ErrGone = fmt.Errorf("el proceso ya no existe")
	// ErrRecycled: el PID existe pero su hora de creación no coincide con la
	// esperada; es OTRO proceso que reusó el número. No se lo debe tocar.
	ErrRecycled = fmt.Errorf("el PID lo reusó otro proceso")
	// ErrAccessDenied: el proceso existe pero no se pudo abrir con los
	// permisos pedidos (hace falta elevación).
	ErrAccessDenied = fmt.Errorf("acceso denegado")
)

// Handle es un handle abierto a un proceso, atado a su identidad. Mientras
// vive, Windows no puede reciclar el PID (el objeto proceso sigue existiendo
// aunque el proceso muera), así que todo lo que el motor haga con este handle
// se refiere sin ambigüedad al MISMO proceso que se identificó.
type Handle struct {
	PID     uint32
	Created time.Time
	// CanTerminate es false si se pudo abrir para esperar pero no para
	// terminar (faltó PROCESS_TERMINATE): sirve para saber si hará falta
	// elevación antes de intentar matarlo.
	CanTerminate bool
	h            uintptr
}

// OpenForKill abre pid con SYNCHRONIZE|TERMINATE|QUERY_LIMITED y verifica que
// su hora de creación coincida con wantCreated (la identidad esperada). Si
// TERMINATE se niega pero el resto no, reabre sin él (CanTerminate=false) para
// al menos poder esperarlo. Errores: ErrGone, ErrRecycled, ErrAccessDenied.
//
// wantCreated cero significa "no verificar la identidad" (para procesos como
// System, cuya hora el sistema no informa).
func OpenForKill(pid uint32, wantCreated time.Time) (*Handle, error) {
	h, err := openProcess(synchronize|processTerminate|processQueryLimitedInfo, pid)
	canTerminate := true
	if err == ErrAccessDenied {
		// Reintentar sin TERMINATE: quizás alcanza para esperar e identificar.
		h, err = openProcess(synchronize|processQueryLimitedInfo, pid)
		canTerminate = false
	}
	if err != nil {
		return nil, err
	}
	got := handleCreated(h)
	if !wantCreated.IsZero() && !sameInstant(got, wantCreated) {
		closeHandle(h)
		return nil, ErrRecycled
	}
	return &Handle{PID: pid, Created: got, CanTerminate: canTerminate, h: h}, nil
}

// OpenForWait abre pid solo para ESPERAR su salida (SYNCHRONIZE y
// QUERY_LIMITED: nada que permita tocarlo) y verifica su identidad como
// OpenForKill. Es lo que hace falta para dormir hasta que un proceso termine
// sin pedir más permiso del necesario: alcanza aun con procesos de otros
// usuarios que no se dejan terminar. CanTerminate queda en false.
func OpenForWait(pid uint32, wantCreated time.Time) (*Handle, error) {
	h, err := openProcess(synchronize|processQueryLimitedInfo, pid)
	if err != nil {
		return nil, err
	}
	got := handleCreated(h)
	if !wantCreated.IsZero() && !sameInstant(got, wantCreated) {
		closeHandle(h)
		return nil, ErrRecycled
	}
	return &Handle{PID: pid, Created: got, h: h}, nil
}

// openProcess mapea los errores de OpenProcess a los distinguibles del motor.
func openProcess(access uintptr, pid uint32) (uintptr, error) {
	h, e := call(procOpenProcess, access, 0, uintptr(pid))
	if h != 0 {
		return h, nil
	}
	switch e {
	case 5: // ERROR_ACCESS_DENIED
		return 0, ErrAccessDenied
	case 87: // ERROR_INVALID_PARAMETER: no hay proceso con ese PID
		return 0, ErrGone
	default:
		return 0, fmt.Errorf("no pude abrir el proceso %d: %w", pid, lastError(e))
	}
}

// handleCreated devuelve la hora de creación del proceso del handle (el primer
// FILETIME de GetProcessTimes, el mismo instante que SYSTEM_PROCESS_INFORMATION).
func handleCreated(h uintptr) time.Time {
	var creation, exit, kernel, user filetime
	if r, _ := call(procGetProcessTimes, h, uintptr(unsafe.Pointer(&creation)),
		uintptr(unsafe.Pointer(&exit)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); r == 0 {
		return time.Time{}
	}
	return fileTime(int64(creation.HighDateTime)<<32 | int64(creation.LowDateTime))
}

// sameInstant compara dos horas de creación con tolerancia de medio tick de
// FILETIME (redondeos entre GetProcessTimes y el búfer del sistema). En la
// práctica coinciden exactas; la tolerancia es un cinturón de seguridad que NO
// deja pasar el segundo en que un PID se recicla.
func sameInstant(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d < 100*time.Nanosecond
}

// Terminate mata el proceso con el código de salida dado. Devuelve ErrGone si
// ya había terminado (el kernel devuelve ERROR_ACCESS_DENIED sobre un proceso
// que ya salió: se distingue mirando si sigue vivo).
func (p *Handle) Terminate(code uint32) error {
	if !p.CanTerminate {
		return ErrAccessDenied
	}
	r, e := call(procTerminateProcess, p.h, uintptr(code))
	if r != 0 {
		return nil
	}
	if !p.Alive() {
		return ErrGone
	}
	if e == 5 {
		return ErrAccessDenied
	}
	return fmt.Errorf("no pude terminar el proceso %d: %w", p.PID, lastError(e))
}

// Alive dice si el proceso sigue vivo (GetExitCodeProcess == STILL_ACTIVE).
// Cuidado: un proceso que salió justo con el código 259 se vería vivo; para el
// veredicto final el motor usa la espera por handle, no esto.
func (p *Handle) Alive() bool {
	code, ok := p.ExitCode()
	return ok && code == stillActive
}

// ExitCode devuelve el código de salida del proceso (ok=false si no se pudo
// leer). Mientras vive es STILL_ACTIVE (259).
func (p *Handle) ExitCode() (uint32, bool) {
	var code uint32
	if r, _ := call(procGetExitCodeProcess, p.h, uintptr(unsafe.Pointer(&code))); r == 0 {
		return 0, false
	}
	return code, true
}

// ExitTime devuelve cuándo terminó el proceso (el segundo FILETIME de
// GetProcessTimes, que el kernel fija al salir). ok=false si todavía corre o
// no se pudo leer. Alcanza con QUERY_LIMITED. Sirve para decir cuánto tardó
// en cerrar de verdad: quien espera se entera un rato después.
func (p *Handle) ExitTime() (time.Time, bool) {
	var creation, exit, kernel, user filetime
	if r, _ := call(procGetProcessTimes, p.h, uintptr(unsafe.Pointer(&creation)),
		uintptr(unsafe.Pointer(&exit)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user))); r == 0 {
		return time.Time{}, false
	}
	ft := int64(exit.HighDateTime)<<32 | int64(exit.LowDateTime)
	if ft == 0 {
		return time.Time{}, false // sigue corriendo: el kernel no fijó la salida
	}
	return fileTime(ft), true
}

// Protected dice si el proceso es protegido (PP o PPL: antimalware, lsass con
// RunAsPPL, servicios del sistema): ni un administrador puede terminarlo, y
// ofrecer elevación sería prometer algo imposible. Lee el byte PS_PROTECTION
// (ProcessProtectionInformation, 61), que alcanza con QUERY_LIMITED — el
// permiso que justamente los protegidos sí conceden. false si no se pudo leer.
func (p *Handle) Protected() bool {
	var level uint8
	var ret uint32
	r, _ := call(procNtQueryInformationProcess, p.h, processProtectionInformation,
		uintptr(unsafe.Pointer(&level)), 1, uintptr(unsafe.Pointer(&ret)))
	return uint32(r) == statusSuccess && level != 0
}

// Critical dice si el proceso está MARCADO como crítico
// (ProcessBreakOnTermination): si termina, por lo que sea, Windows tira un
// bugcheck. Lo llevan csrss, wininit, services, lsass y algunos svchost, y
// cualquier programa con SeDebugPrivilege se lo puede poner a sí mismo
// (RtlSetProcessIsCritical). known=false si no se pudo leer.
func (p *Handle) Critical() (critical, known bool) { return queryCritical(p.h) }

// Raw expone el handle crudo para WaitForMultipleObjects (paquete interno).
func (p *Handle) Raw() uintptr { return p.h }

// Close libera el handle. A partir de acá el PID puede reciclarse.
func (p *Handle) Close() {
	if p.h != 0 {
		closeHandle(p.h)
		p.h = 0
	}
}

func closeHandle(h uintptr) { call(procCloseHandle, h) }

// filetime es FILETIME: dos DWORD que juntos forman los ticks de 100 ns.
type filetime struct{ LowDateTime, HighDateTime uint32 }
