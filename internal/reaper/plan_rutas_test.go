package reaper

import "testing"

// UnderDir es la única regla de "está en esa carpeta": sin distinguir
// mayúsculas ni barras, y con el prefijo de ruta larga (\\?\) de por medio.
func TestUnderDir(t *testing.T) {
	casos := []struct {
		path, dir string
		want      bool
	}{
		{`C:\Windows\System32\svchost.exe`, `C:\Windows`, true},
		{`c:/windows/system32/svchost.exe`, `C:\WINDOWS\`, true},
		{`\\?\C:\Windows\System32\svchost.exe`, `C:\Windows`, true},
		{`C:\Windows\System32\svchost.exe`, `\\?\C:\Windows`, true},
		{` C:\Windows\System32\svchost.exe `, `C:\Windows`, true},
		{`C:\WindowsApps\x.exe`, `C:\Windows`, false},
		{`C:\Program Files\x.exe`, `C:\Windows`, false},
		{`\\?\UNC\srv\rec\x.exe`, `\\srv\rec`, true},
		{`\\?\UNC\srv\rec\x.exe`, `\\srv\otro`, false},
	}
	for _, c := range casos {
		if got := UnderDir(c.path, c.dir); got != c.want {
			t.Errorf("UnderDir(%q, %q) = %v", c.path, c.dir, got)
		}
	}
	// Lo que protege el plan usa la misma regla: un proceso con la ruta larga
	// dentro de la carpeta de Windows no es de un tercero.
	w := newWorld(nil, nil, Config{SystemRoot: `C:\Windows`})
	if w.thirdParty(Proc{ID: ID{PID: 900}, Name: "vpnagent.exe", Path: `\\?\C:\Windows\System32\vpnagent.exe`}) {
		t.Error(`una ruta \\?\ dentro de la carpeta de Windows no es de un tercero`)
	}
}
