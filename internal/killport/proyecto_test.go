package killport

import (
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func archivo(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

// unidades arma un detector sobre dos unidades falsas.
func unidades() *detector {
	c := fstest.MapFS{
		// Un monorepo: el repo en shop, un proyecto en web.
		"dev/shop/.git/HEAD":            archivo("ref: refs/heads/fix-ui\n"),
		"dev/shop/web/package.json":     archivo(`{"name": "shop-web", "version": "1.0.0"}`),
		"dev/shop/web/src/index.ts":     archivo("//"),
		"dev/shop/web/server.js":        archivo("//"),
		"dev/shop/api/go.mod":           archivo("// comentario\nmodule github.com/acme/api/v2 // el módulo\n\ngo 1.26\n"),
		"dev/shop/motor/Cargo.toml":     archivo("[workspace]\nname = \"no\"\n\n[package]\nname = 'motor'\nversion = \"0.1.0\"\n"),
		"dev/shop/ml/pyproject.toml":    archivo("[build-system]\nrequires = []\n[tool.poetry]\nname = \"ml-lab\"\n"),
		"dev/shop/tienda/Tienda.csproj": archivo("<Project/>"),
		"dev/shop/tienda/Otro.sln":      archivo(""),
		"dev/shop/roto/package.json":    archivo(`{"name": `),
		// Un repo sin marcadores, con HEAD desacoplado.
		"dev/suelto/.git/HEAD": archivo("0123456789abcdef0123456789abcdef01234567\n"),
		"dev/suelto/a/b/c.txt": archivo(""),
		// Worktrees: uno apunta a otra unidad (absoluta, con barras de Unix), otro relativo.
		"dev/shop-feat/.git":               archivo("gitdir: D:/repos/shop/.git/worktrees/feat\n"),
		"dev/shop-feat/package.json":       archivo(`{"name": "shop"}`),
		"dev/shop-hot/.git":                archivo("gitdir: ../shop/.git/worktrees/hot"),
		"dev/shop/.git/worktrees/hot/HEAD": archivo("ref: refs/heads/hotfix/login"),
		// El perfil y la raíz no son proyectos, aunque tengan marcadores.
		"Users/u/package.json": archivo(`{"name": "accidental"}`),
		"Users/u/.git/HEAD":    archivo("ref: refs/heads/dotfiles"),
		"Users/u/dev/x.txt":    archivo(""),
		".git/HEAD":            archivo("ref: refs/heads/raiz"),
		"Windows/System32/x":   archivo(""),
	}
	d := fstest.MapFS{
		"repos/shop/.git/worktrees/feat/HEAD": archivo("ref: refs/heads/feat\n"),
	}
	return &detector{
		vol: func(v string) fs.FS {
			switch v {
			case "C:":
				return c
			case "D:":
				return d
			}
			return fstest.MapFS{}
		},
		home: `C:\Users\u`,
		memo: map[string]*proyecto{},
	}
}

func TestProyectoDe(t *testing.T) {
	d := unidades()
	casos := []struct {
		dir                                string
		raiz, nombre, marcador, repo, rama string
		commit, corto                      string
	}{
		{`C:\dev\shop\web\src`, `C:\dev\shop\web`, "shop-web", "package.json", `C:\dev\shop`, "fix-ui", "", "shop-web · fix-ui"},
		{`C:\dev\shop\api`, `C:\dev\shop\api`, "api", "go.mod", `C:\dev\shop`, "fix-ui", "", "api · fix-ui"},
		{`c:/dev/shop/motor/`, `C:\dev\shop\motor`, "motor", "Cargo.toml", `C:\dev\shop`, "fix-ui", "", "motor · fix-ui"},
		{`C:\dev\shop\ml`, `C:\dev\shop\ml`, "ml-lab", "pyproject.toml", `C:\dev\shop`, "fix-ui", "", "ml-lab · fix-ui"},
		{`C:\dev\shop\tienda`, `C:\dev\shop\tienda`, "Tienda", "Tienda.csproj", `C:\dev\shop`, "fix-ui", "", "Tienda · fix-ui"},
		{`C:\dev\shop\roto`, `C:\dev\shop\roto`, "roto", "package.json", `C:\dev\shop`, "fix-ui", "", "roto · fix-ui"},
		{`C:\dev\shop`, `C:\dev\shop`, "shop", ".git", `C:\dev\shop`, "fix-ui", "", "shop · fix-ui"},
		{`C:\dev\suelto\a\b`, `C:\dev\suelto`, "suelto", ".git", `C:\dev\suelto`, "", "0123456", "suelto · @0123456"},
		{`C:\dev\shop-feat`, `C:\dev\shop-feat`, "shop", "package.json", `C:\dev\shop-feat`, "feat", "", "shop · feat"},
		{`C:\dev\shop-hot\x`, `C:\dev\shop-hot`, "shop-hot", ".git", `C:\dev\shop-hot`, "hotfix/login", "", "shop-hot · hotfix/login"},
		{`C:\dev\shop\web\..\api`, `C:\dev\shop\api`, "api", "go.mod", `C:\dev\shop`, "fix-ui", "", "api · fix-ui"},
	}
	for _, c := range casos {
		p, ok := d.de(c.dir)
		got := []string{p.Raiz, p.Nombre, p.Marcador, p.Repo, p.Rama, p.Commit, p.corto()}
		want := []string{c.raiz, c.nombre, c.marcador, c.repo, c.rama, c.commit, c.corto}
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("de(%q) = %q (%v), esperaba %q", c.dir, got, ok, want)
		}
	}
	for _, dir := range []string{`C:\Users\u`, `C:\Users\u\dev`, `C:\`, `C:\Windows\System32`, `relativa\x`, `D:\nada`} {
		if p, ok := d.de(dir); ok {
			t.Errorf("de(%q) = %+v: no es un proyecto", dir, p)
		}
	}
	if p, _ := d.de(`C:\dev\shop\web`); p.cadena() != "shop › web" || p.largo() != `shop › web (package.json «shop-web», rama fix-ui) · C:\dev\shop\web` {
		t.Errorf("monorepo: %q / %q", p.cadena(), p.largo())
	}
	if p, _ := d.de(`C:\dev\suelto`); p.largo() != `suelto (commit 0123456 (sin rama)) · C:\dev\suelto` {
		t.Errorf("desacoplado: %q", p.largo())
	}
	if len(d.memo) == 0 {
		t.Error("lo averiguado se recuerda")
	}
}

func TestProyectoPorScript(t *testing.T) {
	d := unidades()
	p, ok := d.delProceso(`C:\Windows\System32`, `"C:\Program Files\nodejs\node.exe" --inspect C:\dev\shop\web\server.js 3000`)
	if !ok || p.Nombre != "shop-web" || !p.PorScript || p.Carpeta != `C:\Windows\System32` {
		t.Fatalf("por el script: %+v %v", p, ok)
	}
	if want := `shop › web (package.json «shop-web», rama fix-ui) · C:\dev\shop\web · por el script (arrancó en C:\Windows\System32)`; p.largo() != want {
		t.Errorf("largo = %q", p.largo())
	}
	// Relativo a la carpeta del proceso.
	if p, ok := d.delProceso(`C:\dev\shop\web`, `node server.js`); !ok || p.PorScript || p.Nombre != "shop-web" {
		t.Errorf("la carpeta ya es el proyecto: %+v", p)
	}
	// Un script que no existe, o que no es un script: nada.
	for _, cmd := range []string{`node C:\dev\shop\web\no-existe.js`, `node --version`, `python -m http.server 8000`, ``} {
		if p, ok := d.delProceso(`C:\Users\u`, cmd); ok {
			t.Errorf("%q: %+v", cmd, p)
		}
	}
}

func TestPartirRuta(t *testing.T) {
	casos := []struct {
		in    string
		vol   string
		comps []string
		ok    bool
	}{
		{`C:\dev\shop`, "C:", []string{"dev", "shop"}, true},
		{`c:/dev/./x/../shop/`, "C:", []string{"dev", "shop"}, true},
		{`C:`, "C:", nil, true},
		{`C:\`, "C:", nil, true},
		{`\\srv\rec\a\b`, `\\srv\rec`, []string{"a", "b"}, true},
		{`\\srv\rec`, `\\srv\rec`, nil, true},
		{`\\srv`, "", nil, false},
		{`relativa\x`, "", nil, false},
		{`C:relativa`, "", nil, false},
	}
	for _, c := range casos {
		vol, comps, ok := partirRuta(c.in)
		if vol != c.vol || !reflect.DeepEqual(comps, c.comps) || ok != c.ok {
			t.Errorf("partirRuta(%q) = %q %q %v", c.in, vol, comps, ok)
		}
	}
	if got := unirRuta("C:", []string{"dev", "shop"}); got != `C:\dev\shop` {
		t.Errorf("unirRuta = %q", got)
	}
}

func TestParseHEAD(t *testing.T) {
	casos := map[string][2]string{
		"ref: refs/heads/main\n":                   {"main", ""},
		"ref: refs/heads/feat/x":                   {"feat/x", ""},
		"ref: refs/remotes/origin/main":            {"remotes/origin/main", ""},
		"ABCDEF0123456789ABCDEF0123456789ABCDEF01": {"", "abcdef0"},
		"basura": {"", ""},
		"":       {"", ""},
	}
	for in, want := range casos {
		if r, c := parseHEAD(in); r != want[0] || c != want[1] {
			t.Errorf("parseHEAD(%q) = %q %q", in, r, c)
		}
	}
}

// S-01: la raíz de una unidad, el perfil y lo que lo contiene, y la carpeta de
// Windows no son un proyecto para --project.
func TestNoEsProyecto(t *testing.T) {
	const home, win = `C:\Users\ana`, `C:\Windows`
	for _, c := range []struct {
		dir, por string // por: un pedazo del motivo; "" = sirve
	}{
		{`C:\`, "raíz de una unidad"},
		{`C:`, "raíz de una unidad"},
		{`D:\`, "raíz de una unidad"},
		{`\\srv\rec`, "recurso compartido"},
		{`C:\Users\ana`, "tu carpeta de usuario"},
		{`c:/users/ANA/`, "tu carpeta de usuario"},
		{`C:\Users`, "contiene tu carpeta de usuario"},
		{`C:\Windows`, "carpeta de Windows"},
		{`C:\Windows\System32`, "carpeta de Windows"},
		{`C:\Users\ana\dev\shop`, ""},
		{`C:\dev`, ""},
		{`\\srv\rec\shop`, ""},
		{`C:\Users\anabel`, ""},
		{`C:\WindowsApps`, ""},
	} {
		got := noEsProyecto(c.dir, home, win)
		if (c.por == "") != (got == "") || !strings.Contains(got, c.por) {
			t.Errorf("noEsProyecto(%q) = %q, esperaba algo con %q", c.dir, got, c.por)
		}
	}
	if got := noEsProyecto(`C:\Users\ana`, "", ""); got != "" {
		t.Errorf("sin perfil conocido, una carpeta cualquiera sirve: %q", got)
	}
}
