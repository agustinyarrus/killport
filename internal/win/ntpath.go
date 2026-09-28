package win

// ntpath.go: pasar una ruta en formato NT (la que da el kernel,
// "\Device\HarddiskVolume3\Windows\System32\svchost.exe") al formato que ve
// un usuario ("C:\Windows\System32\svchost.exe"). Sin build tag: es texto
// puro contra una tabla de dispositivos, y se prueba en cualquier plataforma.
//
// La tabla sale de QueryDosDeviceW por cada letra de unidad (dosdevice.go).
// Reglas, en orden:
//   - el dispositivo más LARGO que sea prefijo gana (con "\Device\HarddiskVolume1"
//     y "\Device\HarddiskVolume10" en la tabla, el segundo no debe caer en el
//     primero): se exige además que después del prefijo venga "\" o nada;
//   - los redirectores de red (\Device\Mup, \Device\LanmanRedirector) pasan a
//     UNC: "\Device\Mup\srv\share\x" → "\\srv\share\x";
//   - "\??\C:\x" (prefijo de objeto del administrador de objetos) → "C:\x";
//   - "\SystemRoot\x" → la carpeta de Windows (lo usan los procesos del
//     arranque temprano);
//   - lo que no se reconoce queda tal cual: mejor una ruta NT fiel que una
//     traducción inventada.

import "strings"

// devicePrefix es una entrada de la tabla: dispositivo NT → letra ("C:").
type devicePrefix struct{ device, drive string }

// dosPath traduce una ruta NT con la tabla de dispositivos y la carpeta de
// Windows. Comparación sin distinguir mayúsculas (el administrador de objetos
// de Windows no las distingue). O(dispositivos) por ruta.
func dosPath(nt string, devices []devicePrefix, windir string) string {
	if nt == "" {
		return ""
	}
	lower := strings.ToLower(nt)
	switch {
	case strings.HasPrefix(lower, `\??\`):
		return nt[len(`\??\`):]
	case hasSegmentPrefix(lower, `\systemroot`) && windir != "":
		return windir + nt[len(`\SystemRoot`):]
	}
	for _, redir := range [...]string{`\device\mup`, `\device\lanmanredirector`} {
		if hasSegmentPrefix(lower, redir) {
			rest := nt[len(redir):]
			// LanmanRedirector trae ";X:000000000000cafe\srv\share": se
			// descarta el segmento de sesión que empieza con ';'.
			if strings.HasPrefix(rest, `\;`) {
				if i := strings.Index(rest[1:], `\`); i >= 0 {
					rest = rest[1+i:]
				}
			}
			return `\` + rest
		}
	}
	best := -1
	for i, d := range devices {
		if hasSegmentPrefix(lower, strings.ToLower(d.device)) && (best < 0 || len(d.device) > len(devices[best].device)) {
			best = i
		}
	}
	if best < 0 {
		return nt
	}
	return devices[best].drive + nt[len(devices[best].device):]
}

// hasSegmentPrefix dice si s empieza con prefix y justo después viene "\" o
// el final: "\device\harddiskvolume1" NO es prefijo de "\device\harddiskvolume10\x".
func hasSegmentPrefix(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	return len(s) == len(prefix) || s[len(prefix)] == '\\'
}
