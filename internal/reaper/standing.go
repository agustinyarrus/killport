package reaper

import "strings"

// Impostores.
//
// killport protege a un puñado de procesos por lo que SON para Windows: matar
// a csrss o a lsass reinicia la máquina, y un svchost aloja servicios que no
// se cierran a los golpes. Y en el listado general los oculta, porque son
// ruido para quien busca su servidor de desarrollo.
//
// Pero un nombre no es una identidad. Llamarse svchost.exe no cuesta nada: es
// la técnica de enmascaramiento más vieja del malware (MITRE ATT&CK T1036.005,
// "match legitimate name or location"), y un programa así, corriendo desde
// AppData con el puerto 4444 abierto, quedaría escondido Y protegido por la
// misma herramienta a la que uno le pregunta "¿qué es esto que escucha?".
//
// Lo que sí identifica a un proceso de Windows son dos cosas que un programa
// sin privilegios no puede falsificar:
//
//   - DÓNDE vive su imagen: exactamente %SystemRoot%\System32\<nombre>. No
//     "en algún lugar debajo de System32": ahí adentro hay carpetas donde
//     escribe cualquier usuario (spool\drivers\color, Tasks…). La ruta sale del
//     kernel (NtQuerySystemInformation clase 88), no de lo que el proceso
//     declara de sí mismo.
//   - QUIÉN lo lanzó: a un svchost lo lanza services.exe y a nadie más; a
//     lsass, wininit. Esto atrapa lo que la ruta sola deja pasar: el svchost
//     legítimo de System32 lanzado por un troyano para vaciarlo y correr su
//     código adentro (process hollowing) tiene la ruta perfecta y el padre
//     equivocado. El padre tiene que ser, a su vez, genuino.
//
// La protección y el ocultamiento por nombre exigen las dos. Si alguna falla,
// el proceso es un POSIBLE IMPOSTOR: se muestra, se dice por qué, y se lo
// trata como lo que es —un programa cualquiera—. "Posible" porque killport no
// verifica firmas (Authenticode más catálogos es caro y la regla de ruta y
// padre ya cubre el caso): dice lo que ve, no dicta sentencia.
//
// La última red no depende del nombre: un proceso MARCADO crítico
// (ProcessBreakOnTermination) no se termina jamás, se llame como se llame; eso
// lo comprueba el adaptador al abrirlo (ErrCritical).

// Standing es qué tan cierto es que un proceso sea el de Windows cuyo nombre
// lleva.
type Standing uint8

const (
	// StandingOrdinary: su nombre no es el de un proceso de Windows protegido.
	StandingOrdinary Standing = iota
	// StandingGenuine: nombre, ubicación y padre son los del proceso de Windows.
	StandingGenuine
	// StandingUnverified: lleva el nombre y nada lo desmiente, pero no se pudo
	// confirmar dónde vive su imagen (la ruta no se pudo leer, o no se sabe
	// cuál es la carpeta de Windows). Se lo PROTEGE —equivocarse para el otro
	// lado es tumbar la máquina— pero no se lo oculta.
	StandingUnverified
	// StandingImpostor: lleva el nombre de un proceso de Windows y no está
	// donde tendría que estar o no lo lanzó quien tendría que haberlo lanzado.
	StandingImpostor
)

func (s Standing) String() string {
	switch s {
	case StandingGenuine:
		return "genuino"
	case StandingUnverified:
		return "sin verificar"
	case StandingImpostor:
		return "posible impostor"
	}
	return ""
}

// Trusted dice si el proceso merece el trato de un proceso de Windows:
// genuino, o al menos no desmentido.
func (s Standing) Trusted() bool { return s == StandingGenuine || s == StandingUnverified }

// lineage es quién lanza a un proceso de Windows.
type lineage uint8

const (
	// bornOfKernel: lo crea el kernel. Su padre es System (PID 4, que no se
	// recicla jamás), así que se mira el PPID crudo: la hora de creación de
	// System no sirve para validar la arista (Registry y Secure System figuran
	// como nacidos ANTES que su padre). El smss.exe de cada sesión nueva es
	// hijo del smss.exe maestro.
	bornOfKernel lineage = iota + 1
	// bornOfSmss: lo lanza una instancia de smss.exe que sale enseguida. El
	// proceso queda SIN padre vivo; con padre vivo, solo puede ser ese smss.
	bornOfSmss
	// bornOfWininit y bornOfServices: su padre vive mientras viva Windows.
	bornOfWininit
	bornOfServices
)

// parentImage es la imagen del padre esperado, para los linajes que lo tienen.
func (l lineage) parentImage() string {
	switch l {
	case bornOfSmss:
		return "smss.exe"
	case bornOfWininit:
		return "wininit.exe"
	case bornOfServices:
		return "services.exe"
	}
	return ""
}

// systemImage describe a un proceso de Windows que killport trata distinto
// por su nombre.
type systemImage struct {
	file     bool    // su imagen es un archivo de System32 (los del kernel no tienen archivo)
	wow64    bool    // también puede vivir en SysWOW64 (el svchost de los servicios de 32 bits)
	born     lineage // quién lo lanza
	critical bool    // terminarlo cuelga o reinicia Windows
}

// systemImages: los nombres protegidos, en minúsculas. Algunos no llevan
// ".exe": son los procesos mínimos del kernel, que la instantánea nombra así
// ("Memory Compression" es como lo muestra el Administrador de tareas).
//
// Linajes medidos en Windows 11 (28120): 93 de 93 svchost son hijos de
// services.exe; services, lsass y LsaIso, de wininit; smss, de System; csrss,
// wininit y winlogon no tienen padre vivo.
var systemImages = map[string]systemImage{
	"system":             {born: bornOfKernel, critical: true},
	"registry":           {born: bornOfKernel, critical: true},
	"memcompression":     {born: bornOfKernel, critical: true},
	"memory compression": {born: bornOfKernel, critical: true},
	"secure system":      {born: bornOfKernel, critical: true},
	"smss.exe":           {file: true, born: bornOfKernel, critical: true},
	"csrss.exe":          {file: true, born: bornOfSmss, critical: true},
	"wininit.exe":        {file: true, born: bornOfSmss, critical: true},
	"winlogon.exe":       {file: true, born: bornOfSmss, critical: true},
	"services.exe":       {file: true, born: bornOfWininit, critical: true},
	"lsass.exe":          {file: true, born: bornOfWininit, critical: true},
	"lsaiso.exe":         {file: true, born: bornOfWininit, critical: true},
	"svchost.exe":        {file: true, wow64: true, born: bornOfServices},
	"svchost":            {file: true, born: bornOfServices},
}

// systemImageOf busca el nombre de un proceso en la tabla.
func systemImageOf(p Proc) (systemImage, bool) {
	si, ok := systemImages[strings.ToLower(strings.TrimSpace(p.Name))]
	return si, ok
}

// kernelPID es el PID de System: fijo desde Windows XP, y el único proceso
// cuyo número identifica.
const kernelPID = 4

// IsServiceHost dice si p es de verdad un host de servicios de Windows (un
// svchost genuino, o uno al que nada desmiente): ahí el nombre que importa es
// el del servicio, no el del ejecutable. Un programa que solo se llama
// svchost.exe no lo es. f es el bosque de donde salió p.
func IsServiceHost(p Proc, f *Forest, systemRoot string) bool {
	if !namedSvchost(p) {
		return false
	}
	s, _ := StandingOf(p, f, systemRoot)
	return s.Trusted()
}

// StandingOf juzga si p es de verdad el proceso de Windows cuyo nombre lleva.
// f es el bosque de la instantánea de donde salió p (de ahí sale su padre) y
// systemRoot la carpeta de Windows. Devuelve además el motivo, en español y
// para mostrar tal cual, cuando el proceso es un posible impostor.
//
// Pura. O(profundidad): juzgar a un proceso obliga a juzgar a su padre. La
// cadena genuina más larga tiene tres eslabones (svchost → services →
// wininit); lineageDepth la acota por si una instantánea rara trajera un
// ciclo de padres.
func StandingOf(p Proc, f *Forest, systemRoot string) (Standing, string) {
	return standingOf(p, f, systemRoot, lineageDepth)
}

// lineageDepth es cuántos ancestros se juzgan como mucho.
const lineageDepth = 6

func standingOf(p Proc, f *Forest, systemRoot string, depth int) (Standing, string) {
	si, ok := systemImageOf(p)
	if !ok {
		return StandingOrdinary, ""
	}
	if strings.EqualFold(strings.TrimSpace(p.Name), "system") {
		// System es su PID. Otro proceso con ese nombre es un archivo que
		// alguien llamó "System".
		if p.PID == kernelPID {
			return StandingGenuine, ""
		}
		return StandingImpostor, "se llama " + p.Name + " pero no es el proceso del kernel (que es el PID 4)"
	}

	// 1. Dónde vive.
	located := true
	switch {
	case !si.file && p.Path != "":
		return StandingImpostor, "se llama " + p.Name + " pero es un programa (" + p.Path + "); el de Windows es un proceso del kernel, sin archivo"
	case !si.file:
	case p.Path == "" || systemRoot == "":
		located = false
	case !atSystem32(p.Path, systemRoot, p.Name, si.wow64):
		return StandingImpostor, "se llama " + p.Name + " pero corre desde " + p.Path + "; el de Windows está en " + system32(systemRoot) + `\` + p.Name
	}

	// 2. Quién lo lanzó.
	if why := wrongLineage(p, si, f, systemRoot, depth); why != "" {
		return StandingImpostor, "se llama " + p.Name + " pero " + why
	}
	if !located {
		return StandingUnverified, ""
	}
	return StandingGenuine, ""
}

// wrongLineage devuelve por qué el padre de p no es el que tendría que ser, o
// "" si lo es (o no se puede saber: p no está en el bosque).
func wrongLineage(p Proc, si systemImage, f *Forest, systemRoot string, depth int) string {
	if depth <= 0 {
		return "su cadena de padres no es la de un proceso de Windows"
	}
	if si.born == bornOfKernel {
		if p.Parent == kernelPID {
			return ""
		}
		// El smss.exe de una sesión nueva es hijo del smss.exe maestro.
		if par, ok := parentIn(f, p); ok && si.file && strings.EqualFold(par.Name, p.Name) && trusted(par, f, systemRoot, depth-1) {
			return ""
		}
		return "no lo creó el kernel (su padre es " + parentText(f, p) + ")"
	}
	if f == nil {
		return ""
	}
	if _, known := f.Lookup(p.ID); !known {
		return "" // no salió de este bosque: no hay forma de saber quién lo lanzó
	}
	want := si.born.parentImage()
	par, alive := f.Parent(p.ID)
	switch {
	case !alive && si.born == bornOfSmss:
		return "" // lo esperado: el smss que lo lanzó ya salió
	case !alive:
		return "no tiene padre vivo; al de Windows lo lanza " + want
	case !strings.EqualFold(par.Name, want):
		return "lo lanzó " + procLabel(par) + "; al de Windows lo lanza " + want
	case !trusted(par, f, systemRoot, depth-1):
		return "lo lanzó un " + want + " que tampoco es el de Windows (PID " + itoa(par.PID) + ")"
	}
	return ""
}

// trusted: el proceso lleva un nombre de Windows y nada lo desmiente.
func trusted(p Proc, f *Forest, systemRoot string, depth int) bool {
	s, _ := standingOf(p, f, systemRoot, depth)
	return s.Trusted()
}

func parentIn(f *Forest, p Proc) (Proc, bool) {
	if f == nil {
		return Proc{}, false
	}
	return f.Parent(p.ID)
}

// parentText nombra al padre de p para un motivo: "explorer.exe (PID 812)",
// o "el PID 812, que ya no existe".
func parentText(f *Forest, p Proc) string {
	if par, ok := parentIn(f, p); ok {
		return procLabel(par)
	}
	if p.Parent == 0 {
		return "desconocido"
	}
	return "el PID " + itoa(p.Parent) + ", que ya no existe"
}

// system32 es la carpeta System32 de esa instalación de Windows.
func system32(systemRoot string) string {
	return strings.TrimRight(strings.ReplaceAll(systemRoot, "/", `\`), `\`) + `\System32`
}

// atSystem32 dice si path es EXACTAMENTE el archivo name de System32 (o de
// SysWOW64, si wow64). Sin distinguir mayúsculas (NTFS no las distingue) y
// tolerando el prefijo de ruta larga.
func atSystem32(path, systemRoot, name string, wow64 bool) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "/", `\`))
		return strings.TrimPrefix(s, `\\?\`)
	}
	got := norm(path)
	root := strings.TrimRight(norm(systemRoot), `\`)
	file := `\` + strings.ToLower(strings.TrimSpace(name))
	if got == root+`\system32`+file {
		return true
	}
	return wow64 && got == root+`\syswow64`+file
}
