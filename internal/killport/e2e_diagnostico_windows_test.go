//go:build windows

package killport

import (
	"strconv"
	"strings"
	"testing"
)

// X-29, de punta a punta: con varios puertos y uno con dueño, lo que se
// promete de los que no lo tienen ("reviso si lo reserva Windows") se cuenta
// ANTES de preguntar. Antes no se decía nunca ni con --list, ni sin consola
// (solo mostrar), ni cuando el usuario contestaba que no.
func TestE2ESinDuenoSeCuentaEnElBloque(t *testing.T) {
	dir := binariosE2E(t)
	p1, _ := escuchar(t, dir, t.TempDir(), "127.0.0.1", 0, "A")
	p2 := puertoLibreDelSistema(t)
	s1, s2 := strconv.Itoa(p1), strconv.Itoa(p2)
	for _, args := range [][]string{{s1, s2, "--list", "--tcp"}, {s1, s2, "--tcp"}} {
		out, code := killportE2E(t, dir, nil, args...)
		if code != 0 || !strings.Contains(out, "Sin dueño en la tabla: "+s2) || !strings.Contains(out, "puerto "+s2+" libre (ya estaba libre)") ||
			strings.Contains(out, "reviso si") {
			t.Fatalf("killport %s: código %d\n%s", strings.Join(args, " "), code, out)
		}
	}
	if libre(p1) {
		t.Fatal("se cerró el escuchador: solo había que mirar")
	}
}
