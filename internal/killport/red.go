//go:build windows

package killport

// red.go: lo que netsh sabe de la red de Windows y killport necesita para
// explicar un puerto que nadie tiene: los RANGOS EXCLUIDOS (los bloques que
// WinNAT aparta para Hyper-V, WSL y Docker, los puertos que aparta http.sys
// mientras los atiende, las reservas de los programas y las exclusiones
// administradas; de quién es cada una lo dice exclusiones.go) y el RANGO
// DINÁMICO de cada protocolo y familia.
//
// Son ocho consultas (dos tablas × dos protocolos × dos familias) y netsh
// tarda unos 200 ms en arrancar. Van TODAS A LA VEZ: son independientes, así
// que el diagnóstico de un puerto sin dueño —el caso más común: "ya estaba
// libre"— cuesta lo que una sola. El resultado se arma en un orden fijo (TCP
// v4, TCP v6, UDP v4, UDP v6), no en el de llegada.
//
// La lectura se memoiza: dentro de una corrida las tablas no cambian solas.
// Cuando killport mismo las cambia (--reserve, --unreserve, --fix-dynamic) o
// vuelve de un trabajo elevado que pudo cambiarlas, se la olvida y la próxima
// consulta vuelve a leer.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/agustinyarrus/killport/internal/ports"
	"github.com/agustinyarrus/killport/internal/win"
)

// netshTimeout es el tope de cada corrida de netsh. Contesta en décimas de
// segundo; si tarda más que esto, está trabado.
const netshTimeout = 15 * time.Second

// correrNetsh corre el netsh de System32 (por ruta absoluta, con el ejecutor
// acotado: sin ventana, con plazo, dentro de un Job) y pasa su salida a texto.
// netsh escribe en UTF-8 cuando su salida va a un pipe en los Windows
// actuales, y en la página de códigos OEM en los anteriores: win.ConsoleText
// mira lo que mandó.
func correrNetsh(ctx context.Context, args ...string) (string, error) {
	res, err := win.RunBounded(ctx, win.SystemExe("netsh.exe"), args, win.Bounded{Timeout: netshTimeout})
	if err != nil {
		return "", err
	}
	out := win.ConsoleText(res.Stdout)
	if res.ExitCode != 0 {
		return out, &errorNetsh{Codigo: res.ExitCode, Dijo: primeraConTexto(out)}
	}
	return out, nil
}

// lecturaRed es una lectura de las ocho tablas. Las exclusiones y el rango
// dinámico fallan por separado: que una no se haya podido leer no invalida a
// la otra. Dentro de cada una, en cambio, media tabla no sirve: un error en
// cualquiera de sus cuatro consultas la invalida entera (con tres de cuatro,
// "no está excluido" sería una mentira posible).
type lecturaRed struct {
	excl    ports.Exclusions
	errExcl error
	dyn     ports.DynamicRanges
	errDyn  error
	// atribuidas son excl con lo que se sabe de http.sys (ver
	// sistema.exclusiones): se arman una vez, la primera vez que hacen falta.
	atribucion sync.Once
	atribuidas ports.Exclusions
}

// red es el lector de una corrida.
type red struct {
	netsh netshFn
	mu    sync.Mutex
	vista *lecturaRed
}

func nuevaRed(n netshFn) *red { return &red{netsh: n} }

// leer devuelve la lectura memoizada, o lee.
func (r *red) leer() *lecturaRed {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.vista == nil {
		r.vista = leerRed(r.netsh)
	}
	return r.vista
}

// olvidar descarta la lectura: la próxima consulta vuelve a correr netsh.
func (r *red) olvidar() {
	r.mu.Lock()
	r.vista = nil
	r.mu.Unlock()
}

// leerRed corre las ocho consultas a la vez y arma la lectura.
func leerRed(netsh netshFn) *lecturaRed {
	var (
		wg   sync.WaitGroup
		excl [len(combos)]ports.Exclusions
		eExc [len(combos)]error
		dyn  [len(combos)]ports.DynamicRange
		eDyn [len(combos)]error
	)
	for i, c := range combos {
		wg.Add(2)
		go func() {
			defer wg.Done()
			out, err := netsh(context.Background(), "interface", c.ipv(), "show", "excludedportrange", "protocol="+c.prot())
			if err == nil {
				excl[i], err = ports.ParseExcluded(out, c.proto, c.fam)
			}
			eExc[i] = err
		}()
		go func() {
			defer wg.Done()
			out, err := netsh(context.Background(), "interface", c.ipv(), "show", "dynamicport", "protocol="+c.prot())
			if err == nil {
				dyn[i], err = ports.ParseDynamic(out, c.proto, c.fam)
			}
			eDyn[i] = err
		}()
	}
	wg.Wait()

	l := &lecturaRed{}
	for i := range combos {
		if eExc[i] != nil && l.errExcl == nil {
			l.errExcl = fmt.Errorf("rangos excluidos de %s: %w", combos[i], eExc[i])
		}
		if eDyn[i] != nil && l.errDyn == nil {
			l.errDyn = fmt.Errorf("rango dinámico de %s: %w", combos[i], eDyn[i])
		}
	}
	if l.errExcl == nil {
		for i := range combos {
			l.excl = append(l.excl, excl[i]...)
		}
	}
	if l.errDyn == nil {
		l.dyn = append(l.dyn, dyn[:]...)
	}
	return l
}
