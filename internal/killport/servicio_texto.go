package killport

// servicio_texto.go: cómo se CUENTA la configuración de un servicio. Detener
// un servicio no es el final de la historia: el que arranca con Windows vuelve
// en el próximo reinicio, y la pelea por el puerto se repite. Lo que el
// usuario necesita saber antes de confirmar es si eso que va a detener vuelve
// solo, y cómo se hace para que no.
//
// Puro: sin build tag (win.ServiceConfig es aritmética sobre bytes).

import (
	"strconv"
	"strings"

	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/win"
)

// inicioClave es el tipo de inicio como clave estable del JSON.
func inicioClave(c win.ServiceConfig) string {
	switch c.Start {
	case win.StartBoot:
		return "arranque"
	case win.StartSystem:
		return "sistema"
	case win.StartAuto:
		if c.Delayed {
			return "auto-diferido"
		}
		return "auto"
	case win.StartDemand:
		return "manual"
	case win.StartDisabled:
		return "deshabilitado"
	}
	return "desconocido"
}

// inicioTexto es el tipo de inicio para mostrar: "inicio automático".
func inicioTexto(c win.ServiceConfig) string {
	switch c.Start {
	case win.StartBoot, win.StartSystem:
		return "lo carga Windows al arrancar"
	case win.StartAuto:
		if c.Delayed {
			return "inicio automático (retrasado)"
		}
		return "inicio automático"
	case win.StartDemand:
		return "inicio manual"
	case win.StartDisabled:
		return "deshabilitado"
	}
	return "inicio desconocido"
}

// vuelveSolo dice si el servicio arranca por su cuenta después de detenerlo, y
// por qué. Un servicio manual con disparadores también vuelve: un evento del
// sistema (llegó la red, se enchufó un dispositivo) lo despierta.
func vuelveSolo(c win.ServiceConfig) (vuelve bool, porque string) {
	switch c.Start {
	case win.StartBoot, win.StartSystem:
		return true, "lo carga Windows en cada arranque"
	case win.StartAuto:
		if c.Delayed {
			return true, "vuelve al reiniciar Windows, un par de minutos después del arranque"
		}
		return true, "vuelve al reiniciar Windows"
	case win.StartDemand:
		if c.Triggers > 0 {
			return true, "aunque es manual, tiene " + contar(c.Triggers, "disparador", "disparadores") + ": un evento del sistema lo puede volver a arrancar"
		}
		return false, "no vuelve solo: arranca cuando alguien lo pide"
	}
	return false, "no vuelve: no se puede arrancar"
}

// lineaServicio es el renglón del bloque: "servicio MySQL80 · inicio
// automático: vuelve al reiniciar Windows".
func lineaServicio(nombre string, c win.ServiceConfig) string {
	_, porque := vuelveSolo(c)
	return "servicio " + nombre + " · " + inicioTexto(c) + ": " + porque
}

// comandoInicio es el comando de sc que deja al servicio con ESE tipo de
// inicio: lo que se imprime para deshacer un --manual. El espacio después de
// "start=" es de sc, no un error de tipeo.
func comandoInicio(nombre string, c win.ServiceConfig) string {
	valor := ""
	switch c.Start {
	case win.StartBoot:
		valor = "boot"
	case win.StartSystem:
		valor = "system"
	case win.StartAuto:
		valor = "auto"
		if c.Delayed {
			valor = "delayed-auto"
		}
	case win.StartDemand:
		valor = "demand"
	case win.StartDisabled:
		valor = "disabled"
	default:
		return ""
	}
	return "sc config " + citarSC(nombre) + " start= " + valor
}

// citarSC pone el nombre entre comillas si hace falta para que sc lo lea como
// un solo argumento.
func citarSC(nombre string) string {
	if strings.ContainsAny(nombre, " \t&|<>^()%!,;=") {
		return `"` + nombre + `"`
	}
	return nombre
}

// recuperacionClaves son las acciones de recuperación como claves del JSON.
func recuperacionClaves(c win.ServiceConfig) []string {
	out := []string{}
	for _, a := range c.Recovery {
		switch a {
		case win.RecoveryNone:
			out = append(out, "nada")
		case win.RecoveryRestart:
			out = append(out, "reiniciar-servicio")
		case win.RecoveryReboot:
			out = append(out, "reiniciar-equipo")
		case win.RecoveryCommand:
			out = append(out, "comando")
		default:
			out = append(out, "desconocida")
		}
	}
	return out
}

// contar arma "1 disparador" / "3 disparadores".
func contar(n int, uno, varios string) string {
	if n == 1 {
		return "1 " + uno
	}
	return strconv.Itoa(n) + " " + varios
}

// motivoNoManual dice por qué killport no le cambia el inicio a un servicio, o
// "" si se puede. systemRoot es la carpeta de Windows. La regla es la misma
// que decide si un servicio se detiene o se niega: solo los de terceros, los
// que instaló un programa. Un controlador, un servicio de Windows o uno que
// vive dentro de un svchost no se reconfiguran jamás: de ellos dependen otras
// cosas, y el daño de equivocarse no se arregla con un Ctrl+Z.
func motivoNoManual(c win.ServiceConfig, systemRoot string) string {
	switch img := c.Image(); {
	case c.Driver():
		return "es un controlador, no un servicio"
	case c.Type&(win.ServiceWin32OwnProcess|win.ServiceWin32ShareProc) == 0:
		return "no es un servicio de Win32"
	case img == "":
		return "no pude saber cuál es su ejecutable"
	case systemRoot == "":
		return "no pude saber cuál es la carpeta de Windows"
	case deWindows(img, systemRoot):
		return "es un servicio de Windows"
	}
	return ""
}

// deWindows dice si el ejecutable de un servicio está en la carpeta de
// Windows. La configuración de un servicio puede nombrarla de varias formas
// además de la ruta completa: con la variable (%SystemRoot%\system32\…), con
// el enlace del kernel (\SystemRoot\…) o con una ruta relativa, que el
// administrador de servicios resuelve contra la carpeta de Windows.
func deWindows(img, systemRoot string) bool {
	low := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(img), "/", `\`))
	for _, pre := range []string{`%systemroot%\`, `%windir%\`, `\systemroot\`} {
		if strings.HasPrefix(low, pre) {
			return true
		}
	}
	absoluta := strings.HasPrefix(low, `\\`) || (len(low) >= 3 && low[1] == ':' && low[2] == '\\')
	return !absoluta || reaper.UnderDir(img, systemRoot)
}
