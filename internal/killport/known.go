package killport

// known.go: los SOSPECHOSOS DE SIEMPRE. Las peleas clásicas por un puerto en
// Windows tienen media docena de culpables que se repiten, y cada uno se
// resuelve de una forma propia que no tiene nada que ver con matar un proceso:
// una opción escondida en un menú, un servicio que hay que reconfigurar. Es lo
// que las respuestas más votadas de los foros repiten a mano desde hace quince
// años.
//
// La tabla es chica a propósito, y es SOLO EXPLICATIVA: agrega una línea al
// bloque del objetivo o de la negativa. No decide nada —qué se toca y qué no
// lo decide la planificación, con sus reglas— y no ejecuta nada. Cada entrada
// lleva su fuente, porque una tabla así envejece: el día que un consejo deje
// de valer, tiene que poder rastrearse de dónde salió.
//
// Puro: sin build tag, se prueba en cualquier plataforma.

import (
	"strings"

	"github.com/agustinyarrus/killport/internal/ports"
)

// sospechoso es un programa conocido por quedarse con un puerto.
type sospechoso struct {
	servicios []string // nombres de servicio en minúsculas; con "*" al final, prefijo
	imagenes  []string // nombres de imagen en minúsculas
	puertos   []uint16 // los puertos por los que se lo conoce; vacío = cualquiera
	que       string   // qué es, en una frase que sigue a "es …"
	como      string   // cómo se libera o se evita, si hay una forma propia; "" si no
	fuente    string   // de dónde sale el dato
}

// sospechosos va de lo más específico a lo más general: gana la primera
// entrada que coincide.
var sospechosos = []sospechoso{
	{
		servicios: []string{"vmwarehostd"}, imagenes: []string{"vmware-hostd.exe"}, puertos: []uint16{443, 902},
		que:    "VMware Workstation Server (las máquinas virtuales compartidas)",
		como:   "se apaga en VMware › Edit › Preferences › Shared VMs, donde también se le cambia el puerto",
		fuente: "Stack Overflow 21182512",
	},
	{
		servicios: []string{"sqlserverreportingservices", "powerbireportserver", "reportserver", "reportserver$*"},
		imagenes:  []string{"reportingservicesservice.exe"}, puertos: []uint16{80, 443},
		que:    "SQL Server Reporting Services",
		como:   "el puerto se cambia en el Administrador de configuración de Reporting Services (Dirección URL del servicio web)",
		fuente: "Stack Overflow 1430141",
	},
	{
		servicios: []string{"msdepsvc"}, puertos: []uint16{80},
		que:    "el Agente de Web Deploy (Web Deployment Agent Service)",
		como:   "`net stop MsDepSvc` lo detiene y `sc config MsDepSvc start= demand` hace que no vuelva (los dos como administrador)",
		fuente: "Stack Overflow 1430141",
	},
	{
		servicios: []string{"w3svc", "was"}, imagenes: []string{"w3wp.exe"}, puertos: []uint16{80, 443},
		que:    "IIS, el servidor web de Windows",
		como:   "detené el sitio en el Administrador de IIS; `net stop was /y` baja TODOS los sitios y `sc config W3SVC start= demand` hace que no vuelvan (como administrador)",
		fuente: "Stack Overflow 20558410",
	},
	{
		servicios: []string{"winrm"}, puertos: []uint16{5985, 5986},
		que:    "la administración remota de Windows (WinRM)",
		como:   "si no la usás, `Disable-PSRemoting -Force` en un PowerShell de administrador",
		fuente: "documentación de WinRM",
	},
	{
		imagenes: []string{"skype.exe"}, puertos: []uint16{80, 443},
		que:    "Skype, que usa el 80 y el 443 como alternativa para las conexiones entrantes",
		como:   "se apaga en Herramientas › Opciones › Avanzada › Conexión",
		fuente: "Stack Overflow 1430141",
	},
	{
		servicios: []string{"apache*"}, imagenes: []string{"httpd.exe"}, puertos: []uint16{80, 443, 8080},
		que:    "Apache instalado como servicio de Windows",
		fuente: "Stack Overflow 20558410",
	},
	{
		servicios: []string{"mysql*", "mariadb*"}, imagenes: []string{"mysqld.exe", "mariadbd.exe"}, puertos: []uint16{3306, 33060},
		que:    "MySQL o MariaDB instalado como servicio de Windows",
		fuente: "instalador de MySQL",
	},
	{
		servicios: []string{"postgresql*"}, imagenes: []string{"postgres.exe"}, puertos: []uint16{5432},
		que:    "PostgreSQL instalado como servicio de Windows",
		fuente: "instalador de PostgreSQL",
	},
	{
		servicios: []string{"mssqlserver", "mssql$*"}, imagenes: []string{"sqlservr.exe"}, puertos: []uint16{1433},
		que:    "SQL Server",
		fuente: "documentación de SQL Server",
	},
	{
		servicios: []string{"mongodb"}, imagenes: []string{"mongod.exe"}, puertos: []uint16{27017},
		que:    "MongoDB instalado como servicio de Windows",
		fuente: "instalador de MongoDB",
	},
	{
		servicios: []string{"jenkins"}, imagenes: []string{"jenkins.exe"}, puertos: []uint16{8080},
		que:    "Jenkins instalado como servicio de Windows",
		fuente: "instalador de Jenkins",
	},
}

// conocido busca en la tabla a quien tiene esos puertos: por el servicio, y si
// no por la imagen. servicio e imagen pueden venir vacíos. Coincide solo si
// ALGUNO de los puertos es uno de los que hacen conocido al programa: un
// httpd.exe en el 9000 es un Apache cualquiera, no el que tapa al 80.
func conocido(servicio, imagen string, puertos ports.Set) (sospechoso, bool) {
	servicio, imagen = strings.ToLower(strings.TrimSpace(servicio)), strings.ToLower(strings.TrimSpace(imagen))
	for _, s := range sospechosos {
		if !s.enPuertos(puertos) {
			continue
		}
		if servicio != "" && coincide(s.servicios, servicio) {
			return s, true
		}
		if imagen != "" && coincide(s.imagenes, imagen) {
			return s, true
		}
	}
	return sospechoso{}, false
}

func (s sospechoso) enPuertos(puertos ports.Set) bool {
	if len(s.puertos) == 0 {
		return true
	}
	for _, p := range s.puertos {
		if puertos.Contains(p) {
			return true
		}
	}
	return false
}

// coincide compara un nombre con los patrones: exacto, o por prefijo si el
// patrón termina en "*" (postgresql-x64-16, MSSQL$SQLEXPRESS).
func coincide(patrones []string, nombre string) bool {
	for _, p := range patrones {
		if pre, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(nombre, pre) {
				return true
			}
		} else if p == nombre {
			return true
		}
	}
	return false
}

// texto es la línea que se muestra: "es IIS, el servidor web de Windows: …".
func (s sospechoso) texto() string {
	if s.como == "" {
		return "es " + s.que
	}
	return "es " + s.que + ": " + s.como
}
