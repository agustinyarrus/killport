//go:build windows

package killport

// servicios.go: la configuración de los servicios que aparecen en un plan
// (con qué arrancan, si vuelven solos) y el paso de --manual: dejar en inicio
// MANUAL un servicio de terceros que killport acaba de detener, para que no
// vuelva en el próximo arranque.
//
// Leer la configuración no pide administrador y se hace una vez por servicio.
// Cambiarla, sí, y es la única escritura de este archivo. Las reglas:
//
//   - solo servicios de TERCEROS (los que instaló un programa: su ejecutable
//     no está en la carpeta de Windows). Jamás un controlador, un servicio de
//     Windows ni uno que viva en un svchost: para esos killport, a lo sumo,
//     muestra el comando;
//   - solo DESPUÉS de verlo detenido, y solo al que detuvo ESTA corrida. A un
//     servicio que no se pudo detener no se le toca la configuración: el
//     usuario no consiguió su puerto y no tiene por qué quedarse además con un
//     servicio cambiado. Y a uno que ya estaba detenido tampoco: killport le
//     cambia el inicio a lo que tenía el puerto, no a lo que alguien nombre;
//   - nunca se deshabilita nada: manual quiere decir que sigue arrancando
//     cuando alguien lo pide;
//   - siempre se dice cómo deshacerlo, con el tipo de inicio que tenía.

import (
	"errors"
	"sync"

	"github.com/agustinyarrus/killport/internal/reaper"
	"github.com/agustinyarrus/killport/internal/win"
)

// servicios es el lector de una corrida. Las tres funciones se inyectan en
// las pruebas: ninguna prueba cambia un servicio de verdad.
type servicios struct {
	leer   func(nombre string) (win.ServiceConfig, error)
	estado func(nombre string) (win.ServiceState, error)
	manual func(nombre string) error

	mu     sync.Mutex
	vistos map[string]lecturaServicio
}

type lecturaServicio struct {
	cfg win.ServiceConfig
	err error
}

func nuevosServicios() *servicios {
	return &servicios{leer: win.QueryServiceConfig, estado: win.QueryServiceState, manual: win.SetServiceDemandStart}
}

// de devuelve la configuración del servicio, memoizada. ok=false si no se
// pudo leer (no existe más, o Windows no deja): ahí el bloque no dice nada
// del inicio, que es mejor que decir algo inventado.
func (s *servicios) de(nombre string) (win.ServiceConfig, bool) {
	if s == nil || nombre == "" {
		return win.ServiceConfig{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.vistos[nombre]
	if !ok {
		l.cfg, l.err = s.leer(nombre)
		if s.vistos == nil {
			s.vistos = map[string]lecturaServicio{}
		}
		s.vistos[nombre] = l
	}
	return l.cfg, l.err == nil
}

// fijo dice si el servicio corre SIN aceptar que lo detengan: aconsejar un
// `net stop` sobre él es mandar al usuario a chocar contra un error. false si
// no se pudo saber.
func (s *servicios) fijo(nombre string) bool {
	if s == nil || s.estado == nil || nombre == "" {
		return false
	}
	st, err := s.estado(nombre)
	return err == nil && st.Running() && !st.AcceptsStop()
}

// afinarNegativas corrige el consejo de las negativas por un servicio de
// Windows que no se deja detener. La planificación es pura y no sabe qué
// acepta cada servicio: aconseja `net stop`, que vale para casi todos. Acá,
// con el estado a la vista, se dice la verdad. Devuelve un plan nuevo (no
// cambia el que recibe).
func afinarNegativas(s *servicios, plan reaper.Plan) reaper.Plan {
	var afinadas []reaper.Refusal
	for i, r := range plan.Refusals {
		if r.Service == "" || !s.fijo(r.Service) {
			continue
		}
		if afinadas == nil {
			afinadas = append([]reaper.Refusal(nil), plan.Refusals...)
		}
		afinadas[i].Advice = "Windows no deja detener " + r.Service + " (corre sin aceptar esa orden): para ese puerto no hay nada que cerrar; usá otro"
	}
	if afinadas != nil {
		plan.Refusals = afinadas
	}
	return plan
}

// olvidar descarta lo leído de un servicio (después de cambiarlo).
func (s *servicios) olvidar(nombre string) {
	s.mu.Lock()
	delete(s.vistos, nombre)
	s.mu.Unlock()
}

// finManual es en qué terminó el --manual de un servicio.
type finManual uint8

const (
	manualHecho     finManual = iota + 1 // quedó en inicio manual
	manualYaEstaba                       // ya era manual (o estaba deshabilitado): nada que cambiar
	manualNoSeToca                       // no es un servicio de terceros: killport no lo reconfigura
	manualSigueVivo                      // no se lo vio detenido: no se le cambia nada
	manualDenegado                       // hace falta administrador
	manualFallo                          // otro error
	// manualNoDetenido: lo dice el proceso elevado de un servicio que el trabajo
	// nombra y que ÉL no detuvo en su corrida (cuando miró, ya no tenía ninguno
	// de los puertos pedidos). Cambiarle el inicio sería obedecer al trabajo y
	// no a lo que se ve en el sistema: no se toca.
	manualNoDetenido
)

func (f finManual) clave() string {
	switch f {
	case manualHecho:
		return "manual"
	case manualYaEstaba:
		return "ya_estaba"
	case manualNoSeToca:
		return "no_se_toca"
	case manualSigueVivo:
		return "sigue_corriendo"
	case manualDenegado:
		return "denegado"
	case manualNoDetenido:
		return "no_lo_detuvo"
	}
	return "error"
}

// resultadoManual es cómo le fue a un servicio con --manual.
type resultadoManual struct {
	Servicio string
	Fin      finManual
	Antes    win.ServiceConfig // cómo estaba configurado (vale si se lo pudo leer)
	Motivo   string            // por qué no se hizo, en español; "" si se hizo
	Deshacer string            // el comando que lo devuelve a como estaba; "" si no se cambió nada
	Aviso    string            // lo que hay que saber aunque haya salido bien (disparadores)
	// Elevable (con manualDenegado): el servicio NO se pudo detener por falta de
	// permiso, así que viaja en el trabajo elevado, que lo detiene y después lo
	// deja en manual. Si lo que se negó fue solo el cambio de inicio —el
	// servicio ya quedó detenido—, no viaja: el proceso elevado le cambia el
	// inicio únicamente a lo que él mismo detuvo, y ahí se da el comando.
	Elevable bool
}

// dejarEnManual deja en inicio manual un servicio, con todas las reglas de
// arriba. Vuelve a leer la configuración (no usa la memoizada: decide sobre
// lo que hay AHORA) y verifica el resultado releyéndola.
func (s *servicios) dejarEnManual(nombre, systemRoot string) resultadoManual {
	r := resultadoManual{Servicio: nombre}
	cfg, err := s.leer(nombre)
	if err != nil {
		r.Fin, r.Motivo = manualFallo, "no pude leer su configuración: "+err.Error()
		if errors.Is(err, win.ErrServiceNotFound) {
			r.Motivo = "el servicio ya no existe"
		}
		return r
	}
	r.Antes = cfg
	if por := motivoNoManual(cfg, systemRoot); por != "" {
		r.Fin, r.Motivo = manualNoSeToca, por+": killport no le cambia el inicio"
		return r
	}
	st, err := s.estado(nombre)
	switch {
	case err != nil:
		r.Fin, r.Motivo = manualFallo, "no pude consultar su estado: "+err.Error()
		return r
	case !st.Stopped():
		r.Fin, r.Motivo = manualSigueVivo, "no quedó detenido: no le cambio el inicio a un servicio que sigue corriendo"
		return r
	}
	switch cfg.Start {
	case win.StartDemand:
		r.Fin, r.Motivo = manualYaEstaba, "ya estaba en inicio manual"
		r.Aviso = avisoDisparadores(cfg)
		return r
	case win.StartDisabled:
		r.Fin, r.Motivo = manualYaEstaba, "está deshabilitado: no vuelve"
		return r
	}
	switch err := s.manual(nombre); {
	case errors.Is(err, win.ErrAccessDenied):
		r.Fin, r.Motivo = manualDenegado, "cambiar el inicio de un servicio pide administrador"
		r.Aviso = "se hace a mano, en una consola de administrador: `" + comandoManual(nombre) + "`"
		return r
	case err != nil:
		r.Fin, r.Motivo = manualFallo, err.Error()
		return r
	}
	s.olvidar(nombre)
	ahora, err := s.leer(nombre)
	if err != nil || ahora.Start != win.StartDemand {
		r.Fin, r.Motivo = manualFallo, "Windows aceptó el cambio pero el servicio no quedó en inicio manual"
		return r
	}
	r.Fin, r.Deshacer, r.Aviso = manualHecho, comandoInicio(nombre, cfg), avisoDisparadores(cfg)
	return r
}

// comandoManual es el comando de sc que deja un servicio en inicio manual.
func comandoManual(nombre string) string {
	return comandoInicio(nombre, win.ServiceConfig{Start: win.StartDemand})
}

// avisoDisparadores: un servicio manual con disparadores vuelve igual.
func avisoDisparadores(c win.ServiceConfig) string {
	if c.Triggers == 0 {
		return ""
	}
	return "tiene " + contar(c.Triggers, "disparador", "disparadores") + ": un evento del sistema lo puede arrancar aunque esté en manual (`sc qtriggerinfo " + citarSC(c.Name) + "` los muestra)"
}

// serviciosDetenidos son los servicios de terceros del plan que la corrida
// detuvo: los candidatos de --manual.
func serviciosDetenidos(plan reaper.Plan, rep reaper.Report) []string {
	fin := map[reaper.ID]reaper.Outcome{}
	for _, v := range rep.Victims {
		fin[v.ID.Canon()] = v.Outcome
	}
	var out []string
	for _, tg := range plan.Targets {
		if tg.Kind == reaper.StopThirdSvc && fin[tg.Root.Canon()] == reaper.OutcomeServiceStopped {
			out = append(out, tg.Service)
		}
	}
	return out
}
