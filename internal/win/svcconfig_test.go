package win

// Tests de bytes sintéticos para la configuración de un servicio
// (QUERY_SERVICE_CONFIGW y SERVICE_FAILURE_ACTIONSW): los punteros apuntan
// dentro del mismo búfer, y un búfer truncado o con punteros que se salen
// termina en ErrMalformed, nunca en un pánico.

import (
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// qscBuf arma un QUERY_SERVICE_CONFIGW con sus cadenas a continuación.
func qscBuf(base uintptr, tipo, inicio uint32, binario, cuenta, visible string) []byte {
	b := make([]byte, qscSize)
	binary.LittleEndian.PutUint32(b[qscServiceType:], tipo)
	binary.LittleEndian.PutUint32(b[qscStartType:], inicio)
	poner := func(off int, s string) {
		if s == "\x00nulo" {
			return // el puntero queda en NULL
		}
		binary.LittleEndian.PutUint64(b[off:], uint64(base)+uint64(len(b)))
		for _, u := range utf16.Encode([]rune(s)) {
			b = binary.LittleEndian.AppendUint16(b, u)
		}
		b = append(b, 0, 0)
	}
	poner(qscBinaryPath, binario)
	poner(qscStartName, cuenta)
	poner(qscDisplayName, visible)
	return b
}

func TestParseServiceConfig(t *testing.T) {
	base := uintptr(0x7ff600000000)
	b := qscBuf(base, ServiceWin32OwnProcess, uint32(StartAuto),
		`"C:\Program Files\MySQL\MySQL Server 8.0\bin\mysqld.exe" --defaults-file="C:\ProgramData\MySQL\my.ini" MySQL80`,
		"NT AUTHORITY\\NetworkService", "MySQL80 — base de datos")
	c, err := parseServiceConfig(b, base)
	if err != nil {
		t.Fatal(err)
	}
	if c.Type != ServiceWin32OwnProcess || c.Start != StartAuto || !c.OwnProcess() || c.Driver() {
		t.Errorf("tipo/inicio: %+v", c)
	}
	if c.Account != `NT AUTHORITY\NetworkService` || c.DisplayName != "MySQL80 — base de datos" {
		t.Errorf("cuenta/nombre: %q / %q", c.Account, c.DisplayName)
	}
	if want := `C:\Program Files\MySQL\MySQL Server 8.0\bin\mysqld.exe`; c.Image() != want {
		t.Errorf("imagen = %q, esperaba %q", c.Image(), want)
	}

	// Un controlador: sin cuenta (puntero NULL) y con ruta relativa a Windows.
	d, err := parseServiceConfig(qscBuf(base, ServiceKernelDriver, uint32(StartDemand), `system32\drivers\winnat.sys`, "\x00nulo", "Windows NAT Driver"), base)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Driver() || d.OwnProcess() || d.Start != StartDemand || d.Account != "" || d.Image() != "" {
		t.Errorf("controlador: %+v (imagen %q)", d, d.Image())
	}
}

func TestParseServiceConfigCorrupto(t *testing.T) {
	base := uintptr(0x10000)
	b := qscBuf(base, ServiceWin32ShareProc, uint32(StartAuto), `C:\Windows\system32\svchost.exe -k netsvcs -p`, "LocalSystem", "IP Helper")

	for corte := 0; corte < len(b); corte++ {
		c, err := parseServiceConfig(b[:corte], base)
		if err == nil {
			t.Fatalf("cortado en %d de %d: %+v sin error", corte, len(b), c)
		}
		if !isMalformed(err) {
			t.Fatalf("cortado en %d: %v no es ErrMalformed", corte, err)
		}
	}
	// Punteros que apuntan afuera del búfer, antes y después.
	for _, off := range []int{qscBinaryPath, qscStartName, qscDisplayName} {
		for _, ptr := range []uint64{1, uint64(base) - 2, uint64(base) + uint64(len(b)) + 2, ^uint64(0)} {
			mal := append([]byte(nil), b...)
			binary.LittleEndian.PutUint64(mal[off:], ptr)
			if c, err := parseServiceConfig(mal, base); !isMalformed(err) {
				t.Errorf("puntero 0x%x en 0x%x: %+v, %v", ptr, off, c, err)
			}
		}
	}
	// Una cadena sin terminador: llega al final del búfer y no sigue leyendo.
	sin := append([]byte(nil), b[:len(b)-2]...)
	if c, err := parseServiceConfig(sin, base); !isMalformed(err) {
		t.Errorf("sin terminador: %+v, %v", c, err)
	}
}

// sfaBuf arma un SERVICE_FAILURE_ACTIONSW con n acciones a continuación.
func sfaBuf(base uintptr, declaradas uint32, acciones ...RecoveryAction) []byte {
	b := make([]byte, sfaSize)
	binary.LittleEndian.PutUint32(b[0:], 86400) // dwResetPeriod
	binary.LittleEndian.PutUint32(b[sfaActionCount:], declaradas)
	if len(acciones) > 0 {
		binary.LittleEndian.PutUint64(b[sfaActions:], uint64(base)+sfaSize)
	}
	for i, a := range acciones {
		b = binary.LittleEndian.AppendUint32(b, uint32(a))
		b = binary.LittleEndian.AppendUint32(b, uint32(60000*(i+1))) // la demora
	}
	return b
}

func TestParseFailureActions(t *testing.T) {
	base := uintptr(0x20000)
	acts, err := parseFailureActions(sfaBuf(base, 3, RecoveryRestart, RecoveryRestart, RecoveryNone), base)
	if err != nil || len(acts) != 3 || acts[0] != RecoveryRestart || acts[2] != RecoveryNone {
		t.Fatalf("acciones = %v, %v", acts, err)
	}
	if !(ServiceConfig{Recovery: acts}).Restarts() {
		t.Error("con una acción de reinicio, Windows lo vuelve a levantar")
	}
	if (ServiceConfig{Recovery: []RecoveryAction{RecoveryNone, RecoveryCommand, RecoveryReboot}}).Restarts() {
		t.Error("correr un comando o reiniciar la máquina no es relanzar el servicio")
	}

	// Sin acciones: cuenta cero, o puntero NULL.
	for _, b := range [][]byte{sfaBuf(base, 0), sfaBuf(base, 2)} {
		if acts, err := parseFailureActions(b, base); err != nil || acts != nil {
			t.Errorf("sin acciones: %v, %v", acts, err)
		}
	}
	// Declara más acciones de las que entran en el búfer.
	if acts, err := parseFailureActions(sfaBuf(base, 4, RecoveryRestart), base); !isMalformed(err) {
		t.Errorf("declaradas 4 y hay 1: %v, %v", acts, err)
	}
	// Una cuenta absurda no se usa para reservar memoria.
	if acts, err := parseFailureActions(sfaBuf(base, 1<<30, RecoveryRestart), base); !isMalformed(err) {
		t.Errorf("cuenta absurda: %v, %v", acts, err)
	}
	// El puntero a las acciones apunta afuera.
	mal := sfaBuf(base, 1, RecoveryRestart)
	binary.LittleEndian.PutUint64(mal[sfaActions:], uint64(base)+uint64(len(mal))+8)
	if acts, err := parseFailureActions(mal, base); !isMalformed(err) {
		t.Errorf("puntero afuera: %v, %v", acts, err)
	}
	for corte := 0; corte < sfaSize; corte++ {
		if _, err := parseFailureActions(sfaBuf(base, 1, RecoveryRestart)[:corte], base); !isMalformed(err) {
			t.Fatalf("cortado en %d: %v", corte, err)
		}
	}
}

// Image saca el ejecutable de la línea de comandos del servicio. Las rutas
// con espacios y sin comillas son un clásico de los instaladores (y un
// agujero de seguridad conocido): se toma el prefijo más corto que termina en
// ".exe" seguido de un espacio o del final, que es el que Windows ejecuta si
// existe.
func TestServiceImage(t *testing.T) {
	casos := map[string]string{
		`"C:\Program Files\PostgreSQL\16\bin\pg_ctl.exe" runservice -N "postgresql-x64-16"`: `C:\Program Files\PostgreSQL\16\bin\pg_ctl.exe`,
		`C:\Windows\system32\svchost.exe -k netsvcs -p`:                                     `C:\Windows\system32\svchost.exe`,
		`C:\Program Files\Mi App\servicio.exe --puerto 8080`:                                `C:\Program Files\Mi App\servicio.exe`,
		`C:\tools\daemon.EXE`:                   `C:\tools\daemon.EXE`,
		`C:\a.exe.config\b.exe -x`:              `C:\a.exe.config\b.exe`,
		`  "C:\con espacios alrededor\x.exe"  `: `C:\con espacios alrededor\x.exe`,
		`"C:\comilla sin cerrar\x.exe`:          `C:\comilla sin cerrar\x.exe`,
		`system32\drivers\winnat.sys`:           ``,
		`\SystemRoot\System32\drivers\HTTP.sys`: ``,
		``:                                      ``,
		`C:\sin\extension arg`:                  ``,
		"C:\\tab\\x.exe\t-k":                    `C:\tab\x.exe`,
	}
	for linea, want := range casos {
		if got := (ServiceConfig{BinaryPath: linea}).Image(); got != want {
			t.Errorf("Image(%q) = %q, esperaba %q", linea, got, want)
		}
	}
}
