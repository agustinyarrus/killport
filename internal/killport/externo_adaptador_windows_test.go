//go:build windows

package killport

// El adaptador de los objetivos externos (sistema.StopExternal) y lo que se
// muestra de ellos: el listado anotado y el JSON. Con el motor falso por pipe
// y el wsl.exe falso de las otras pruebas.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
	"github.com/agustinyarrus/killport/internal/win/pipetest"
)

// goroutineID: el número de la goroutine actual (solo para verificar desde
// dónde se llama a done).
func goroutineID() string {
	b := make([]byte, 64)
	b = b[:runtime.Stack(b, false)]
	return string(bytes.Fields(b)[1])
}

func sistemaFalso(t *testing.T, m *motorFalso, f *wslFalso) *sistema {
	t.Helper()
	s := &sistema{docker: dockerDe(), wsl: &wsl{run: f.run}}
	if m != nil {
		s.docker = dockerDe(m.path)
	}
	return s
}

// Todos a la vez, un veredicto por externo apenas llega, SIEMPRE desde la
// goroutine que llamó: el proceso Linux (instantáneo) llega antes que el
// contenedor (que tarda 400 ms), aunque se pidió después.
func TestStopExternalConcurrente(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{})
	m.stopDelay = 400 * time.Millisecond
	f := &wslFalso{running: []string{"Ubuntu"}, cierre: "@fin INT\n"}
	sys := sistemaFalso(t, m, f)
	sys.docker.consultar(context.Background(), reaper.NewForest(nil))
	c := sys.docker.publican(5432, ports.TCP)[0]
	p := &procesoLinux{Distro: "Ubuntu", PID: 812, Arranque: 523400, Comm: "node"}
	xs := []reaper.External{
		{Kind: reaper.ExtContainer, Key: c.ID, Name: c.Nombre, Ref: &c},
		{Kind: reaper.ExtLinux, Key: "Ubuntu/812@523400", Name: "node", Ref: p},
	}
	yo := goroutineID()
	var orden []int
	var res [2]reaper.ExternalStop
	start := time.Now()
	sys.StopExternal(context.Background(), xs, time.Now().Add(3*time.Second), false, func(i int, r reaper.ExternalStop) {
		if g := goroutineID(); g != yo {
			t.Errorf("done se llamó desde la goroutine %s, no desde la del motor (%s)", g, yo)
		}
		orden = append(orden, i)
		res[i] = r
	})
	if len(orden) != 2 || orden[0] != 1 || orden[1] != 0 {
		t.Fatalf("orden de llegada = %v, esperaba [1 0]", orden)
	}
	if res[0].Outcome != reaper.OutcomeClosed || res[0].Method != reaper.MethodEngine || res[1].Method != reaper.MethodSigInt {
		t.Fatalf("veredictos = %+v", res)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("tardó %v: no fueron a la vez", d)
	}
}

// El usuario cancela mientras el motor todavía está deteniendo: StopExternal
// vuelve al toque y NO da veredicto de lo que no terminó.
func TestStopExternalCancelado(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{})
	m.stopDelay = 5 * time.Second
	sys := sistemaFalso(t, m, &wslFalso{})
	sys.docker.consultar(context.Background(), reaper.NewForest(nil))
	c := sys.docker.publican(5432, ports.TCP)[0]
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	llamado := false
	sys.StopExternal(ctx, []reaper.External{{Kind: reaper.ExtContainer, Key: c.ID, Ref: &c}}, time.Now().Add(3*time.Second), false,
		func(int, reaper.ExternalStop) { llamado = true })
	if llamado {
		t.Fatal("no debía dar veredicto de algo que no terminó")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("tardó %v en volver tras cancelar", d)
	}
}

// El listado: la fila de Docker Desktop dice qué contenedor es; con --wsl, la
// de wslrelay dice qué proceso Linux escucha (con SU PID) y lo que escucha
// solo en WSL va aparte; sin --wsl, no se corre wsl.exe.
func TestAnotarListado(t *testing.T) {
	m := nuevoMotorFalso(t, pipetest.Options{})
	f := &wslFalso{running: []string{"Ubuntu"}, sondeo: map[string]string{"Ubuntu": string(grabado(t, "wsl_sondeo_ss.txt"))}}
	forest := reaper.NewForest([]reaper.Proc{
		{ID: reaper.ID{PID: 8124, Created: t0}, Name: "com.docker.backend.exe", Path: `C:\Program Files\Docker\Docker\resources\com.docker.backend.exe`},
		{ID: reaper.ID{PID: 9120, Created: t0}, Name: "wslrelay.exe", Path: `C:\Program Files\WSL\wslrelay.exe`},
	})
	socks := []ports.Socket{
		sock(ports.TCP, "0.0.0.0", 5432, ports.Listen, 8124, "com.docker.backend.exe"),
		sock(ports.TCP, "127.0.0.1", 3000, ports.Listen, 9120, "wslrelay.exe"),
		sock(ports.TCP, "127.0.0.1", 5555, ports.Listen, 9120, "wslrelay.exe"),
	}
	cfg := reaper.Config{SystemRoot: `C:\Windows`}

	// Sin --wsl.
	sys := sistemaFalso(t, m, f)
	filas, solo, _ := anotarListado(context.Background(), sys, filasDeListado(socks, forest, nil, cfg), forest, opciones{})
	if len(solo) != 0 || len(f.llamadas) != 0 {
		t.Fatalf("sin --wsl no se mira adentro (%d filas, %d llamadas)", len(solo), len(f.llamadas))
	}
	by := map[uint16]fila{}
	for _, fl := range filas {
		by[fl.Port] = fl
	}
	// PROCESO es el contenedor; PROYECTO, su compose; COMANDO, el motor y la
	// imagen. El dato (Name, el JSON) sigue siendo el proceso de Windows.
	if db := by[5432]; nombreFila(db) != "shop-db-1" || db.Name != "com.docker.backend.exe" || proyectoFila(db) != "shop/db" ||
		db.Comando != "docker · postgres:16" || db.Via != "com.docker.backend.exe" || db.EsWindows {
		t.Fatalf("5432 = %+v", db)
	}
	// Sin --wsl, la pista empieza por la opción, y el recorte la respeta (X-23).
	if w := by[3000]; w.Name != "wslrelay.exe" || !strings.HasPrefix(w.Comando, "--wsl dice quién") || w.keep != tui.Width("--wsl dice quién") {
		t.Fatalf("3000 sin --wsl = %+v", w)
	}

	// Con --wsl.
	sys = sistemaFalso(t, m, f)
	var ocultosLinux int
	filas, solo, ocultosLinux = anotarListado(context.Background(), sys, filasDeListado(socks, forest, nil, cfg), forest, opciones{wsl: true})
	by = map[uint16]fila{}
	for _, fl := range filas {
		by[fl.Port] = fl
	}
	// El PID de la tabla es siempre de Windows (el de wslrelay); el de Linux va
	// en PROCESO, con su distro (X-21).
	if w := by[3000]; nombreFila(w) != "node (812) en Ubuntu" || pidFila(w) != 9120 || w.PID != 9120 || w.Comando != "node server.js --port 3000" || w.Linux == nil {
		t.Fatalf("3000 con --wsl = %+v", w)
	}
	if z := by[5555]; !strings.Contains(z.Comando, "relay colgado") {
		t.Fatalf("5555 = %+v", z)
	}
	var puertos []uint16
	for _, fl := range solo {
		if !fl.SoloWSL || fl.PID != 0 || pidFila(fl) != 0 || textoPID(pidFila(fl)) != "—" || fl.Linux == nil || fl.Linux.PID == 0 {
			t.Fatalf("fila solo-WSL mal armada: %+v", fl)
		}
		puertos = append(puertos, fl.Port)
	}
	// 3000 ya salió detrás del relay; el resto de lo que escucha en Ubuntu va
	// aparte. El 53 es de systemd-resolved, del sistema de la distro: no se
	// lista salvo --system (X-23), y se cuenta para el pie.
	if got := ports.Of(puertos...); !got.Equal(ports.Of(4000, 8000, 8080, 9229)) || ocultosLinux != 1 {
		t.Fatalf("solo en WSL = %v (ocultos del sistema: %d)", got, ocultosLinux)
	}
	sys = sistemaFalso(t, m, f)
	_, conSistema, ocultos := anotarListado(context.Background(), sys, filasDeListado(socks, forest, nil, cfg), forest, opciones{wsl: true, system: true})
	if len(conSistema) != len(solo)+1 || ocultos != 0 {
		t.Fatalf("con --system: %d filas (sin: %d), %d ocultos", len(conSistema), len(solo), ocultos)
	}

	// El JSON del listado lleva el contenedor, el proceso Linux y el relay.
	b, _ := json.Marshal(construirJSONListado(append(filas, solo...), sinMostrar{}))
	var doc struct {
		Escuchando []map[string]any `json:"escuchando"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var vioCont, vioWSL, vioSolo bool
	for _, e := range doc.Escuchando {
		if c, ok := e["contenedor"].(map[string]any); ok && c["id"] == idDB && c["nombre"] == "shop-db-1" && e["via"] == "com.docker.backend.exe" {
			vioCont = len(c["puertos"].([]any)) == 2 && e["intermediario"] == "contenedores"
		}
		if w, ok := e["wsl"].(map[string]any); ok && w["distro"] == "Ubuntu" && w["pid"].(float64) == 812 && e["pid"].(float64) == 9120 {
			vioWSL = e["intermediario"] == "wsl" && e["via"] == "wslrelay.exe"
		}
		if e["puerto"].(float64) == 5555 {
			// Relay colgado: nada detrás, pero el JSON igual dice que es un relay.
			if _, hay := e["via"]; hay || e["intermediario"] != "wsl" || e["proceso"] != "wslrelay.exe" {
				t.Fatalf("fila del relay colgado = %v", e)
			}
		}
		if e["solo_wsl"] == true {
			if _, hay := e["intermediario"]; hay {
				t.Fatalf("lo que escucha solo en WSL no tiene relay: %v", e)
			}
		}
		if e["solo_wsl"] == true && e["pid"].(float64) == 0 {
			vioSolo = true
		}
	}
	if !vioCont || !vioWSL || !vioSolo {
		t.Fatalf("JSON del listado incompleto (contenedor %v, wsl %v, solo %v):\n%s", vioCont, vioWSL, vioSolo, b)
	}
}

// El JSON del reporte: objetivos (con lo de atrás del relay), negativas (con
// la clase de intermediario) y cómo le fue a cada externo. Claves estables.
func TestJSONReporteConExternos(t *testing.T) {
	c := &contenedor{ID: idDB, Nombre: "shop-db-1", Imagen: "postgres:16", Reinicio: "always", Motor: &motor{Nombre: "Docker Desktop"},
		Puertos: []publicado{{IP: "0.0.0.0", Privado: 5432, Publico: 5432, Proto: ports.TCP}}}
	x := reaper.External{Kind: reaper.ExtContainer, Where: "Docker Desktop", Key: idDB, Name: "shop-db-1", Ports: ports.Of(5432), Ref: c}
	plan := reaper.Plan{
		Targets:  []reaper.Target{{Kind: reaper.StopExternal, Name: "shop-db-1", External: x, Ports: ports.Of(5432), Via: viaRelay, ViaName: "com.docker.backend.exe", ViaClass: reaper.RelayContainer, Caution: "vuelve solo"}},
		Refusals: []reaper.Refusal{{PID: 9120, Name: "wslrelay.exe", Ports: ports.Of(3000), Reason: "no pude mirar", Relay: reaper.RelayWSL}},
	}
	rep := reaper.Report{
		Externals: []reaper.ExternalResult{{External: x, Outcome: reaper.OutcomeClosed, Method: reaper.MethodEngine, Elapsed: 1200 * time.Millisecond}},
		Ports:     []reaper.PortResult{{Port: 5432, Status: reaper.PortFree}, {Port: 3000, Status: reaper.PortRefused, Reason: "no pude mirar"}},
	}
	b, err := json.Marshal(construirJSONReporte(rep, plan, false))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	o := doc["objetivos"].([]any)[0].(map[string]any)
	if o["tipo"] != "contenedor" || o["contenedor"].(map[string]any)["reinicio"] != "always" || o["via"].(map[string]any)["proceso"] != "com.docker.backend.exe" || o["cautela"] != "vuelve solo" {
		t.Fatalf("objetivo = %v", o)
	}
	if _, tienePID := o["pid"]; tienePID {
		t.Fatalf("un externo no tiene PID de Windows propio: %v", o)
	}
	n := doc["negativas"].([]any)[0].(map[string]any)
	if n["intermediario"] != "wsl" || n["pid"].(float64) != 9120 {
		t.Fatalf("negativa = %v", n)
	}
	e := doc["externos"].([]any)[0].(map[string]any)
	if e["tipo"] != "contenedor" || e["destino"] != "cerrado" || e["metodo"] != "stop del motor" || e["ms"].(float64) != 1200 || e["clave"] != idDB {
		t.Fatalf("externo = %v", e)
	}
	// Las claves de siempre siguen ahí.
	for _, k := range []string{"puertos", "victimas", "libres", "total", "cancelado", "solo_listado"} {
		if _, ok := doc[k]; !ok {
			t.Fatalf("falta la clave estable %q", k)
		}
	}
}

// Lo que netsh sabe de la red: las OCHO consultas (rangos excluidos y rango
// dinámico, por protocolo y familia) corren A LA VEZ —cada corrida falsa
// espera a que hayan arrancado las ocho: en serie se trabaría—, se unen en
// orden fijo sin importar cuál llega primero, y la lectura se memoiza.
func TestRedEnParalelo(t *testing.T) {
	excluidos := func(lo, n int) string {
		return "\nProtocolo tcp Intervalos de exclusión de puertos\n\nPuerto de inicio    Puerto final\n----------    --------\n" +
			"     " + u32(uint32(lo)) + "       " + u32(uint32(lo+n-1)) + "\n\n* - Exclusiones de puertos administrados.\n"
	}
	dinamico := func(inicio, n int) string {
		return "\nIntervalo de puerto dinámico de protocolo tcp\n------------------\nPuerto de inicio    : " + u32(uint32(inicio)) +
			"\nNúmero de puertos   : " + u32(uint32(n)) + "\n\n"
	}
	var mu sync.Mutex
	arrancaron, corridas := 0, 0
	todas := make(chan struct{})
	falla := ""
	var pedidos []string
	netsh := func(_ context.Context, args ...string) (string, error) {
		mu.Lock()
		arrancaron++
		corridas++
		pedidos = append(pedidos, strings.Join(args, " "))
		if arrancaron == 8 {
			close(todas)
		}
		mu.Unlock()
		select {
		case <-todas:
		case <-time.After(5 * time.Second):
			return "", errors.New("las ocho consultas no corrieron a la vez")
		}
		// interface ipvX show TABLA protocol=Y
		if len(args) != 5 || args[0] != "interface" || args[2] != "show" {
			return "", errors.New("pedido inesperado: " + strings.Join(args, " "))
		}
		if falla != "" && args[3] == falla && args[4] == "protocol=udp" {
			return "", &errorNetsh{Codigo: 1, Dijo: "El servicio no responde."}
		}
		lo := 50000
		if args[4] == "protocol=udp" {
			lo = 60000
		}
		if args[1] == "ipv6" {
			lo += 1000
			time.Sleep(20 * time.Millisecond) // las de v6 llegan después: el orden no puede depender de eso
		}
		if args[3] == "dynamicport" {
			return dinamico(lo-40000, 1000), nil
		}
		return excluidos(lo, 100), nil
	}
	s := &sistema{red: nuevaRed(netsh)}
	xs, err := s.Excluded()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range xs {
		got = append(got, x.Proto.String()+"/"+u32(uint32(x.Lo)))
	}
	if want := "TCP/50000 TCP/51000 UDP/60000 UDP/61000"; strings.Join(got, " ") != want {
		t.Fatalf("rangos = %v, esperaba %s", got, want)
	}
	if _, ok := xs.Find(ports.TCP, ports.IPv6, 51050); !ok {
		t.Fatal("el rango de TCP v6 no quedó en la tabla unida")
	}
	dyn, err := s.DynamicRanges()
	if err != nil || len(dyn) != 4 {
		t.Fatalf("rangos dinámicos = %v, %v", dyn, err)
	}
	for i, c := range combos {
		if dyn[i].Proto != c.proto || dyn[i].Family != c.fam {
			t.Fatalf("el rango dinámico %d es de %v %v, esperaba %v", i, dyn[i].Proto, dyn[i].Family, c)
		}
	}
	if d, _ := dyn.Find(ports.UDP, ports.IPv6); d.Start != 21000 || d.Count != 1000 {
		t.Fatalf("UDP v6 = %+v", d)
	}
	// Memoizado: pedir las dos tablas costó UNA lectura (ocho corridas).
	if corridas != 8 {
		t.Fatalf("netsh corrió %d veces, esperaba 8", corridas)
	}
	sort.Strings(pedidos)
	if pedidos[0] != "interface ipv4 show dynamicport protocol=tcp" || pedidos[7] != "interface ipv6 show excludedportrange protocol=udp" {
		t.Fatalf("pedidos = %q", pedidos)
	}

	// Una tabla ilegible invalida las exclusiones enteras (media tabla sería un
	// diagnóstico mentiroso) pero no el rango dinámico, y al revés. Hay que
	// olvidar la lectura para que se vuelva a leer.
	falla = "excludedportrange"
	if xs, _ := s.Excluded(); xs == nil {
		t.Fatal("sin olvidar, tiene que seguir devolviendo la lectura memoizada")
	}
	s.red.olvidar()
	arrancaron, todas = 0, make(chan struct{})
	xs, err = s.Excluded()
	if err == nil || xs != nil || !strings.Contains(err.Error(), "UDP IPv4") || !strings.Contains(err.Error(), "El servicio no responde") {
		t.Fatalf("con una tabla ilegible no hay diagnóstico: %v / %v", xs, err)
	}
	if dyn, err := s.DynamicRanges(); err != nil || len(dyn) != 4 {
		t.Fatalf("el rango dinámico no dependía de esa tabla: %v, %v", dyn, err)
	}
	s.red.olvidar()
	arrancaron, todas, falla = 0, make(chan struct{}), "dynamicport"
	if dyn, err := s.DynamicRanges(); err == nil || dyn != nil {
		t.Fatalf("con un rango ilegible no hay rangos: %v / %v", dyn, err)
	}
	if xs, err := s.Excluded(); err != nil || len(xs) != 4 {
		t.Fatalf("las exclusiones no dependían de ese rango: %v, %v", xs, err)
	}
}
