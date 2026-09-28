package reaper

// Run es la escalera entera y se lee de una vez: cada peldaño es una función
// con su nombre (openAll, stopServices, stopExternals, sendGraceful,
// waitExits, terminateRest, settle) y Run solo los encadena. Antes tenía 178
// líneas con los peldaños 0, 1 y 4 y el cierre de cuentas adentro. El tope
// del resto del motor es el de hoy, para que no crezca sin darse cuenta.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestFuncionesDelMotorAcotadas(t *testing.T) {
	const topeRun, tope = 70, 150
	fset := token.NewFileSet()
	entradas, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entradas {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			lineas := fset.Position(fn.End()).Line - fset.Position(fn.Pos()).Line + 1
			max := tope
			if fn.Recv == nil && fn.Name.Name == "Run" {
				max = topeRun
			}
			if lineas > max {
				t.Errorf("%s: %s tiene %d líneas (tope %d)", fset.Position(fn.Pos()), fn.Name.Name, lineas, max)
			}
		}
	}
}
