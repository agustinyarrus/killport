package reaper

// Un nombre para cada cosa: el vocabulario de puertos (BindResult y sus
// valores, Proto, Family…) vive en ports y el núcleo lo usa desde ahí, sin
// alias propios que dupliquen el nombre (antes reaper.BindForbidden y
// ports.BindForbidden eran lo mismo, y cada lado usaba uno distinto).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestSinAliasDePorts(t *testing.T) {
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
		ast.Inspect(f, func(x ast.Node) bool {
			switch d := x.(type) {
			case *ast.TypeSpec:
				if sel, ok := d.Type.(*ast.SelectorExpr); ok && d.Assign.IsValid() && isPorts(sel) {
					t.Errorf("%s: type %s = ports.%s duplica un nombre de ports", fset.Position(d.Pos()), d.Name, sel.Sel.Name)
				}
			case *ast.ValueSpec:
				for i, v := range d.Values {
					if sel, ok := v.(*ast.SelectorExpr); ok && isPorts(sel) && sel.Sel.Name == d.Names[i].Name {
						t.Errorf("%s: %s = ports.%s duplica un nombre de ports", fset.Position(d.Pos()), d.Names[i], sel.Sel.Name)
					}
				}
			}
			return true
		})
	}
}

func isPorts(sel *ast.SelectorExpr) bool {
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "ports"
}
