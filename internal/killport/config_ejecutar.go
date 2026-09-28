package killport

// config_ejecutar.go: el EJECUTOR de un plan de configuración. Corre los pasos
// en orden contra una interfaz (configurador): en producción es netsh y el
// administrador de servicios; en las pruebas, un doble que anota lo que le
// piden y falla donde se le diga. Ninguna prueba toca la red de la máquina.
//
// Dos garantías, las dos probadas:
//
//   - WINNAT VUELVE. Si se INTENTÓ detener WinNAT, se lo vuelve a arrancar
//     pase lo que pase después: que falle un paso, que el usuario corte con
//     Ctrl+C, incluso que la detención misma haya informado un error (un plazo
//     vencido no dice si llegó a detenerse; arrancar algo que ya corre no hace
//     nada). Dejar la máquina sin la red de WSL y Docker por un error a mitad
//     de camino sería peor que no haber hecho nada.
//   - NO SIGUE A CIEGAS. Si un paso falla, los que quedan no se intentan
//     (salvo ese arranque): cada uno supone que el anterior salió bien.

import (
	"context"
	"errors"
	"time"
)

// configurador es lo que el ejecutor necesita del sistema.
type configurador interface {
	// detenerWinnat detiene el controlador de WinNAT y espera a verlo detenido.
	detenerWinnat(ctx context.Context, deadline time.Time) error
	// arrancarWinnat lo arranca y espera a verlo corriendo.
	arrancarWinnat(ctx context.Context, deadline time.Time) error
	// netsh corre netsh con esos argumentos.
	netsh(ctx context.Context, args ...string) (string, error)
}

// plazoWinnat es cuánto se espera a que WinNAT se detenga o arranque. Es un
// controlador chico: tarda décimas de segundo.
const plazoWinnat = 20 * time.Second

// finPaso es cómo terminó un paso.
type finPaso uint8

const (
	pasoHecho    finPaso = iota + 1 // salió bien
	pasoFallo                       // se intentó y falló
	pasoSalteado                    // no se intentó: falló uno anterior, o se canceló
)

func (f finPaso) clave() string {
	switch f {
	case pasoHecho:
		return "hecho"
	case pasoFallo:
		return "error"
	}
	return "salteado"
}

// resultadoPaso es cómo le fue a un paso.
type resultadoPaso struct {
	Paso  paso
	Fin   finPaso
	Err   error
	Tardo time.Duration
}

// errCancelado: el usuario cortó antes de este paso.
var errCancelado = errors.New("cancelado")

// ejecutarConfig corre los pasos del plan, en orden, y devuelve cómo le fue a
// cada uno (uno por paso, en el mismo orden). aviso, si no es nil, se llama
// dos veces por paso que se intenta: antes (Fin en cero) y después. now es el
// reloj (las pruebas lo fijan).
func ejecutarConfig(ctx context.Context, c configurador, plan planConfig, now func() time.Time, aviso func(resultadoPaso)) []resultadoPaso {
	out := make([]resultadoPaso, len(plan.Pasos))
	for i, p := range plan.Pasos {
		out[i] = resultadoPaso{Paso: p, Fin: pasoSalteado}
	}
	if !plan.ejecutable() {
		return out
	}
	avisar := func(r resultadoPaso) {
		if aviso != nil {
			aviso(r)
		}
	}
	tocado, roto := false, false
	for i, p := range plan.Pasos {
		cancelado := ctx.Err() != nil
		if p.Clase == pasoArrancarWinnat {
			if !tocado {
				continue // a WinNAT no se lo llegó a tocar: no hay nada que devolver
			}
			// El arranque no se saltea ni se cancela: lleva su propio contexto.
			ctx := context.WithoutCancel(ctx)
			avisar(resultadoPaso{Paso: p})
			t0 := now()
			err := c.arrancarWinnat(ctx, t0.Add(plazoWinnat))
			out[i] = cerrar(p, err, now().Sub(t0))
			avisar(out[i])
			continue
		}
		if roto || cancelado {
			if cancelado && !roto {
				out[i].Err = errCancelado
			}
			roto = true
			continue
		}
		avisar(resultadoPaso{Paso: p})
		t0 := now()
		var err error
		switch p.Clase {
		case pasoDetenerWinnat:
			tocado = true
			err = c.detenerWinnat(ctx, t0.Add(plazoWinnat))
		case pasoExcluir, pasoQuitar, pasoDinamico:
			_, err = c.netsh(ctx, p.netsh()...)
		default:
			err = errors.New("paso desconocido") // el vocabulario es cerrado: acá no se llega
		}
		out[i] = cerrar(p, err, now().Sub(t0))
		avisar(out[i])
		roto = err != nil
	}
	return out
}

func cerrar(p paso, err error, tardo time.Duration) resultadoPaso {
	r := resultadoPaso{Paso: p, Fin: pasoHecho, Err: err, Tardo: tardo}
	if err != nil {
		r.Fin = pasoFallo
	}
	return r
}

// todoHecho dice si todos los pasos salieron bien.
func todoHecho(rs []resultadoPaso) bool {
	for _, r := range rs {
		if r.Fin != pasoHecho {
			return false
		}
	}
	return len(rs) > 0
}
