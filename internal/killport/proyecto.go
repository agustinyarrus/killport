package killport

// proyecto.go: ¿de qué proyecto es? Con cinco node.exe escuchando, el nombre
// y el PID no dicen nada; "shop-web · fix-ui" sí. Se parte de la carpeta de
// trabajo del proceso (win.QueryDetail la lee de su PEB) y se sube:
//
//   - la RAÍZ del proyecto es la carpeta más cercana con un marcador:
//     package.json, go.mod, Cargo.toml, pyproject.toml, un .csproj/.fsproj/
//     .vbproj o una .sln (en ese orden de preferencia dentro de una carpeta);
//   - el NOMBRE es el que declara el marcador (el "name" de package.json, el
//     último tramo del módulo de go.mod, el [package] de Cargo, el [project]
//     de pyproject, el del .csproj) o, si no declara, el de la carpeta;
//   - el REPOSITORIO es la carpeta más cercana con .git (una carpeta, o un
//     archivo "gitdir: …" en un worktree), y la RAMA sale de su HEAD —sin
//     ejecutar git—; con HEAD desacoplado, los 7 primeros del commit.
//
// En un monorepo el proyecto y el repositorio no coinciden: se muestra la
// cadena ("shop › web"). La raíz de una unidad y el perfil del usuario nunca
// son la raíz de un proyecto: un package.json ahí es casi siempre un `npm
// install` hecho sin querer, y convertiría en "un proyecto" a todo lo que
// arranca desde el perfil.
//
// Si la carpeta de trabajo no es un proyecto (C:\Windows\System32, el
// perfil), se prueba con la del primer argumento que es un script (.js, .ts,
// .py…) y existe: `node C:\dev\shop\server.js` lanzado desde cualquier lado
// es de shop.
//
// Puro sobre fs.FS (un volumen por unidad): se prueba con fstest.MapFS. Cada
// lectura está acotada, y lo que se averigua de una carpeta se recuerda en la
// corrida (los hermanos de un monorepo comparten los ancestros).

import (
	"bufio"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

// proyecto es lo que se sabe del proyecto de un proceso.
type proyecto struct {
	Carpeta   string // de dónde se partió: la carpeta del proceso o la de su script
	Raiz      string // la raíz del proyecto: donde está el marcador
	Nombre    string // el que declara el marcador, o el de la carpeta
	Marcador  string // package.json, go.mod, Cargo.toml, pyproject.toml, x.csproj, x.sln o .git
	Repo      string // la raíz del repositorio git que lo contiene ("" si no hay)
	Rama      string // la rama de HEAD ("" si no se supo o si está desacoplado)
	Commit    string // HEAD desacoplado: los 7 primeros caracteres del commit
	PorScript bool   // la carpeta del proceso no era de un proyecto: salió de su script
}

// corto es la columna PROYECTO: "shop-web · fix-ui".
func (p proyecto) corto() string {
	switch {
	case p.Rama != "":
		return p.Nombre + " · " + p.Rama
	case p.Commit != "":
		return p.Nombre + " · @" + p.Commit
	}
	return p.Nombre
}

// cadena es el lugar del proyecto en su repositorio: "shop › web" en un
// monorepo, o solo "web".
func (p proyecto) cadena() string {
	if p.Repo != "" && !strings.EqualFold(p.Repo, p.Raiz) {
		if rest, ok := debajoDe(p.Raiz, strings.TrimRight(p.Repo, `\`)); ok {
			return base(p.Repo) + " › " + strings.ReplaceAll(rest, `\`, " › ")
		}
	}
	return base(p.Raiz)
}

// largo es la línea del bloque: "shop › web (package.json «shop-web», rama
// fix-ui) · C:\dev\shop\web".
func (p proyecto) largo() string {
	var det []string
	if p.Marcador != ".git" && p.Nombre != base(p.Raiz) {
		det = append(det, p.Marcador+" «"+p.Nombre+"»")
	}
	switch {
	case p.Rama != "":
		det = append(det, "rama "+p.Rama)
	case p.Commit != "":
		det = append(det, "commit "+p.Commit+" (sin rama)")
	}
	s := p.cadena()
	if len(det) > 0 {
		s += " (" + strings.Join(det, ", ") + ")"
	}
	s += " · " + p.Raiz
	switch {
	case p.PorScript && p.Carpeta != "":
		s += " · por el script (arrancó en " + p.Carpeta + ")"
	case p.PorScript:
		s += " · por el script"
	}
	return s
}

// base es la última carpeta de una ruta de Windows.
func base(p string) string {
	p = strings.TrimRight(p, `\/`)
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// maxNiveles es cuánto se sube como mucho desde la carpeta de partida.
const maxNiveles = 25

// topeListado y topeArchivo acotan lo que se lee de cada carpeta y archivo.
const (
	topeListado = 2048
	topeArchivo = 1 << 20
)

// detector encuentra el proyecto de una carpeta.
type detector struct {
	vol  func(vol string) fs.FS // el sistema de archivos de un volumen ("C:", `\\srv\recurso`)
	home string                 // el perfil del usuario
	memo map[string]*proyecto   // carpeta pedida (en minúsculas) → su proyecto (nil = ninguno)
}

// nuevoDetector es el de verdad: cada volumen es os.DirFS de su raíz.
func nuevoDetector() *detector {
	return &detector{
		vol:  func(v string) fs.FS { return os.DirFS(v + `\`) },
		home: os.Getenv("USERPROFILE"),
		memo: map[string]*proyecto{},
	}
}

// de busca el proyecto de la carpeta dir (absoluta). ok=false si dir no está
// dentro de ninguno.
func (d *detector) de(dir string) (proyecto, bool) {
	key := strings.ToLower(strings.TrimRight(dir, `\/`))
	if p, seen := d.memo[key]; seen {
		if p == nil {
			return proyecto{}, false
		}
		return *p, true
	}
	p, ok := d.buscar(dir)
	if ok {
		d.memo[key] = &p
	} else {
		d.memo[key] = nil
	}
	return p, ok
}

func (d *detector) buscar(dir string) (proyecto, bool) {
	vol, comps, ok := partirRuta(dir)
	if !ok {
		return proyecto{}, false
	}
	fsys := d.vol(vol)
	p := proyecto{Carpeta: unirRuta(vol, comps)}
	for i, n := len(comps), 0; i >= 0 && n <= maxNiveles; i, n = i-1, n+1 {
		aca := unirRuta(vol, comps[:i])
		if d.noEsRaiz(aca) {
			continue
		}
		rel := rutaFS(comps[:i])
		nombres := listar(fsys, rel)
		if p.Raiz == "" {
			if m, nombre := marcador(fsys, rel, nombres); m != "" {
				p.Raiz, p.Marcador, p.Nombre = aca, m, nombre
			}
		}
		if git, ok := nombres[".git"]; ok {
			p.Repo = aca
			p.Rama, p.Commit = d.head(fsys, vol, comps[:i], git)
			break
		}
	}
	switch {
	case p.Raiz == "" && p.Repo == "":
		return proyecto{}, false
	case p.Raiz == "":
		p.Raiz, p.Marcador = p.Repo, ".git"
	}
	if p.Nombre == "" {
		p.Nombre = base(p.Raiz)
	}
	return p, true
}

// noEsRaiz: la raíz de una unidad y el perfil del usuario no son proyectos.
func (d *detector) noEsRaiz(dir string) bool {
	return esRaiz(dir) || strings.HasPrefix(dir, `\\`) && strings.Count(strings.Trim(dir, `\`), `\`) <= 1 || (d.home != "" && mismaRuta(dir, d.home))
}

// existe dice si hay un archivo en esa ruta absoluta.
func (d *detector) existe(p string) bool {
	vol, comps, ok := partirRuta(p)
	if !ok || len(comps) == 0 {
		return false
	}
	st, err := fs.Stat(d.vol(vol), rutaFS(comps))
	return err == nil && !st.IsDir()
}

// partirRuta separa una ruta absoluta de Windows en su volumen y sus
// carpetas: "C:\dev\shop" → ("C:", [dev shop]); `\\srv\rec\x` → (`\\srv\rec`,
// [x]). Acepta las dos barras. ok=false si no es absoluta.
func partirRuta(p string) (vol string, comps []string, ok bool) {
	p = strings.ReplaceAll(p, "/", `\`)
	switch {
	case strings.HasPrefix(p, `\\`):
		parts := strings.SplitN(strings.TrimPrefix(p, `\\`), `\`, 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return "", nil, false
		}
		vol = `\\` + parts[0] + `\` + parts[1]
		if len(parts) == 3 {
			p = parts[2]
		} else {
			p = ""
		}
	case len(p) >= 2 && p[1] == ':' && esLetra(p[0]) && (len(p) == 2 || p[2] == '\\'):
		vol, p = strings.ToUpper(p[:1])+":", strings.TrimPrefix(p[2:], `\`)
	default:
		return "", nil, false
	}
	for _, c := range strings.Split(p, `\`) {
		switch c {
		case "", ".":
		case "..":
			if len(comps) > 0 {
				comps = comps[:len(comps)-1]
			}
		default:
			comps = append(comps, c)
		}
	}
	return vol, comps, true
}

// unirRuta es la inversa de partirRuta.
func unirRuta(vol string, comps []string) string {
	if len(comps) == 0 {
		return vol + `\`
	}
	return vol + `\` + strings.Join(comps, `\`)
}

// rutaFS es la ruta dentro del fs.FS del volumen.
func rutaFS(comps []string) string {
	if len(comps) == 0 {
		return "."
	}
	return strings.Join(comps, "/")
}

// listar da los nombres de una carpeta (en minúsculas → como están), a lo
// sumo topeListado. Una carpeta ilegible no tiene nada.
func listar(fsys fs.FS, dir string) map[string]string {
	out := map[string]string{}
	f, err := fsys.Open(dir)
	if err != nil {
		return out
	}
	defer f.Close()
	rd, ok := f.(fs.ReadDirFile)
	if !ok {
		return out
	}
	es, _ := rd.ReadDir(topeListado)
	for _, e := range es {
		out[strings.ToLower(e.Name())] = e.Name()
	}
	return out
}

// marcador dice qué marcador de proyecto hay en la carpeta, y el nombre que
// declara ("" si no declara ninguno).
func marcador(fsys fs.FS, dir string, nombres map[string]string) (string, string) {
	leer := func(n string) string { return leerAcotado(fsys, path.Join(dir, n)) }
	for _, m := range []struct {
		archivo string
		nombre  func(string) string
	}{
		{"package.json", nombrePackageJSON},
		{"go.mod", nombreGoMod},
		{"cargo.toml", func(s string) string { return nombreTOML(s, "package") }},
		{"pyproject.toml", func(s string) string {
			if n := nombreTOML(s, "project"); n != "" {
				return n
			}
			return nombreTOML(s, "tool.poetry")
		}},
	} {
		if real, ok := nombres[m.archivo]; ok {
			return real, m.nombre(leer(real))
		}
	}
	// Un proyecto de .NET: el primero por orden alfabético, para que la
	// respuesta no dependa del orden del listado.
	for _, ext := range []string{".csproj", ".fsproj", ".vbproj", ".sln"} {
		mejor := ""
		for low, real := range nombres {
			if strings.HasSuffix(low, ext) && len(low) > len(ext) && (mejor == "" || strings.ToLower(real) < strings.ToLower(mejor)) {
				mejor = real
			}
		}
		if mejor != "" {
			return mejor, mejor[:len(mejor)-len(ext)]
		}
	}
	return "", ""
}

// leerAcotado lee un archivo hasta topeArchivo bytes ("" si no se pudo).
func leerAcotado(fsys fs.FS, name string) string {
	f, err := fsys.Open(name)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, topeArchivo))
	return string(b)
}

func nombrePackageJSON(s string) string {
	var pj struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(s), &pj) != nil {
		return ""
	}
	return strings.TrimSpace(pj.Name)
}

// nombreGoMod es el último tramo del módulo, sin el sufijo de versión mayor
// (github.com/x/shop/v2 → shop).
func nombreGoMod(s string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		mod, ok := strings.CutPrefix(l, "module")
		if !ok || (mod != "" && mod[0] != ' ' && mod[0] != '\t') {
			continue
		}
		if i := strings.Index(mod, "//"); i >= 0 {
			mod = mod[:i]
		}
		mod = strings.Trim(strings.TrimSpace(mod), "\"`")
		parts := strings.Split(mod, "/")
		last := parts[len(parts)-1]
		if len(parts) > 1 && esVersionMayor(last) {
			last = parts[len(parts)-2]
		}
		return last
	}
	return ""
}

func esVersionMayor(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// nombreTOML busca `name = "…"` dentro de la tabla [tabla] de un TOML. Lee
// lo justo: encabezados de tabla y asignaciones simples de una línea.
func nombreTOML(s, tabla string) string {
	sc := bufio.NewScanner(strings.NewReader(s))
	dentro := false
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "[") {
			dentro = strings.TrimSpace(strings.Trim(l, "[]")) == tabla
			continue
		}
		if !dentro {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok || strings.TrimSpace(k) != "name" {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) < 2 || (v[0] != '"' && v[0] != '\'') {
			continue
		}
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			return v[1 : 1+end]
		}
	}
	return ""
}

// head lee la rama de un repositorio: .git es una carpeta (HEAD adentro) o,
// en un worktree o un submódulo, un archivo "gitdir: RUTA" que dice dónde
// está la carpeta de verdad (relativa al archivo, o absoluta; puede estar en
// otra unidad).
func (d *detector) head(fsys fs.FS, vol string, repo []string, git string) (rama, commit string) {
	gitRel := path.Join(rutaFS(repo), git)
	st, err := fs.Stat(fsys, gitRel)
	if err != nil {
		return "", ""
	}
	if st.IsDir() {
		return parseHEAD(leerAcotado(fsys, path.Join(gitRel, "HEAD")))
	}
	linea := strings.TrimSpace(strings.SplitN(leerAcotado(fsys, gitRel), "\n", 2)[0])
	dir, ok := strings.CutPrefix(linea, "gitdir:")
	if !ok {
		return "", ""
	}
	dir = strings.TrimSpace(dir)
	if !esRutaAbsoluta(dir) && !strings.HasPrefix(dir, `\\`) {
		dir = unirRuta(vol, repo) + `\` + dir
	}
	v, comps, ok := partirRuta(dir)
	if !ok {
		return "", ""
	}
	return parseHEAD(leerAcotado(d.vol(v), path.Join(rutaFS(comps), "HEAD")))
}

// parseHEAD interpreta el contenido de HEAD: "ref: refs/heads/RAMA", o el
// hash del commit (40 o 64 caracteres hexadecimales) si está desacoplado.
func parseHEAD(s string) (rama, commit string) {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if ref, ok := strings.CutPrefix(s, "ref:"); ok {
		ref = strings.TrimSpace(ref)
		if r, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			return r, ""
		}
		return strings.TrimPrefix(ref, "refs/"), ""
	}
	if (len(s) == 40 || len(s) == 64) && strings.Trim(strings.ToLower(s), "0123456789abcdef") == "" {
		return "", strings.ToLower(s[:7])
	}
	return "", ""
}

// scriptExts son las extensiones de los scripts que un intérprete recibe
// como argumento.
var scriptExts = []string{".js", ".mjs", ".cjs", ".ts", ".mts", ".cts", ".jsx", ".tsx", ".py", ".pyw", ".rb", ".php", ".pl", ".lua"}

// scriptDe es la carpeta del primer argumento que es un script existente
// (relativo a la carpeta del proceso si no es absoluto); "" si no hay.
func (d *detector) scriptDe(argv []string, cwd string) string {
	for _, a := range argv[min(1, len(argv)):] {
		if strings.HasPrefix(a, "-") {
			continue
		}
		low := strings.ToLower(a)
		es := false
		for _, e := range scriptExts {
			es = es || strings.HasSuffix(low, e)
		}
		if !es {
			continue
		}
		p := strings.ReplaceAll(a, "/", `\`)
		if !esRutaAbsoluta(p) {
			if cwd == "" {
				return ""
			}
			p = strings.TrimRight(cwd, `\`) + `\` + p
		}
		if d.existe(p) {
			if i := strings.LastIndex(p, `\`); i > 0 {
				return p[:i]
			}
		}
		return ""
	}
	return ""
}

// delProceso es el proyecto de un proceso: el de su carpeta de trabajo, o el
// de su script.
func (d *detector) delProceso(cwd, cmdline string) (proyecto, bool) {
	if cwd != "" {
		if p, ok := d.de(cwd); ok {
			return p, true
		}
	}
	if dir := d.scriptDe(argvDe(strings.TrimSpace(cmdline)), cwd); dir != "" {
		if p, ok := d.de(dir); ok {
			p.PorScript, p.Carpeta = true, cwd
			return p, true
		}
	}
	return proyecto{}, false
}
