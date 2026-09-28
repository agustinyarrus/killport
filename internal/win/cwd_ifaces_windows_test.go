//go:build windows

package win

// Pruebas REALES contra el sistema: la carpeta de trabajo de
// otro proceso (64 bits y WOW64) y las interfaces de red contra el oráculo de
// PowerShell. Todo fixture se lanza sin ventana y se mata en t.Cleanup.

import (
	"context"
	"net/netip"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// lanzarEn lanza exe sin ventana, con dir de carpeta de trabajo.
func lanzarEn(t *testing.T, dir, exe string, args ...string) uint32 {
	t.Helper()
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	if err := cmd.Start(); err != nil {
		t.Fatalf("no pude lanzar %s: %v", exe, err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	return uint32(cmd.Process.Pid)
}

// matarConHijos termina al proceso de cmd (por su handle: no hay reciclado
// posible) y a sus hijos, reconocidos por IDENTIDAD —el PID del padre Y
// nacidos después que él— y abiertos con OpenForKill. No con `taskkill /T`:
// elige a los hijos solo por el PID del padre, y un proceso ajeno cuyo padre
// murió hace rato con ese mismo número caería también.
func matarConHijos(cmd *exec.Cmd) {
	padre := uint32(cmd.Process.Pid)
	procs, _ := AllProcesses()
	var nacido time.Time
	for _, p := range procs {
		if p.PID == padre {
			nacido = p.Created
		}
	}
	for _, p := range procs {
		if p.PPID == padre && p.PID != padre && !nacido.IsZero() && !p.Created.Before(nacido) {
			if h, err := OpenForKill(p.PID, p.Created); err == nil {
				h.Terminate(1)
				h.Close()
			}
		}
	}
	cmd.Process.Kill()
	cmd.Wait()
}

// La carpeta de trabajo que se lee de la memoria del proceso es la que le
// dio quien lo lanzó, en un proceso de 64 bits y en uno de 32 (WOW64: el
// ping de SysWOW64), con acentos y espacios en la ruta.
func TestCwdDeOtroProceso(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proyecto con ñandú")
	if err := exec.Command("cmd", "/c", "mkdir", dir).Run(); err != nil {
		t.Fatal(err)
	}
	for _, exe := range []string{
		filepath.Join(systemDirectory, "PING.EXE"),
		filepath.Join(filepath.Dir(systemDirectory), "SysWOW64", "PING.EXE"),
	} {
		pid := lanzarEn(t, dir, exe, "-n", "30", "127.0.0.1")
		var d ProcessDetail
		var err error
		for i := 0; i < 50; i++ { // el loader tarda un instante en armar el PEB
			if d, err = QueryDetail(pid, time.Time{}); err == nil && d.Cwd != "" {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil || !strings.EqualFold(d.Cwd, dir) {
			t.Errorf("%s: Cwd = %q (%v), esperaba %q", exe, d.Cwd, err, dir)
		}
	}
	// Un proceso que CAMBIA de carpeta después de arrancar: se lee la actual,
	// no la del arranque. En uno de 32 bits eso solo lo tiene su PEB de 32
	// bits (el nativo se quedó con la del arranque): es la prueba de que en
	// WOW64 manda el PEB32.
	otra := filepath.Join(dir, "otra carpeta")
	if err := exec.Command("cmd", "/c", "mkdir", otra).Run(); err != nil {
		t.Fatal(err)
	}
	for _, sys := range []string{systemDirectory, filepath.Join(filepath.Dir(systemDirectory), "SysWOW64")} {
		exe := filepath.Join(sys, "cmd.exe")
		cmd := exec.Command(exe)
		cmd.Dir = dir
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000,
			CmdLine: `/d /c "cd /d "` + otra + `" && ping -n 30 127.0.0.1 >nul"`}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { matarConHijos(cmd) })
		var d ProcessDetail
		for i := 0; i < 100; i++ {
			if d, _ = QueryDetail(uint32(cmd.Process.Pid), time.Time{}); strings.EqualFold(d.Cwd, otra) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !strings.EqualFold(d.Cwd, otra) {
			t.Errorf("%s cambió a %q y se leyó %q", exe, otra, d.Cwd)
		}
	}
	// El propio proceso: su carpeta es la del paquete que se prueba.
	d, err := QueryDetail(CurrentProcessID(), time.Time{})
	wd, _ := filepath.Abs(".")
	if err != nil || !strings.EqualFold(d.Cwd, wd) {
		t.Errorf("la propia: %q (%v), esperaba %q", d.Cwd, err, wd)
	}
	// Uno de otro usuario (System): no se puede leer su memoria, y no se
	// inventa nada.
	if d, err := QueryDetail(4, time.Time{}); err == nil && d.Cwd != "" {
		t.Errorf("System no debería dar carpeta: %q", d.Cwd)
	}
}

// Las interfaces, contra Get-NetIPAddress: cada dirección que informa
// PowerShell aparece con el mismo nombre de interfaz.
func TestInterfacesContraGetNetIPAddress(t *testing.T) {
	ifs, err := Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	nombre := map[netip.Addr]string{}
	for _, it := range ifs {
		for _, a := range it.Addrs {
			nombre[a.WithZone("")] = it.Name
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command",
		"[Console]::OutputEncoding=[Text.Encoding]::UTF8; Get-NetIPAddress | ForEach-Object { $_.InterfaceAlias + '|' + $_.IPAddress }")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("sin oráculo (Get-NetIPAddress): %v", err)
	}
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		alias, ip, ok := strings.Cut(strings.TrimSpace(l), "|")
		if !ok {
			continue
		}
		a, err := netip.ParseAddr(ip)
		if err != nil {
			continue
		}
		n++
		if got, ok := nombre[a.WithZone("")]; !ok || got != alias {
			t.Errorf("%s: Interfaces dice %q (%v), Get-NetIPAddress dice %q", a, got, ok, alias)
		}
	}
	if n == 0 {
		t.Skip("Get-NetIPAddress no informó direcciones")
	}
	t.Logf("%d direcciones en %d interfaces, todas coinciden", n, len(ifs))
}
