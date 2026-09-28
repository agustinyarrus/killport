package killport

// La gramática del trabajo elevado. Lo que se prueba acá es lo que lee un
// proceso con token de administrador de sus propios argumentos: cada prueba
// es una forma de decir "y esto tampoco pasa".

import (
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
)

var horaTrabajo = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func reglaDe(t *testing.T, kind ports.ProxyKind, name, data string) ports.ProxyRule {
	t.Helper()
	r, err := ports.ParseProxyRule(kind, name, data)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func trabajoCompleto(t *testing.T) trabajo {
	return trabajo{
		Clase:     trabajoCierre,
		Puertos:   ports.NewSet(ports.R(3000, 3000), ports.R(5000, 5003)),
		Tree:      true,
		TCP:       true,
		AllStates: true,
		Timeout:   4500 * time.Millisecond,
		Kill:      []reaper.ID{{PID: 100, Created: horaTrabajo}, {PID: 200, Created: horaTrabajo.Add(5 * time.Millisecond)}, {PID: 4294967295}},
		Servicios: []svcObjetivo{
			{Nombre: "postgresql-x64-16", Owner: reaper.ID{PID: 300, Created: horaTrabajo}},
			// Un nombre con todo lo que un servicio puede tener y un trabajo no.
			{Nombre: "MSSQL$SQLEXPRESS", Owner: reaper.ID{PID: 301, Created: horaTrabajo}},
			{Nombre: "Servicio de Ñandú; con=cosas,raras|@", Owner: reaper.ID{PID: 302, Created: horaTrabajo}},
		},
		Manual: []string{"MSSQL$SQLEXPRESS"},
		Reglas: []ports.ProxyRule{
			reglaDe(t, ports.V4ToV4, "0.0.0.0/3000", "172.24.1.5/3000"),
			reglaDe(t, ports.V6ToV4, "::/5001", "localhost/80"),
			reglaDe(t, ports.V4ToV6, "*/5002", "fd00::7/8080"),
		},
	}
}

func TestTrabajoIdaYVuelta(t *testing.T) {
	job := trabajoCompleto(t)
	texto, err := codificarJob(job)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(texto); i++ {
		if !alfabetoTrabajo(texto[i]) || texto[i] <= ' ' || texto[i] == '"' || texto[i] == '\\' {
			t.Fatalf("el trabajo tiene %q en la posición %d: %s", texto[i], i, texto)
		}
	}
	got, err := decodificarJob(texto)
	if err != nil {
		t.Fatalf("%v\n%s", err, texto)
	}
	for i := range job.Kill {
		job.Kill[i] = job.Kill[i].Canon()
	}
	for i := range job.Servicios {
		job.Servicios[i].Owner = job.Servicios[i].Owner.Canon()
	}
	if !got.Puertos.Equal(job.Puertos) {
		t.Errorf("puertos = %v", got.Puertos)
	}
	got.Puertos, job.Puertos = ports.Set{}, ports.Set{}
	if !reflect.DeepEqual(got, job) {
		t.Errorf("volvió\n  %+v\nesperaba\n  %+v", got, job)
	}
	// Canónico: volver a escribir lo leído da el mismo texto.
	job.Puertos = ports.NewSet(ports.R(3000, 3000), ports.R(5000, 5003))
	got.Puertos = job.Puertos
	if otra, err := codificarJob(got); err != nil || otra != texto {
		t.Errorf("no es canónico:\n  %s\n  %s (%v)", texto, otra, err)
	}
}

func TestTrabajoConfigIdaYVuelta(t *testing.T) {
	plan := planificarConfig(estadoRed{WinnatCorriendo: true}, pedidoConfig{Reservar: ports.Of(3000), Dinamico: true, UDP: true})
	job := trabajo{Clase: trabajoConfig, Pedido: pedidoConfig{Reservar: ports.Of(3000), Dinamico: true, UDP: true}, Huella: plan.huella()}
	texto, err := codificarJob(job)
	if err != nil {
		t.Fatal(err)
	}
	if want := "v2;do=config;res=3000;dyn=1;proto=01;huella=" + plan.huella(); texto != want {
		t.Errorf("texto = %s, esperaba %s", texto, want)
	}
	got, err := decodificarJob(texto)
	if err != nil {
		t.Fatal(err)
	}
	if got.Clase != trabajoConfig || !got.Pedido.Reservar.Equal(ports.Of(3000)) || !got.Pedido.Liberar.IsEmpty() || !got.Pedido.Dinamico ||
		got.Pedido.TCP || !got.Pedido.UDP || got.Huella != job.Huella {
		t.Errorf("volvió %+v", got)
	}
	// Por los argumentos no viaja ningún paso: el texto no tiene ni un comando.
	for _, malo := range []string{"netsh", "winnat", "excludedportrange", "stop"} {
		if strings.Contains(texto, malo) {
			t.Errorf("el trabajo nombra %q: %s", malo, texto)
		}
	}
}

// Lo que NO es un trabajo. Cada caso parte de uno válido y le cambia una cosa.
func TestTrabajoRechaza(t *testing.T) {
	const base = "v2;do=cierre;ports=3000;flags=00000;to=3000;kill=100:5"
	if _, err := decodificarJob(base); err != nil {
		t.Fatalf("el caso base tiene que ser válido: %v", err)
	}
	huella := strings.Repeat("ab", 16)
	const svc = ";svc=TXlTUUw4MA@300:5" // MySQL80
	casos := map[string]string{
		"vacío":                           "",
		"otra versión":                    strings.Replace(base, "v2", "v1", 1),
		"sin versión":                     strings.TrimPrefix(base, "v2;"),
		"la versión no va primera":        "do=cierre;v2;ports=3000;flags=00000;to=3000;kill=100:5",
		"sin clase":                       strings.Replace(base, "do=cierre;", "", 1),
		"clase desconocida":               strings.Replace(base, "do=cierre", "do=todo", 1),
		"un campo que no existe":          base + ";cmd=calc",
		"un campo de la otra clase":       base + ";dyn=1",
		"un campo repetido":               base + ";kill=200:5",
		"la clase repetida":               base + ";do=config",
		"un campo sin valor":              base + ";manual",
		"una clave vacía":                 base + ";=x",
		"sin puertos":                     strings.Replace(base, "ports=3000;", "", 1),
		"puertos vacíos":                  strings.Replace(base, "ports=3000", "ports=", 1),
		"puertos con un nombre":           strings.Replace(base, "ports=3000", "ports=localhost:3000", 1),
		"puertos fuera de rango":          strings.Replace(base, "ports=3000", "ports=70000", 1),
		"puertos sin normalizar":          strings.Replace(base, "ports=3000", "ports=3001,3000", 1),
		"puertos adyacentes sin fundir":   strings.Replace(base, "ports=3000", "ports=3000,3001", 1),
		"el puerto cero":                  strings.Replace(base, "ports=3000", "ports=0", 1),
		"sin flags":                       strings.Replace(base, "flags=00000;", "", 1),
		"flags de más":                    strings.Replace(base, "flags=00000", "flags=000001", 1),
		"flags de menos":                  strings.Replace(base, "flags=00000", "flags=0000", 1),
		"flags que no son bits":           strings.Replace(base, "flags=00000", "flags=0000x", 1),
		"sin plazo":                       strings.Replace(base, "to=3000;", "", 1),
		"plazo negativo":                  strings.Replace(base, "to=3000", "to=-1", 1),
		"plazo enorme":                    strings.Replace(base, "to=3000", "to=3600001", 1),
		"plazo con ceros de más":          strings.Replace(base, "to=3000", "to=03000", 1),
		"plazo que no es un número":       strings.Replace(base, "to=3000", "to=3s", 1),
		"nada que hacer":                  strings.Replace(base, ";kill=100:5", "", 1),
		"kill vacío":                      strings.Replace(base, "kill=100:5", "kill=", 1),
		"kill sin hora":                   strings.Replace(base, "kill=100:5", "kill=100", 1),
		"kill al kernel":                  strings.Replace(base, "kill=100:5", "kill=4:0", 1),
		"kill al PID 0":                   strings.Replace(base, "kill=100:5", "kill=0:0", 1),
		"kill con PID que no entra":       strings.Replace(base, "kill=100:5", "kill=4294967296:5", 1),
		"kill con PID negativo":           strings.Replace(base, "kill=100:5", "kill=-100:5", 1),
		"kill con hora negativa":          strings.Replace(base, "kill=100:5", "kill=100:-5", 1),
		"kill repetido":                   strings.Replace(base, "kill=100:5", "kill=100:5,100:6", 1),
		"kill con un hueco":               strings.Replace(base, "kill=100:5", "kill=100:5,,200:5", 1),
		"kill con ceros de más":           strings.Replace(base, "kill=100:5", "kill=0100:5", 1),
		"servicio sin dueño":              base + ";svc=TXlTUUw4MA",
		"servicio con dos dueños":         base + ";svc=TXlTUUw4MA@300:5,301:5",
		"servicio sin codificar":          base + ";svc=MySQL80!@300:5",
		"servicio con relleno":            base + ";svc=TXlTUUw4MA==@300:5",
		"servicio vacío":                  base + ";svc=@300:5",
		"servicio con una barra":          base + ";svc=" + b64(`a\b`) + "@300:5",
		"servicio con un salto de línea":  base + ";svc=" + b64("a\nb") + "@300:5",
		"servicio con un cero":            base + ";svc=" + b64("a\x00b") + "@300:5",
		"servicio que no es UTF-8":        base + ";svc=" + b64("a\xffb") + "@300:5",
		"servicio con espacios al borde":  base + ";svc=" + b64(" MySQL80") + "@300:5",
		"servicio larguísimo":             base + ";svc=" + b64(strings.Repeat("x", 257)) + "@300:5",
		"servicio repetido":               base + svc + ",TVlTUUw4MA@301:5", // MYSQL80: el mismo, con otras mayúsculas
		"manual de un servicio ajeno":     base + svc + ";manual=" + b64("Spooler"),
		"manual sin servicios":            base + ";manual=TXlTUUw4MA",
		"manual repetido":                 base + svc + ";manual=TXlTUUw4MA,TXlTUUw4MA",
		"regla con campos de menos":       base + ";rule=v4tov4|0.0.0.0|3000",
		"regla con campos de más":         base + ";rule=v4tov4|0.0.0.0|3000|1.2.3.4/80|x",
		"regla de otra clase":             base + ";rule=v4tov5|0.0.0.0|3000|1.2.3.4/80",
		"regla con la clase en mayúscula": base + ";rule=V4TOV4|0.0.0.0|3000|1.2.3.4/80",
		"regla de otro puerto":            base + ";rule=v4tov4|0.0.0.0|3001|1.2.3.4/80",
		"regla con el puerto cero":        base + ";rule=v4tov4|0.0.0.0|0|1.2.3.4/80",
		"regla sin dirección":             base + ";rule=v4tov4||3000|1.2.3.4/80",
		"regla sin destino":               base + ";rule=v4tov4|0.0.0.0|3000|",
		"regla repetida":                  base + ";rule=v4tov4|0.0.0.0|3000|1.2.3.4/80,v4tov4|0.0.0.0|3000|1.2.3.5/80",
		"un espacio":                      strings.Replace(base, ";to=", "; to=", 1),
		"comillas":                        base + `;svc="TXlTUUw4MA"@300:5`,
		"una barra invertida":             base + `;rule=v4tov4|0.0.0.0|3000|c:\x`,
		"un salto de línea":               base + "\n",
		"un cero":                         base + "\x00",
		"otro argumento colado":           base + " --force",
		"demasiado largo":                 base + ";svc=" + strings.Repeat("A", maxTrabajo),

		"config sin huella":              "v2;do=config;res=3000;proto=00",
		"config con huella corta":        "v2;do=config;res=3000;proto=00;huella=abcd",
		"config con huella en mayúscula": "v2;do=config;res=3000;proto=00;huella=" + strings.ToUpper(huella),
		"config sin protocolos":          "v2;do=config;res=3000;huella=" + huella,
		"config con tres protocolos":     "v2;do=config;res=3000;proto=001;huella=" + huella,
		"config sin nada que hacer":      "v2;do=config;proto=00;huella=" + huella,
		"config con las dos cosas":       "v2;do=config;res=3000;unres=3001;proto=00;huella=" + huella,
		"config con demasiados puertos":  "v2;do=config;res=1-2000;proto=00;huella=" + huella,
		"config con dyn que no es 1":     "v2;do=config;dyn=si;proto=00;huella=" + huella,
		"config con dyn en cero":         "v2;do=config;dyn=0;proto=00;huella=" + huella,
		"config con procesos":            "v2;do=config;res=3000;proto=00;kill=100:5;huella=" + huella,
		"config con un paso":             "v2;do=config;res=3000;proto=00;paso=netsh;huella=" + huella,
	}
	for nombre, texto := range casos {
		if job, err := decodificarJob(texto); err == nil {
			t.Errorf("%s: se aceptó %q → %+v", nombre, texto, job)
		}
	}
}

// Los topes: justo en el tope, sí; uno más, no.
func TestTrabajoTopes(t *testing.T) {
	armar := func(n int) trabajo {
		job := trabajo{Clase: trabajoCierre, Puertos: ports.Of(3000), Timeout: time.Second}
		for i := 0; i < n; i++ {
			job.Kill = append(job.Kill, reaper.ID{PID: uint32(100 + i), Created: horaTrabajo})
		}
		return job
	}
	if _, err := codificarJob(armar(maxCerrar)); err != nil {
		t.Errorf("en el tope: %v", err)
	}
	if s, err := codificarJob(armar(maxCerrar + 1)); err == nil {
		t.Errorf("pasado el tope se codificó: %s", s)
	}
	// Un trabajo que no cumple su gramática no sale: mejor no elevar que elevar
	// para que el hijo lo rechace.
	for nombre, job := range map[string]trabajo{
		"sin clase":                {Puertos: ports.Of(3000), Kill: []reaper.ID{{PID: 100}}},
		"sin puertos":              {Clase: trabajoCierre, Kill: []reaper.ID{{PID: 100}}, Timeout: time.Second},
		"al kernel":                {Clase: trabajoCierre, Puertos: ports.Of(445), Kill: []reaper.ID{{PID: 4}}, Timeout: time.Second},
		"manual sin el servicio":   {Clase: trabajoCierre, Puertos: ports.Of(3306), Kill: []reaper.ID{{PID: 100}}, Manual: []string{"MySQL80"}, Timeout: time.Second},
		"config sin huella":        {Clase: trabajoConfig, Pedido: pedidoConfig{Reservar: ports.Of(3000)}},
		"config sin pedido":        {Clase: trabajoConfig, Huella: strings.Repeat("0", 32)},
		"plazo de más de una hora": {Clase: trabajoCierre, Puertos: ports.Of(3000), Kill: []reaper.ID{{PID: 100}}, Timeout: 2 * time.Hour},
	} {
		if s, err := codificarJob(job); err == nil {
			t.Errorf("%s: se codificó %s", nombre, s)
		}
	}
	// El plazo cero vale: es "el de siempre" (lo resuelve el motor).
	if _, err := codificarJob(trabajo{Clase: trabajoCierre, Puertos: ports.Of(3000), Kill: []reaper.ID{{PID: 100}}}); err != nil {
		t.Errorf("plazo cero: %v", err)
	}
}

// valido revisa que un trabajo decodificado cumpla, campo por campo, todo lo
// que la gramática promete.
func valido(t *testing.T, texto string, job trabajo) {
	t.Helper()
	falla := func(f string, a ...any) {
		t.Helper()
		t.Fatalf("de %q salió un trabajo inválido: "+f+"\n%+v", append(append([]any{texto}, a...), job)...)
	}
	switch job.Clase {
	case trabajoCierre:
		if job.Puertos.IsEmpty() || !job.Pedido.vacio() || job.Huella != "" {
			falla("cierre sin puertos o con pedido")
		}
		if job.Timeout < 0 || job.Timeout > maxEspera {
			falla("plazo %v", job.Timeout)
		}
		if len(job.Kill) > maxCerrar || len(job.Servicios) > maxServicios || len(job.Reglas) > maxReglas || len(job.Kill)+len(job.Servicios)+len(job.Reglas) == 0 {
			falla("cantidades")
		}
		for _, id := range job.Kill {
			if id.PID <= 4 {
				falla("nombra al PID %d", id.PID)
			}
		}
		nombres := map[string]bool{}
		for _, s := range job.Servicios {
			if !nombreServicioValido(s.Nombre) || s.Owner.PID <= 4 {
				falla("servicio %q", s.Nombre)
			}
			nombres[strings.ToLower(s.Nombre)] = true
		}
		for _, m := range job.Manual {
			if !nombres[strings.ToLower(m)] {
				falla("manual de %q, que no es de los servicios", m)
			}
		}
		for _, r := range job.Reglas {
			if !job.Puertos.Contains(r.ListenPort) {
				falla("regla del %d, que no se pidió", r.ListenPort)
			}
			if _, err := ports.ParseProxyRule(r.Kind, r.ListenAddr+"/"+u32Texto(uint32(r.ListenPort)), r.Target); err != nil {
				falla("regla %v: %v", r, err)
			}
		}
	case trabajoConfig:
		if err := job.Pedido.validar(); err != nil {
			falla("pedido: %v", err)
		}
		if len(job.Huella) != 32 || len(job.Kill)+len(job.Servicios)+len(job.Reglas)+len(job.Manual) != 0 || !job.Puertos.IsEmpty() {
			falla("config con cosas de cierre")
		}
	default:
		falla("sin clase")
	}
}

func u32Texto(n uint32) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// Propiedad: se parte de trabajos válidos y se los rompe de a un carácter
// (cambiar, borrar, insertar, duplicar un campo). Lo que sale, o se rechaza, o
// es un trabajo que cumple toda la gramática. No hay un tercer resultado: un
// texto a medio entender que termina en una acción.
func TestTrabajoPropiedadMutaciones(t *testing.T) {
	rng := rand.New(rand.NewPCG(20260927, 6))
	completo, err := codificarJob(trabajoCompleto(t))
	if err != nil {
		t.Fatal(err)
	}
	plan := planificarConfig(estadoRed{}, pedidoConfig{Liberar: ports.NewSet(ports.R(3000, 3010)), Dinamico: true, TCP: true})
	config, err := codificarJob(trabajo{Clase: trabajoConfig, Pedido: pedidoConfig{Liberar: ports.NewSet(ports.R(3000, 3010)), Dinamico: true, TCP: true}, Huella: plan.huella()})
	if err != nil {
		t.Fatal(err)
	}
	semillas := []string{completo, config, "v2;do=cierre;ports=3000;flags=00000;to=3000;kill=100:5"}
	alfabeto := "abzAZ019_.:,;=@|*/- \"\\\x00\n+%&"
	aceptados, rechazados := 0, 0
	for iter := 0; iter < 60000; iter++ {
		b := []byte(semillas[rng.IntN(len(semillas))])
		for n := 1 + rng.IntN(3); n > 0 && len(b) > 0; n-- {
			i := rng.IntN(len(b))
			switch rng.IntN(5) {
			case 0:
				b[i] = alfabeto[rng.IntN(len(alfabeto))]
			case 1:
				b = append(b[:i], b[i+1:]...)
			case 2:
				b = append(b[:i], append([]byte{alfabeto[rng.IntN(len(alfabeto))]}, b[i:]...)...)
			case 3: // duplicar o mover un campo entero
				campos := strings.Split(string(b), ";")
				j, k := rng.IntN(len(campos)), rng.IntN(len(campos))
				campos = append(campos, campos[j])
				campos[k], campos[len(campos)-1] = campos[len(campos)-1], campos[k]
				b = []byte(strings.Join(campos[:len(campos)-rng.IntN(2)], ";"))
			default: // incrementar un dígito
				if b[i] >= '0' && b[i] < '9' {
					b[i]++
				}
			}
		}
		texto := string(b)
		job, err := decodificarJob(texto)
		if err != nil {
			rechazados++
			continue
		}
		aceptados++
		valido(t, texto, job)
		// Y lo aceptado se vuelve a escribir igual: no hay dos textos para el
		// mismo trabajo, ni un texto que se lea de dos maneras.
		if otra, err := codificarJob(job); err != nil || otra != texto {
			t.Fatalf("se aceptó %q y al volver a escribirlo dio %q (%v)", texto, otra, err)
		}
	}
	if aceptados < 500 || rechazados < 10000 {
		t.Fatalf("la propiedad casi no se ejerció: %d aceptados, %d rechazados", aceptados, rechazados)
	}
	t.Logf("%d mutaciones aceptadas (todas válidas y canónicas), %d rechazadas", aceptados, rechazados)
}

func TestNombreServicio(t *testing.T) {
	for _, n := range []string{"MySQL80", "postgresql-x64-16", "MSSQL$SQLEXPRESS", "Apple Mobile Device Service", "Ñandú", "a", strings.Repeat("x", 256)} {
		if !nombreServicioValido(n) {
			t.Errorf("%q es un nombre de servicio", n)
		}
	}
	for _, n := range []string{"", " x", "x ", "a/b", `a\b`, "a\tb", "a\nb", "a\x00b", "a\x7fb", "a\xffb", strings.Repeat("x", 257)} {
		if nombreServicioValido(n) {
			t.Errorf("%q no es un nombre de servicio", n)
		}
	}
}

// La ruta del archivo de resultado: el proceso elevado escribe solo en sus
// carpetas y con un nombre que no elige nadie.
func TestRutaResultado(t *testing.T) {
	carpetas := []string{`C:\Users\mauro\dev\_pruebas\killport`, `C:\Users\mauro\AppData\Local\Temp`}
	for _, ruta := range []string{
		`C:\Users\mauro\dev\_pruebas\killport\resultado-1234-1790000000000000000.json`,
		`c:\users\MAURO\dev\_pruebas\KILLPORT\resultado-1-1.json`, // NTFS no distingue mayúsculas
		`C:\Users\mauro\AppData\Local\Temp\resultado-4294967295-4611686018427387904.json`,
		`C:\Users\mauro\AppData\Local\Temp\TestAlgo123\001\resultado-1-1.json`, // debajo de una permitida
		`C:\Users\mauro\AppData\Local\Temp\con espacios y ñ\resultado-1-1.json`,
	} {
		if !rutaResultadoValida(ruta, carpetas) {
			t.Errorf("%q es una ruta de resultado válida", ruta)
		}
	}
	if got := nombreResultado(1234, 1790000000000000000); got != "resultado-1234-1790000000000000000.json" || !nombreResultadoValido(got) {
		t.Errorf("nombreResultado = %q", got)
	}
	largo := `C:\Users\mauro\AppData\Local\Temp\` + strings.Repeat(`x\`, 600) + `resultado-1-1.json`
	for nombre, ruta := range map[string]string{
		"vacía":                            "",
		"otra carpeta":                     `C:\Windows\System32\resultado-1-1.json`,
		"el inicio de todos los usuarios":  `C:\ProgramData\Microsoft\Windows\Start Menu\Programs\StartUp\resultado-1-1.json`,
		"la carpeta de al lado":            `C:\Users\mauro\dev\_pruebas\killport2\resultado-1-1.json`,
		"la carpeta de arriba":             `C:\Users\mauro\dev\_pruebas\resultado-1-1.json`,
		"el prefijo sin la barra":          `C:\Users\mauro\AppData\Local\Temporal\resultado-1-1.json`,
		"la raíz de la unidad":             `C:\resultado-1-1.json`,
		"otra unidad":                      `D:\Users\mauro\AppData\Local\Temp\resultado-1-1.json`,
		"saliendo con ..":                  `C:\Users\mauro\AppData\Local\Temp\..\..\..\..\..\Windows\resultado-1-1.json`,
		"con un . en el medio":             `C:\Users\mauro\AppData\Local\Temp\.\resultado-1-1.json`,
		"barras al derecho":                `C:/Users/mauro/AppData/Local/Temp/resultado-1-1.json`,
		"una barra al derecho":             `C:\Users\mauro\AppData\Local\Temp/resultado-1-1.json`,
		"doble barra":                      `C:\Users\mauro\AppData\Local\Temp\\resultado-1-1.json`,
		"termina en barra":                 `C:\Users\mauro\AppData\Local\Temp\resultado-1-1.json\`,
		"relativa":                         `resultado-1-1.json`,
		"relativa a la unidad":             `C:resultado-1-1.json`,
		"sin unidad":                       `\Users\mauro\AppData\Local\Temp\resultado-1-1.json`,
		"de red":                           `\\servidor\c$\Users\mauro\AppData\Local\Temp\resultado-1-1.json`,
		"ruta larga":                       `\\?\C:\Users\mauro\AppData\Local\Temp\resultado-1-1.json`,
		"un dispositivo":                   `\\.\C:\Users\mauro\AppData\Local\Temp\resultado-1-1.json`,
		"un flujo alternativo":             `C:\Users\mauro\AppData\Local\Temp\resultado-1-1.json:oculto`,
		"un flujo en la carpeta":           `C:\Users\mauro\AppData\Local\Temp:x\resultado-1-1.json`,
		"termina en punto":                 `C:\Users\mauro\AppData\Local\Temp\resultado-1-1.json.`,
		"termina en espacio":               `C:\Users\mauro\AppData\Local\Temp\resultado-1-1.json `,
		"una carpeta que termina en punto": `C:\Users\mauro\AppData\Local\Temp.\resultado-1-1.json`,
		"otra extensión":                   `C:\Users\mauro\AppData\Local\Temp\resultado-1-1.bat`,
		"dos extensiones":                  `C:\Users\mauro\AppData\Local\Temp\resultado-1-1.json.bat`,
		"sin extensión":                    `C:\Users\mauro\AppData\Local\Temp\resultado-1-1`,
		"la extensión en mayúsculas":       `C:\Users\mauro\AppData\Local\Temp\resultado-1-1.JSON`,
		"otro nombre":                      `C:\Users\mauro\AppData\Local\Temp\informe.json`,
		"el nombre con algo adelante":      `C:\Users\mauro\AppData\Local\Temp\xresultado-1-1.json`,
		"sin el PID":                       `C:\Users\mauro\AppData\Local\Temp\resultado--1.json`,
		"sin la hora":                      `C:\Users\mauro\AppData\Local\Temp\resultado-1-.json`,
		"PID 0":                            `C:\Users\mauro\AppData\Local\Temp\resultado-0-1.json`,
		"un PID que no entra en 32 bits":   `C:\Users\mauro\AppData\Local\Temp\resultado-4294967296-1.json`,
		"ceros de más":                     `C:\Users\mauro\AppData\Local\Temp\resultado-01-1.json`,
		"un signo":                         `C:\Users\mauro\AppData\Local\Temp\resultado-1-+1.json`,
		"un comodín":                       `C:\Users\mauro\AppData\Local\Temp\resultado-1-*.json`,
		"un carácter de control":           "C:\\Users\\mauro\\AppData\\Local\\Temp\\resultado-1-1.json\x00.bat",
		"un salto de línea":                "C:\\Users\\mauro\\AppData\\Local\\Temp\\x\n\\resultado-1-1.json",
		"UTF-8 roto":                       "C:\\Users\\mauro\\AppData\\Local\\Temp\\\xff\\resultado-1-1.json",
		"demasiado larga":                  largo,
	} {
		if rutaResultadoValida(ruta, carpetas) {
			t.Errorf("%s: %q pasó por válida", nombre, ruta)
		}
	}
	// Sin carpetas permitidas (no se pudo saber cuáles son), ninguna ruta vale.
	// Y una "carpeta permitida" que es una raíz, o está vacía, no permite nada.
	buena := `C:\Users\mauro\AppData\Local\Temp\resultado-1-1.json`
	for _, cs := range [][]string{nil, {""}, {`C:\`}, {`C:`}, {`\`}} {
		if rutaResultadoValida(buena, cs) {
			t.Errorf("con las carpetas %q, %q pasó por válida", cs, buena)
		}
	}

	// Propiedad: se rompe una ruta válida de a un carácter. Lo que sigue
	// pasando por válido está, sí o sí, debajo de una carpeta permitida, es un
	// .json con el nombre de siempre y no tiene por dónde salirse.
	rng := rand.New(rand.NewPCG(20260927, 7))
	alfabeto := `abC019_.:-\/ *?"<>|` + "\x00\n\t"
	validas := 0
	for iter := 0; iter < 40000; iter++ {
		b := []byte(`C:\Users\mauro\AppData\Local\Temp\a\resultado-12-345.json`)
		for n := 1 + rng.IntN(3); n > 0 && len(b) > 0; n-- {
			i := rng.IntN(len(b))
			switch rng.IntN(3) {
			case 0:
				b[i] = alfabeto[rng.IntN(len(alfabeto))]
			case 1:
				b = append(b[:i], b[i+1:]...)
			default:
				b = append(b[:i], append([]byte{alfabeto[rng.IntN(len(alfabeto))]}, b[i:]...)...)
			}
		}
		ruta := string(b)
		if !rutaResultadoValida(ruta, carpetas) {
			continue
		}
		validas++
		low := strings.ToLower(ruta)
		i := strings.LastIndexByte(ruta, '\\')
		switch {
		case !strings.HasPrefix(low, `c:\users\mauro\appdata\local\temp\`),
			!strings.HasSuffix(ruta, ".json"),
			!strings.HasPrefix(ruta[i+1:], "resultado-"),
			strings.Contains(ruta, ".."), strings.ContainsAny(ruta, "/*?\"<>|\x00\n\t"),
			strings.Count(ruta, ":") != 1, strings.Contains(ruta, `\\`),
			strings.Contains(ruta, `.\`), strings.Contains(ruta, ` \`):
			t.Fatalf("pasó por válida %q", ruta)
		}
	}
	if validas < 200 {
		t.Fatalf("la propiedad casi no se ejerció: %d rutas válidas", validas)
	}
}
