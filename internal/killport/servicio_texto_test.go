package killport

// La tabla de sospechosos de siempre y cómo se cuenta la configuración de un
// servicio. Puro: corre en cualquier plataforma.

import (
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/win"
)

func TestConocidos(t *testing.T) {
	casos := []struct {
		servicio, imagen string
		puertos          ports.Set
		dice             string // "" = no está en la tabla
	}{
		{"VMwareHostd", "vmware-hostd.exe", ports.Of(443), "VMware Workstation Server"},
		{"", "VMWARE-HOSTD.EXE", ports.Of(443), "Shared VMs"},
		{"W3SVC", "svchost.exe", ports.Of(80), "IIS"},
		{"WAS", "", ports.Of(443), "net stop was /y"},
		{"", "w3wp.exe", ports.Of(80, 8080), "IIS"},
		{"MsDepSvc", "MsDepSvc.exe", ports.Of(80), "Web Deploy"},
		{"SQLServerReportingServices", "ReportingServicesService.exe", ports.Of(80), "Reporting Services"},
		{"ReportServer$SQLEXPRESS", "", ports.Of(80), "Reporting Services"},
		{"WinRM", "svchost.exe", ports.Of(5985), "WinRM"},
		{"", "Skype.exe", ports.Of(443), "Skype"},
		{"MySQL80", "mysqld.exe", ports.Of(3306), "MySQL"},
		{"MariaDB", "mariadbd.exe", ports.Of(3306), "MariaDB"},
		{"postgresql-x64-16", "postgres.exe", ports.Of(5432), "PostgreSQL"},
		{"MSSQL$SQLEXPRESS", "sqlservr.exe", ports.Of(1433), "SQL Server"},
		{"MSSQLSERVER", "", ports.Of(1433), "SQL Server"},
		{"MongoDB", "mongod.exe", ports.Of(27017), "MongoDB"},
		{"Apache2.4", "httpd.exe", ports.Of(80), "Apache"},
		{"jenkins", "jenkins.exe", ports.Of(8080), "Jenkins"},
		// Conocido por OTRO puerto: en este es un programa cualquiera.
		{"VMwareHostd", "vmware-hostd.exe", ports.Of(8443), ""},
		{"", "httpd.exe", ports.Of(9000), ""},
		{"MySQL80", "mysqld.exe", ports.Of(3307), ""},
		// Lo que no está.
		{"", "node.exe", ports.Of(3000), ""},
		{"Spooler", "spoolsv.exe", ports.Of(80), ""},
		{"", "", ports.Of(80), ""},
		{"w3svcx", "", ports.Of(80), ""}, // los nombres exactos son exactos
		{"", "xmysqld.exe", ports.Of(3306), ""},
	}
	for _, c := range casos {
		s, ok := conocido(c.servicio, c.imagen, c.puertos)
		if ok != (c.dice != "") {
			t.Errorf("conocido(%q, %q, %v) = %v", c.servicio, c.imagen, c.puertos, ok)
			continue
		}
		if ok && !strings.Contains(s.texto(), c.dice) {
			t.Errorf("conocido(%q, %q, %v) dice %q, esperaba que nombre %q", c.servicio, c.imagen, c.puertos, s.texto(), c.dice)
		}
	}
}

// La tabla es chica, explicativa y con fuente. Y ningún consejo es uno de los
// martillos peligrosos que recomiendan los foros.
func TestConocidosTabla(t *testing.T) {
	if len(sospechosos) > 16 {
		t.Errorf("la tabla tiene %d entradas: tiene que ser chica", len(sospechosos))
	}
	prohibido := []string{"start= disabled", "start=disabled", "net stop http", "sc config http", "net stop iphlpsvc", "sc delete", "taskkill"}
	for i, s := range sospechosos {
		if s.que == "" || s.fuente == "" || len(s.puertos) == 0 || len(s.servicios)+len(s.imagenes) == 0 {
			t.Errorf("entrada %d incompleta: %+v", i, s)
		}
		if !strings.HasPrefix(s.texto(), "es ") || strings.HasSuffix(s.texto(), ".") {
			t.Errorf("entrada %d: el texto %q no sigue la forma", i, s.texto())
		}
		for _, n := range append(append([]string{}, s.servicios...), s.imagenes...) {
			if n != strings.ToLower(n) || strings.TrimSpace(n) != n || n == "" || n == "*" {
				t.Errorf("entrada %d: el nombre %q tiene que ir en minúsculas y sin espacios", i, n)
			}
		}
		for _, malo := range prohibido {
			if strings.Contains(strings.ToLower(s.texto()), malo) {
				t.Errorf("entrada %d aconseja %q: %s", i, malo, s.texto())
			}
		}
	}
}

func cfgServicio(inicio win.StartType, retrasado bool, disparadores int) win.ServiceConfig {
	return win.ServiceConfig{Name: "MySQL80", Type: win.ServiceWin32OwnProcess, Start: inicio, Delayed: retrasado, Triggers: disparadores,
		BinaryPath: `"C:\Program Files\MySQL\bin\mysqld.exe" MySQL80`}
}

func TestInicioDeServicio(t *testing.T) {
	casos := []struct {
		cfg     win.ServiceConfig
		clave   string
		texto   string
		vuelve  bool
		porque  string
		comando string
	}{
		{cfgServicio(win.StartAuto, false, 0), "auto", "inicio automático", true, "vuelve al reiniciar Windows", "sc config MySQL80 start= auto"},
		{cfgServicio(win.StartAuto, true, 0), "auto-diferido", "inicio automático (retrasado)", true, "minutos después", "sc config MySQL80 start= delayed-auto"},
		{cfgServicio(win.StartDemand, false, 0), "manual", "inicio manual", false, "no vuelve solo", "sc config MySQL80 start= demand"},
		{cfgServicio(win.StartDemand, false, 1), "manual", "inicio manual", true, "1 disparador:", "sc config MySQL80 start= demand"},
		{cfgServicio(win.StartDemand, false, 3), "manual", "inicio manual", true, "3 disparadores", "sc config MySQL80 start= demand"},
		{cfgServicio(win.StartDisabled, false, 2), "deshabilitado", "deshabilitado", false, "no se puede arrancar", "sc config MySQL80 start= disabled"},
		{cfgServicio(win.StartBoot, false, 0), "arranque", "lo carga Windows al arrancar", true, "cada arranque", "sc config MySQL80 start= boot"},
		{cfgServicio(win.StartSystem, false, 0), "sistema", "lo carga Windows al arrancar", true, "cada arranque", "sc config MySQL80 start= system"},
		{cfgServicio(win.StartType(9), false, 0), "desconocido", "inicio desconocido", false, "", ""},
	}
	for _, c := range casos {
		if got := inicioClave(c.cfg); got != c.clave {
			t.Errorf("inicioClave(%v) = %q", c.cfg.Start, got)
		}
		if got := inicioTexto(c.cfg); got != c.texto {
			t.Errorf("inicioTexto(%v) = %q", c.cfg.Start, got)
		}
		vuelve, porque := vuelveSolo(c.cfg)
		if vuelve != c.vuelve || !strings.Contains(porque, c.porque) {
			t.Errorf("vuelveSolo(%v, %d disparadores) = %v, %q", c.cfg.Start, c.cfg.Triggers, vuelve, porque)
		}
		if got := comandoInicio("MySQL80", c.cfg); got != c.comando {
			t.Errorf("comandoInicio(%v) = %q", c.cfg.Start, got)
		}
		if l := lineaServicio("MySQL80", c.cfg); !strings.HasPrefix(l, "servicio MySQL80 · "+c.texto+": ") {
			t.Errorf("lineaServicio = %q", l)
		}
	}
	if got := comandoInicio("Apple Mobile Device Service", cfgServicio(win.StartAuto, false, 0)); got != `sc config "Apple Mobile Device Service" start= auto` {
		t.Errorf("un nombre con espacios va entre comillas: %q", got)
	}
	if got := recuperacionClaves(win.ServiceConfig{Recovery: []win.RecoveryAction{win.RecoveryRestart, win.RecoveryCommand, win.RecoveryReboot, win.RecoveryNone, 7}}); strings.Join(got, ",") != "reiniciar-servicio,comando,reiniciar-equipo,nada,desconocida" {
		t.Errorf("recuperacionClaves = %v", got)
	}
	if got := recuperacionClaves(win.ServiceConfig{}); got == nil || len(got) != 0 {
		t.Errorf("sin acciones es una lista vacía, no nula: %#v", got)
	}
}

// A qué servicios killport les cambia el inicio: solo a los de terceros.
func TestMotivoNoManual(t *testing.T) {
	const raiz = `C:\Windows`
	de := func(tipo uint32, binario string) win.ServiceConfig {
		return win.ServiceConfig{Type: tipo, Start: win.StartAuto, BinaryPath: binario}
	}
	casos := []struct {
		nombre string
		cfg    win.ServiceConfig
		raiz   string
		dice   string // "" = se puede
	}{
		{"MySQL", de(win.ServiceWin32OwnProcess, `"C:\Program Files\MySQL\bin\mysqld.exe" --defaults-file=x MySQL80`), raiz, ""},
		{"postgres, sin comillas y con espacios", de(win.ServiceWin32OwnProcess, `C:\Program Files\PostgreSQL\16\bin\pg_ctl.exe runservice`), raiz, ""},
		{"en otra unidad", de(win.ServiceWin32OwnProcess, `D:\srv\app.exe`), raiz, ""},
		{"por red", de(win.ServiceWin32OwnProcess, `\\servidor\compartido\app.exe`), raiz, ""},
		{"compartido pero de terceros", de(win.ServiceWin32ShareProc, `C:\Program Files\X\host.exe -k grupo`), raiz, ""},
		{"la carpeta se parece a la de Windows", de(win.ServiceWin32OwnProcess, `C:\WindowsApps\x\svc.exe`), raiz, ""},

		{"un svchost", de(win.ServiceWin32ShareProc, `C:\Windows\system32\svchost.exe -k netsvcs -p`), raiz, "servicio de Windows"},
		{"otras mayúsculas", de(win.ServiceWin32OwnProcess, `c:\WINDOWS\System32\spoolsv.exe`), raiz, "servicio de Windows"},
		{"con la variable", de(win.ServiceWin32ShareProc, `%SystemRoot%\system32\svchost.exe -k LocalService`), raiz, "servicio de Windows"},
		{"con %windir%", de(win.ServiceWin32OwnProcess, `%windir%\system32\inetsrv\inetinfo.exe`), raiz, "servicio de Windows"},
		{"con el enlace del kernel", de(win.ServiceWin32OwnProcess, `\SystemRoot\System32\algo.exe`), raiz, "servicio de Windows"},
		{"ruta relativa", de(win.ServiceWin32OwnProcess, `system32\algo.exe -x`), raiz, "servicio de Windows"},
		{"con barras al revés", de(win.ServiceWin32OwnProcess, `C:/Windows/System32/algo.exe`), raiz, "servicio de Windows"},
		{"winnat", de(win.ServiceKernelDriver, `system32\drivers\winnat.sys`), raiz, "controlador"},
		{"un controlador de terceros", de(win.ServiceKernelDriver, `\??\C:\Program Files\X\drv.sys`), raiz, "controlador"},
		{"un controlador de sistema de archivos", de(win.ServiceFileSystemDriver, `C:\x\fs.sys`), raiz, "controlador"},
		{"un servicio por usuario", de(win.ServiceUserService, `C:\Program Files\X\u.exe`), raiz, "no es un servicio de Win32"},
		{"sin ejecutable", de(win.ServiceWin32OwnProcess, ``), raiz, "ejecutable"},
		{"sin saber dónde está Windows", de(win.ServiceWin32OwnProcess, `C:\Program Files\X\a.exe`), "", "carpeta de Windows"},
	}
	for _, c := range casos {
		got := motivoNoManual(c.cfg, c.raiz)
		if (got == "") != (c.dice == "") || !strings.Contains(got, c.dice) {
			t.Errorf("%s: motivoNoManual = %q, esperaba %q", c.nombre, got, c.dice)
		}
	}
}
