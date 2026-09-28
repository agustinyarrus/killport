package killport

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Q-01 y Q-02: killport habla HTTP/1.1 con el motor por su cuenta (http1.go)
// y valida los id sin expresiones regulares: el binario no carga con net/http
// (TLS, certificados, HTTP/2, IDNA) ni con regexp. Medido en la revisión:
// 8,55 MB → 4,70 MB.
func TestBinarioSinNetHTTPNiRegexp(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: sin correr go list")
	}
	out, err := exec.Command("go", "list", "-deps", "github.com/agustinyarrus/killport").Output()
	if err != nil {
		t.Skipf("go list no anduvo: %v", err)
	}
	deps := strings.Fields(string(out))
	for _, no := range []string{"net/http", "crypto/tls", "crypto/x509", "regexp"} {
		if slices.Contains(deps, no) {
			t.Errorf("killport depende de %s", no)
		}
	}
}

// Q-08: lo puro del paquete (y sus pruebas) compila en cualquier sistema: una
// prueba sin etiqueta no puede depender de un archivo con //go:build windows.
// Antes alcance_test.go usaba compartidosJSON, que vivía en salida.go.
func TestLoPuroCompilaFueraDeWindows(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: sin correr go vet")
	}
	cmd := exec.Command("go", "vet", "github.com/agustinyarrus/killport/internal/killport")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("GOOS=linux go vet: %v\n%s", err, out)
	}
}
