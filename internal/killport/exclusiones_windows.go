//go:build windows

package killport

// exclusiones_windows.go: lo que hace falta leer para decir de quién es una
// exclusión (exclusiones.go). Solo se lee si hay una fila que puede ser de
// http.sys —de un solo puerto, TCP, sin "*"—: en una máquina con nada más que
// bloques de WinNAT no se corre ni un netsh de más.

import (
	"context"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/agustinyarrus/killport/internal/ports"
)

// exclusiones son los rangos excluidos de la lectura memoizada, con lo que se
// sabe de http.sys en los que pueden ser suyos. Se atribuyen una vez por
// lectura.
func (s *sistema) exclusiones() (ports.Exclusions, error) {
	l := s.red.leer()
	if l.errExcl != nil {
		return nil, l.errExcl
	}
	l.atribucion.Do(func() { l.atribuidas = s.atribuir(l.excl) })
	return l.atribuidas, nil
}

// atribuir junta la evidencia de http.sys (las colas y las reservas de URL, a
// la vez; los puertos que escucha el kernel) y la aplica. Lo que no se pudo
// leer queda sin evidencia: una fila sin pruebas es la de un programa, que es
// lo más prudente de decir (nunca aconseja tocar WinNAT).
func (s *sistema) atribuir(xs ports.Exclusions) ports.Exclusions {
	if s.http == nil || !slices.ContainsFunc(xs, candidataHTTP) {
		return xs
	}
	ctx, cancel := context.WithTimeout(context.Background(), netshTimeout)
	defer cancel()
	ev := evidenciaHTTP{servicioDeSID: servicioDeSID}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.http.consultar(ctx)
	}()
	go func() {
		defer wg.Done()
		if out, err := s.http.netsh(ctx, "http", "show", "urlacl"); err == nil {
			ev.reservas, _ = ports.ParseURLReservations(out)
		}
	}()
	if socks, err := s.Listening(ctx); err == nil {
		var ps []uint16
		for _, x := range socks {
			if x.PID == 4 && x.Proto == ports.TCP && x.State == ports.Listen {
				ps = append(ps, x.Port())
			}
		}
		ev.kernel = ports.Of(ps...)
	}
	wg.Wait()
	ev.colas = s.http.colas
	servicios := sync.OnceValue(func() map[uint32][]string {
		m := map[uint32][]string{}
		if svcs, err := s.Services(); err == nil {
			for pid, xs := range svcs {
				for _, x := range xs {
					m[pid] = append(m[pid], x.Name)
				}
			}
		}
		return m
	})
	ev.serviciosDe = func(pid uint32) []string { return servicios()[pid] }
	return atribuirHTTP(xs, ev)
}

// servicioDeSID resuelve el SID de un servicio (S-1-5-80-…, el que Windows
// deriva de su nombre) al nombre del servicio. LookupAccountSid devuelve el
// dominio traducido ("NT SERVICE", "NT-DIENST") y la cuenta, que es el nombre
// del servicio tal cual: se usa solo la cuenta.
func servicioDeSID(sid string) string {
	if !strings.HasPrefix(sid, "S-1-5-80-") {
		return ""
	}
	x, err := syscall.StringToSid(sid)
	if err != nil {
		return ""
	}
	cuenta, _, _, err := x.LookupAccount("")
	if err != nil {
		return ""
	}
	return cuenta
}
