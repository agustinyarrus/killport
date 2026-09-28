package win

import "testing"

func TestDosPath(t *testing.T) {
	devs := []devicePrefix{
		{`\Device\HarddiskVolume1`, "E:"},
		{`\Device\HarddiskVolume3`, "C:"},
		{`\Device\HarddiskVolume10`, "D:"},
	}
	for _, tc := range []struct{ nt, want string }{
		{`\Device\HarddiskVolume3\Windows\System32\svchost.exe`, `C:\Windows\System32\svchost.exe`},
		{`\device\harddiskvolume3\x.exe`, `C:\x.exe`}, // sin distinguir mayúsculas
		// El 1 no es prefijo del 10: gana el dispositivo exacto.
		{`\Device\HarddiskVolume10\tools\a.exe`, `D:\tools\a.exe`},
		{`\Device\HarddiskVolume1\a.exe`, `E:\a.exe`},
		{`\Device\HarddiskVolume1`, `E:`},
		{`\Device\Mup\srv\share\app.exe`, `\\srv\share\app.exe`},
		{`\Device\LanmanRedirector\;Z:0000000000012345\srv\share\app.exe`, `\\srv\share\app.exe`},
		{`\??\C:\Windows\x.exe`, `C:\Windows\x.exe`},
		{`\SystemRoot\System32\smss.exe`, `C:\WINDOWS\System32\smss.exe`},
		{`\Device\HarddiskVolume99\x.exe`, `\Device\HarddiskVolume99\x.exe`}, // desconocido: fiel
		{`\SystemRootX\a`, `\SystemRootX\a`},                                 // no es el segmento \SystemRoot
		{"", ""},
		{"Registry", "Registry"}, // pseudo-procesos sin ruta
	} {
		if got := dosPath(tc.nt, devs, `C:\WINDOWS`); got != tc.want {
			t.Errorf("dosPath(%q) = %q, esperaba %q", tc.nt, got, tc.want)
		}
	}
}
