//go:build windows

package killport

// desenmascarar.go: el pegamento entre el protocolo puro de reaper (Masked →
// Unmasking → Unmask) y los dominios que se consultan de verdad: el motor de
// contenedores (docker.go), las distros de WSL (wsl.go), las colas de http.sys
// (httpsys.go) y las reglas de portproxy (portproxy.go). También anota el
// listado general: la fila de un intermediario dice QUÉ hay detrás.

import (
	"context"
	"sort"
	"strings"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/tui"
)

// desenmascarar mira detrás de los intermediarios del plan y lo reescribe con
// lo que vio. Reparte lo enmascarado por clase de intermediario y le pregunta
// a cada dominio SOLO por lo suyo y solo si hace falta: si ningún puerto
// pedido lo sostiene un relay, no abre un pipe ni corre wsl.exe. --no-docker
// apaga la consulta al motor (el relay queda negado con el motivo genérico).
//
// Una clase nueva de intermediario se suma acá: su dominio contesta un
// reaper.Unmasking por puerto, y lo que encuentre viaja en External.Ref como
// un `externo` (externo.go). El resto de la herramienta no se entera.
//
// Un puerto, dos intermediarios. Docker Desktop con WSL 2 publica cada puerto
// con com.docker.backend (0.0.0.0 y [::]) y ADEMÁS con wslrelay ([::1]): tres
// sockets de dos dueños para un mismo contenedor (visto en la máquina real).
// El socket de wslrelay es parte de esa publicación, así que sigue al motor:
// lo que el motor diga de ese puerto vale para los dos, no se mira dentro de
// WSL por él (ahí no hay ningún proceso del usuario) y jamás se lo toma por
// un relay colgado.
func desenmascarar(ctx context.Context, sys *sistema, plan reaper.Plan, socks []ports.Socket, forest *reaper.Forest, o opciones) reaper.Plan {
	porClase := map[reaper.Relay][]reaper.MaskedPort{}
	for _, m := range plan.Masked() {
		porClase[m.Relay] = append(porClase[m.Relay], m)
	}
	cont := porClase[reaper.RelayContainer]
	delMotor, deWSL := repartirWSL(porClase[reaper.RelayWSL], cont)

	var answers []reaper.Unmasking
	if len(cont) > 0 && !o.noDocker {
		// El motor contesta por sus sockets y por los de wslrelay que son
		// parte de sus publicaciones, repartiéndolos por la dirección
		// publicada (docker.go).
		answers = append(answers, sys.docker.desenmascarar(ctx, append(append([]reaper.MaskedPort(nil), cont...), delMotor...), forest)...)
	} else {
		answers = append(answers, seguirAlMotor(delMotor, cont)...)
	}
	if len(deWSL) > 0 {
		answers = append(answers, soloSusSockets(deWSL, sys.wsl.desenmascarar(ctx, deWSL, socks, forest, sys.docker, o.noDocker))...)
	}
	// Los dos intermediarios de Windows mismo. Detrás de http.sys hay procesos
	// de Windows (Unmasking.Procs): Unmask los clasifica con las reglas de
	// siempre. Detrás del IP Helper hay reglas de portproxy (un externo más).
	if delKernel := porClase[reaper.RelayHTTP]; len(delKernel) > 0 && sys.http != nil {
		answers = append(answers, sys.http.desenmascarar(ctx, delKernel, forest)...)
	}
	if delProxy := porClase[reaper.RelayPortProxy]; len(delProxy) > 0 && sys.proxy != nil {
		answers = append(answers, sys.proxy.desenmascarar(delProxy)...)
	}
	if len(answers) > 0 {
		plan = reaper.Unmask(plan, answers)
	}
	if o.noDocker {
		plan = sinNoDocker(plan)
	}
	return plan
}

// sinNoDocker suma, al consejo de un puerto que publica un contenedor y quedó
// negado porque se pidió --no-docker, que sin esa opción killport lo detiene.
func sinNoDocker(plan reaper.Plan) reaper.Plan {
	refs := append([]reaper.Refusal(nil), plan.Refusals...)
	for i := range refs {
		if r := &refs[i]; r.Relay == reaper.RelayContainer && !strings.Contains(r.Advice, "--no-docker") {
			r.Advice += "; o sin --no-docker, killport detiene el contenedor"
		}
	}
	plan.Refusals = refs
	return plan
}

// puertoProto identifica un puerto publicado.
type puertoProto struct {
	port  uint16
	proto ports.Proto
}

// repartirWSL separa lo que sostiene un relay de WSL en dos: lo que es parte
// de la publicación de un contenedor (es del motor) y lo demás (de WSL: un
// proceso Linux, o un relay colgado). Se decide SOCKET POR SOCKET: Docker
// Desktop publica con wslrelay solo en [::1], y en el mismo puerto puede
// estar también el 127.0.0.1 de un proceso de Ubuntu (arrancó antes que el
// contenedor): ese va a WSL. Decidir por (puerto, protocolo) atribuía el
// reenvío de la distro al contenedor, nunca se miraba dentro de WSL, y tras
// detener el contenedor se hablaba de un "relay colgado". Un par que queda
// partido va en dos MaskedPort con sus sockets; las respuestas lo respetan
// (Unmasking.Sockets). Puro. O(sockets).
func repartirWSL(enWSL, cont []reaper.MaskedPort) (delMotor, deWSL []reaper.MaskedPort) {
	publicados := map[puertoProto]bool{}
	for _, m := range cont {
		publicados[puertoProto{m.Port, m.Proto}] = true
	}
	for _, m := range enWSL {
		if !publicados[puertoProto{m.Port, m.Proto}] {
			deWSL = append(deWSL, m)
			continue
		}
		var motor, wsl []ports.Socket
		for _, s := range m.Sockets {
			if deLaPublicacion(s) {
				motor = append(motor, s)
			} else {
				wsl = append(wsl, s)
			}
		}
		switch {
		case len(m.Sockets) == 0:
			delMotor = append(delMotor, m) // sin sockets a la vista (armado a mano): el par entero
		default:
			if len(motor) > 0 {
				p := m
				p.Sockets = motor
				delMotor = append(delMotor, p)
			}
			if len(wsl) > 0 {
				p := m
				p.Sockets = wsl
				deWSL = append(deWSL, p)
			}
		}
	}
	return delMotor, deWSL
}

// soloSusSockets fija en cada respuesta los sockets de su MaskedPort cuando
// el par quedó partido entre dos dominios (repartirWSL): así la respuesta de
// uno no se aplica a los sockets del otro.
func soloSusSockets(ms []reaper.MaskedPort, us []reaper.Unmasking) []reaper.Unmasking {
	type clave struct {
		via   reaper.ID
		port  uint16
		proto ports.Proto
	}
	socks := map[clave][]ports.Socket{}
	for _, m := range ms {
		socks[clave{m.Via.Canon(), m.Port, m.Proto}] = m.Sockets
	}
	for i := range us {
		if u := &us[i]; u.Sockets == nil {
			u.Sockets = socks[clave{u.Via.Canon(), u.Port, u.Proto}]
		}
	}
	return us
}

// seguirAlMotor contesta por los sockets de wslrelay que son parte de la
// publicación de un contenedor cuando al motor NO se le preguntó
// (--no-docker): quedan negados con el motivo de verdad, no el de un reenvío
// de WSL suelto. Nunca KillVia: ese relay está trabajando. (Si al motor se le
// preguntó, él contesta también por estos sockets: docker.desenmascarar.)
// Puro.
func seguirAlMotor(delMotor, cont []reaper.MaskedPort) []reaper.Unmasking {
	if len(delMotor) == 0 {
		return nil
	}
	motor := map[puertoProto]string{}
	for _, m := range cont {
		motor[puertoProto{m.Port, m.Proto}] = m.Name
	}
	out := make([]reaper.Unmasking, 0, len(delMotor))
	for _, m := range delMotor {
		out = append(out, reaper.Unmasking{
			Via: m.Via, Port: m.Port, Proto: m.Proto, Sockets: m.Sockets,
			Verdict: reaper.UnmaskUnavailable,
			Reason:  "reenvía el mismo puerto que publica el motor de contenedores (" + motor[puertoProto{m.Port, m.Proto}] + "): es parte de esa publicación",
			Advice:  "el puerto lo publica un contenedor: `docker ps` muestra cuál y `docker stop NOMBRE` lo libera; o sin --no-docker, killport detiene el contenedor",
		})
	}
	return out
}

// fundirPublicaciones deja UNA fila por puerto publicado: el socket [::1] de
// wslrelay en un puerto que publica un relay de contenedores es el mismo
// puerto del mismo contenedor ([::1] de un lado, 0.0.0.0 y [::] del otro),
// así que se funde en la fila del motor (suma su familia, y cuenta desde el
// socket más viejo). Lo demás que wslrelay tenga en ese puerto (el 127.0.0.1
// de un proceso de Ubuntu) sigue siendo una fila de WSL (ver repartirWSL).
// Puro; conserva el orden. O(filas).
func fundirPublicaciones(filas []fila) []fila {
	motor := map[puertoProto]int{} // (puerto, proto) → índice en out de la fila del motor
	for _, f := range filas {
		if f.Relay == reaper.RelayContainer {
			motor[puertoProto{f.Port, f.Proto}] = -1
		}
	}
	if len(motor) == 0 {
		return filas
	}
	out := make([]fila, 0, len(filas))
	var sueltas []fila // de wslrelay, llegaron antes que la fila del motor
	for _, f := range filas {
		k := puertoProto{f.Port, f.Proto}
		i, publicado := motor[k]
		if f.Relay == reaper.RelayWSL && publicado {
			// Solo el [::1] es de la publicación: el resto queda en su fila.
			pub, resto := partirFila(f)
			if resto != nil {
				out = append(out, *resto)
			}
			if pub == nil {
				continue
			}
			f = *pub
		}
		switch {
		case f.Relay == reaper.RelayContainer && i < 0:
			motor[k] = len(out)
			out = append(out, f)
		case f.Relay == reaper.RelayWSL && publicado && i >= 0:
			fundir(&out[i], f)
		case f.Relay == reaper.RelayWSL && publicado:
			sueltas = append(sueltas, f)
		default:
			out = append(out, f)
		}
	}
	for _, f := range sueltas {
		fundir(&out[motor[puertoProto{f.Port, f.Proto}]], f)
	}
	return out
}

// partirFila separa una fila de wslrelay en lo que es de la publicación de un
// contenedor (su [::1]) y lo demás; nil donde no queda nada. Una fila sin
// sockets a la vista es entera de la publicación (como antes).
func partirFila(f fila) (pub, resto *fila) {
	if len(f.socks) == 0 {
		return &f, nil
	}
	var a, b []ports.Socket
	for _, s := range f.socks {
		if deLaPublicacion(s) {
			a = append(a, s)
		} else {
			b = append(b, s)
		}
	}
	con := func(socks []ports.Socket) *fila {
		if len(socks) == 0 {
			return nil
		}
		g := f
		g.socks, g.Familias = socks, familias{}
		for _, s := range socks {
			if s.Family() == ports.IPv6 {
				g.Familias.v6 = true
			} else {
				g.Familias.v4 = true
			}
		}
		return &g
	}
	return con(a), con(b)
}

// fundir suma a la fila del motor la de wslrelay del mismo puerto: sus
// familias y sus sockets (la dirección de la fila son las de los dos).
func fundir(m *fila, w fila) {
	m.Familias.v4 = m.Familias.v4 || w.Familias.v4
	m.Familias.v6 = m.Familias.v6 || w.Familias.v6
	m.socks = append(m.socks, w.socks...)
	if !w.Since.IsZero() && (m.Since.IsZero() || w.Since.Before(m.Since)) {
		m.Since = w.Since
	}
}

// anotarWindows completa las filas que figuran a nombre de Windows y son de
// otro: la de System (PID 4) en un puerto donde alguien registró una URL de
// http.sys pasa a nombrar a ese proceso, y la del IP Helper en un puerto con
// una regla de portproxy, a la regla. Cambia también si la fila se oculta: lo
// decide quién está detrás, no el intermediario.
//
// A http.sys se le pregunta (una corrida de netsh) solo si System escucha en
// algún puerto TCP que no sea de compartir archivos; las reglas de portproxy
// se leen del registro solo si el IP Helper escucha por TCP. Donde no se
// encontró nada, la fila vuelve a ser la de un proceso de Windows cualquiera.
func anotarWindows(ctx context.Context, sys *sistema, filas []fila, forest *reaper.Forest, svc map[uint32][]reaper.Service, cfg reaper.Config) []fila {
	for i := range filas {
		f := &filas[i]
		switch {
		case f.Relay == reaper.RelayHTTP && sys.http != nil:
			sys.http.consultar(ctx)
			if sys.http.err != nil {
				// No se pudo preguntar: "no sé quién está detrás" no es "no
				// hay nadie". Se muestra, y se dice por qué no se miró.
				noPudeMirar(f, "http.sys", "no pude preguntarle a http.sys quién registró la URL", sys.http.err)
				continue
			}
			colas := sys.http.enPuerto(f.Port)
			ids := identidades(colas, forest)
			switch {
			case len(ids) > 0:
				anotarHTTP(f, sys.http, ids, forest, svc, cfg)
			case len(colas) > 0:
				f.Comando = "http.sys · " + listaURL(colas, f.Port)
			default:
				f.Relay, f.Via = reaper.RelayNone, ""
			}
		case f.Relay == reaper.RelayPortProxy && sys.proxy != nil:
			sys.proxy.consultar()
			if sys.proxy.err != nil {
				noPudeMirar(f, "portproxy", "no pude leer las reglas de portproxy", sys.proxy.err)
				continue
			}
			reglas := sostienen(sys.proxy.reglas, f.socks)
			if len(reglas) == 0 {
				f.Relay, f.Via = reaper.RelayNone, ""
				continue
			}
			r := reglas[0]
			f.Regla, f.OtrasReglas = &r, reglas[1:]
			f.Titulo = "portproxy"
			f.Comando = "regla " + r.String()
			if n := len(f.OtrasReglas); n > 0 {
				// Como en http.sys: la fila nombra una y dice cuántas más hay,
				// para que nadie borre la primera y crea que liberó el puerto.
				f.Comando += " y " + contar(n, "regla más", "reglas más")
			}
			f.EsWindows = false // la regla la puso alguien: es del usuario, no de Windows
			f.Servicio = ""
		case f.Relay == reaper.RelayHTTP, f.Relay == reaper.RelayPortProxy:
			f.Relay, f.Via = reaper.RelayNone, "" // no hay a quién preguntarle
		}
	}
	return filas
}

// anotarHTTP completa la fila de un puerto de http.sys con los procesos que
// tienen una URL registrada ahí. Pueden ser varios (cada uno con su camino), y
// de distinta clase: un servicio de Windows y el servidor de un programa del
// usuario comparten el 80 sin problema. La fila nombra al primero que NO es de
// Windows —es el que el usuario está buscando— y dice cuántos más hay; se
// oculta solo si son todos de Windows.
// noPudeMirar deja a la vista una fila de un intermediario de Windows cuando
// no se pudo averiguar qué hay detrás (netsh o el registro fallaron): el
// servidor de un usuario no puede quedar escondido detrás de "System" por
// una falla al preguntar. La fila lo dice (y el JSON, en no_pude_mirar).
func noPudeMirar(f *fila, que, por string, err error) {
	f.SinMirar = por + ": " + err.Error()
	f.Comando = que + " · no pude mirar quién está detrás"
	f.EsWindows = false
	f.Servicio = ""
}

func anotarHTTP(f *fila, h *httpsys, ids []reaper.ID, forest *reaper.Forest, svc map[uint32][]reaper.Service, cfg reaper.Config) {
	var detras []reaper.Proc
	principal, deWindows := -1, true
	for _, id := range ids {
		p, ok := forest.Lookup(id)
		if !ok {
			continue
		}
		detras = append(detras, p)
		if !reaper.IsWindowsProc(p, forest, svc, cfg) && deWindows {
			principal, deWindows = len(detras)-1, false
		}
	}
	if len(detras) == 0 {
		return
	}
	if principal < 0 {
		principal = 0
	}
	puerto := ports.Of(f.Port)
	de := func(p reaper.Proc) jsonHTTP {
		return jsonHTTP{PID: p.PID, Proceso: p.Name, URLs: h.urlsDe(p.PID, puerto)}
	}
	p := detras[principal]
	http := de(p)
	for i, otro := range detras {
		if i != principal {
			http.Otros = append(http.Otros, de(otro))
		}
	}
	f.HTTP = &http
	f.Titulo = "http.sys · " + nombreVisible(p.Name, p.PID)
	if n := len(http.Otros); n > 0 {
		f.Titulo += " y " + u32(uint32(n)) + " más"
	}
	f.Comando = strings.Join(http.URLs, "  ")
	f.EsWindows = deWindows
	f.Servicio = ""
	for _, x := range detras {
		if st, por := reaper.StandingOf(x, forest, cfg.SystemRoot); st == reaper.StandingImpostor {
			f.Impostor, f.impNombre, f.impPID = por, x.Name, x.PID
			break
		}
	}
}

// anotarListado completa las filas cuyo dueño es un intermediario: la de un
// relay de contenedores dice qué contenedor publica ese puerto (salvo
// --no-docker); con --wsl, la de wslrelay dice qué proceso Linux escucha, y
// se devuelven aparte las que escuchan SOLO dentro de WSL (no se reenvían a
// Windows). Sin --wsl no se corre wsl.exe: la fila solo avisa que se puede.
//
// Lo que escucha solo dentro de WSL y es del sistema de la distro (systemd,
// chronyd…) no se lista salvo --system, como lo de Windows: ocultosLinux
// cuenta cuántos.
func anotarListado(ctx context.Context, sys *sistema, filas []fila, forest *reaper.Forest, o opciones) (anotadas []fila, soloWSL []fila, ocultosLinux int) {
	hayCont, hayWSL := false, false
	for _, f := range filas {
		hayCont = hayCont || f.Relay == reaper.RelayContainer
		hayWSL = hayWSL || f.Relay == reaper.RelayWSL
	}
	if hayCont && !o.noDocker {
		sys.docker.consultar(ctx, forest)
	}
	if o.wsl {
		sys.wsl.consultar(ctx)
	}
	usados := map[string]bool{}
	var partidas []fila // lo que otro contenedor publica en el mismo puerto, en otra dirección
	for i := range filas {
		f := &filas[i]
		switch f.Relay {
		case reaper.RelayContainer:
			if o.noDocker {
				continue
			}
			// Dos contenedores pueden publicar el mismo puerto en direcciones
			// distintas del host: la fila del motor se parte, una por
			// contenedor, cada una con sus direcciones (docker_parse.go).
			partes := repartirPublicacion(reaper.MaskedPort{Port: f.Port, Proto: f.Proto, Sockets: f.socks}, sys.docker.publican(f.Port, f.Proto))
			for j, parte := range partes {
				if len(parte.conts) == 0 {
					continue
				}
				g := f
				if j > 0 {
					partidas = append(partidas, *f)
					g = &partidas[len(partidas)-1]
				}
				c := sys.docker.conReinicio(ctx, parte.conts[0])
				g.Contenedor = &c
				// PROCESO es el contenedor (el nombre entero: dos de compose
				// empiezan igual); PROYECTO, su proyecto y servicio de compose;
				// COMANDO, el motor y la imagen.
				g.Titulo = c.Nombre
				g.Comando, g.keep = motorCorto(c)+" · "+c.Imagen, tui.Width(motorCorto(c)+" · ")
				if len(partes) > 1 {
					g.socks = parte.socks
				}
			}
		case reaper.RelayWSL:
			if !o.wsl {
				// Lo que importa es la opción: va primero, y el recorte la respeta.
				f.Comando, f.keep = "--wsl dice quién escucha adentro", tui.Width("--wsl dice quién")
				continue
			}
			hay, duenos := sys.wsl.escuchaEn(f.Port, f.Proto)
			switch {
			case len(duenos) > 0 && !duenos[0].esDelMotor():
				// Una fila por proceso Linux: el relay reenvía el puerto a todos
				// los que lo escuchan adentro (SO_REUSEPORT, dos distros), y
				// `killport P` los tiene a todos como objetivo (wsl.encontrado):
				// el listado dice lo mismo (Q-11), no solo el primero.
				base := *f
				for j, p := range duenos {
					if p.esDelMotor() {
						continue
					}
					g := f
					if j > 0 {
						partidas = append(partidas, base)
						g = &partidas[len(partidas)-1]
					}
					cp := p
					g.Linux = &cp
					g.Titulo = tituloLinux(p)
					g.Since = p.Desde
					g.Comando = p.Cmdline
					usados[p.clave()+"|"+protoPuerto(f.Proto, f.Port)] = true
				}
			case len(duenos) > 0:
				// El reenvío de un motor de contenedores que corre DENTRO de
				// WSL (docker-proxy en Ubuntu, o la distro de Docker Desktop).
				f.Comando = "lo publica un contenedor dentro de WSL (" + duenos[0].Comm + " en " + duenos[0].Distro + ")"
			case hay:
				f.Comando = "reenvío de WSL · algo escucha adentro, pero ninguna distro dice qué proceso es"
			case sys.wsl.errLista != nil || len(sys.wsl.fallas) > 0:
				f.Comando = "reenvío de WSL · no pude mirar adentro"
			default:
				f.Comando = "relay colgado: en WSL no escucha nadie · killport " + u32(uint32(f.Port)) + " lo revisa"
			}
		}
	}
	if len(partidas) > 0 {
		filas = append(filas, partidas...)
		sort.SliceStable(filas, func(i, j int) bool { return menorFila(filas[i], filas[j]) })
	}
	if !o.wsl {
		return filas, nil, 0
	}
	// Lo que escucha en WSL y no apareció detrás de un relay: no se reenvía
	// a Windows (atado a la IP de la VM, o no hay relay para ese puerto).
	for _, sd := range sys.wsl.sondeos {
		if distroDeMotor(sd.Distro) {
			continue // la distro de Docker Desktop no es del usuario: lo suyo se ve por sus contenedores
		}
		for _, p := range sd.Procesos {
			for _, s := range p.Sockets {
				k := p.clave() + "|" + protoPuerto(s.Proto, s.Port())
				if usados[k] {
					continue
				}
				usados[k] = true
				if demonioLinux(p) && !o.system {
					ocultosLinux++
					continue
				}
				cp := p
				fl := fila{Port: s.Port(), Proto: s.Proto, Name: p.Comm, Titulo: tituloLinux(p),
					Since: p.Desde, Comando: p.Cmdline, Linux: &cp, SoloWSL: true}
				if s.Local.Addr().Is4() {
					fl.Familias.v4 = true
				} else {
					fl.Familias.v6 = true
				}
				fl.Direccion = s.Local.Addr().WithZone("").String()
				soloWSL = append(soloWSL, fl)
			}
		}
	}
	sort.SliceStable(soloWSL, func(i, j int) bool {
		if soloWSL[i].Port != soloWSL[j].Port || soloWSL[i].Proto != soloWSL[j].Proto {
			return menorFila(soloWSL[i], soloWSL[j])
		}
		return soloWSL[i].Linux.PID < soloWSL[j].Linux.PID // sin PID de Windows: el de Linux
	})
	return filas, soloWSL, ocultosLinux
}

// demoniosLinux son los procesos del sistema de una distro que escuchan
// sin que el usuario haga nada: el resolvedor de systemd en el 53, el reloj
// en el 323… (el comm de Linux tiene hasta 15 letras).
var demoniosLinux = map[string]bool{
	"systemd": true, "systemd-resolve": true, "systemd-network": true, "systemd-timesyn": true,
	"chronyd": true, "ntpd": true, "avahi-daemon": true, "rpcbind": true, "cupsd": true,
	"dnsmasq": true, "dhclient": true, "snapd": true, "init": true,
}

// demonioLinux: es el PID 1 de la distro, o uno de los demonios de siempre.
func demonioLinux(p procesoLinux) bool {
	return p.PID == 1 || demoniosLinux[p.Comm]
}

// motorCorto es el nombre del motor en minúsculas para la columna PROCESO.
func motorCorto(c contenedor) string {
	if c.Motor != nil && c.Motor.Nombre == "Podman" {
		return "podman"
	}
	return "docker"
}

func protoPuerto(p ports.Proto, port uint16) string {
	return p.String() + "/" + u32(uint32(port))
}
