package killport

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/agustinyarrus/killport/internal/ports"
)

func grabado(t *testing.T, nombre string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + nombre)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// La lista de Docker 27 tal cual: nombres sin "/", compose, puertos v4+v6 del
// mismo puerto, expuestos sin publicar (fuera), UDP (sí), SCTP (fuera: no hay
// tabla de Windows para eso) y un contenedor detenido (fuera).
func TestParseContainersDocker(t *testing.T) {
	m := &motor{Nombre: "Docker Desktop", Pipe: `\\.\pipe\docker_engine`}
	cs, err := parseContainers(grabado(t, "docker_containers.json"), m)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("esperaba 2 corriendo, hubo %d: %+v", len(cs), cs)
	}
	db, web := cs[0], cs[1]
	if db.Nombre != "shop-db-1" || db.Imagen != "postgres:16" || db.Proyecto != "shop" || db.Servicio != "db" || db.Motor != m {
		t.Fatalf("db = %+v", db)
	}
	if !db.publicaEn(5432, ports.TCP) || db.publicaEn(5432, ports.UDP) || !db.puertosHost().Equal(ports.Of(5432)) {
		t.Fatalf("puertos de db = %+v", db.Puertos)
	}
	if web.publicaEn(9000, ports.TCP) || web.publicaEn(7000, ports.TCP) {
		t.Fatal("el expuesto sin publicar y el SCTP no ocupan nada en el host")
	}
	if !web.publicaEn(8080, ports.TCP) || !web.publicaEn(5353, ports.UDP) || !web.puertosHost().Equal(ports.Of(5353, 8080, 8443)) {
		t.Fatalf("puertos de web = %+v", web.Puertos)
	}
	// Ordenados por puerto del host (el motor los lista en el orden de su
	// mapa interno, que cambia entre consultas): la salida es estable. Y el
	// puerto del host no es el del contenedor.
	var hosts []uint16
	for _, p := range web.Puertos {
		hosts = append(hosts, p.Publico)
	}
	if !slices.Equal(hosts, []uint16{5353, 8080, 8443}) {
		t.Fatalf("puertos de web fuera de orden: %v", hosts)
	}
	if p := web.Puertos[1]; p.Privado != 80 || p.Publico != 8080 || p.Proto != ports.TCP || p.IP != "0.0.0.0" {
		t.Fatalf("host ≠ contenedor: %+v", p)
	}
	if got := web.publicaciones(); got != "5353 → 5353/udp, 8080 → 80/tcp, 8443 → 443/tcp" {
		t.Fatalf("publicaciones = %q", got)
	}
	if got := db.publicaciones(); got != "5432 → 5432/tcp" {
		t.Fatalf("v4 y v6 del mismo puerto son una sola publicación: %q", got)
	}
	if got := db.comandoCompose(); got != "docker compose -p shop stop db" {
		t.Fatalf("compose = %q", got)
	}
	if web.comandoCompose() != "" {
		t.Fatal("sin compose no hay comando de compose")
	}
}

// Podman por la API compatible: la misma forma, IP vacía.
func TestParseContainersPodman(t *testing.T) {
	cs, err := parseContainers(grabado(t, "podman_containers.json"), &motor{Nombre: "Podman"})
	if err != nil || len(cs) != 1 || cs[0].Nombre != "api" || !cs[0].publicaEn(3000, ports.TCP) {
		t.Fatalf("podman = %+v / %v", cs, err)
	}
}

// Un id que no es hexadecimal invalida la respuesta: no se lo mete en una ruta.
func TestParseContainersIDInvalido(t *testing.T) {
	for _, id := range []string{"../../images", "c0ffee", "ABCDEF0123456789", "8dfafdbc3a40/../x", ""} {
		body := `[{"Id":"` + id + `","Names":["/x"],"State":"running","Ports":[]}]`
		if _, err := parseContainers([]byte(body), nil); err == nil {
			t.Errorf("id %q debió rechazarse", id)
		}
	}
	if _, err := parseContainers([]byte("<html>"), nil); err == nil {
		t.Fatal("una respuesta ilegible es error")
	}
	cs, err := parseContainers([]byte(`[{"Id":"0123456789ab","State":"running"}]`), nil)
	if err != nil || cs[0].Nombre != "0123456789ab" {
		t.Fatalf("sin nombre se usa el id corto: %+v %v", cs, err)
	}
}

func TestParseInspect(t *testing.T) {
	in, err := parseInspect(grabado(t, "docker_inspect_always.json"))
	if err != nil || in.HostConfig.RestartPolicy.Name != "always" || !in.State.Running {
		t.Fatalf("inspect = %+v / %v", in, err)
	}
	k, err := parseInspect(grabado(t, "docker_inspect_killed.json"))
	if err != nil || k.State.ExitCode != 137 || k.State.OOMKilled {
		t.Fatalf("inspect muerto = %+v / %v", k, err)
	}
	c := contenedor{Reinicio: "always", Motor: &motor{Nombre: "Docker Desktop"}}
	if !strings.Contains(c.cautela(), "Docker Desktop") {
		t.Fatalf("cautela = %q", c.cautela())
	}
	for _, r := range []string{"", "no", "unless-stopped", "on-failure"} {
		if (contenedor{Reinicio: r}).cautela() != "" {
			t.Errorf("con %q, un stop a mano no vuelve solo", r)
		}
	}
}

func TestAPIErrorMessage(t *testing.T) {
	if got := apiErrorMessage([]byte(`{"message":"No such container: x"}`)); got != "No such container: x" {
		t.Fatalf("mensaje = %q", got)
	}
	if got := apiErrorMessage([]byte("  texto plano \n")); got != "texto plano" {
		t.Fatalf("mensaje = %q", got)
	}
}

func TestPipeDeHost(t *testing.T) {
	for in, want := range map[string]string{
		"npipe:////./pipe/docker_engine":          `\\.\pipe\docker_engine`,
		"npipe://./pipe/dockerDesktopLinuxEngine": `\\.\pipe\dockerDesktopLinuxEngine`,
		`npipe://\\.\pipe\podman-machine-default`: `\\.\pipe\podman-machine-default`,
		"  npipe:////./pipe/docker_engine  ":      `\\.\pipe\docker_engine`,
		"npipe:////servidor/pipe/docker_engine":   "", // otra máquina: jamás
		"npipe:////./pipe/":                       "",
		"npipe:////./pipe/a/b":                    "",
		"tcp://127.0.0.1:2375":                    "",
		"ssh://yo@host":                           "",
		"unix:///var/run/docker.sock":             "",
		"":                                        "",
	} {
		got, ok := pipeDeHost(in)
		if got != want || ok != (want != "") {
			t.Errorf("pipeDeHost(%q) = %q/%v, esperaba %q", in, got, ok, want)
		}
	}
}

// Candidatos: DOCKER_HOST primero, después el contexto (su meta vive en una
// carpeta con el sha256 del nombre), después los de siempre, sin repetir; y
// un DOCKER_HOST por tcp se dice en vez de ignorarse en silencio.
func TestPipesCandidatos(t *testing.T) {
	sum := sha256.Sum256([]byte("desktop-linux"))
	metaPath := `C:\u\.docker\contexts\meta\` + hex.EncodeToString(sum[:]) + `\meta.json`
	files := map[string]string{
		`C:\u\.docker\config.json`: `{"auths":{},"currentContext":"desktop-linux"}`,
		metaPath:                   `{"Name":"desktop-linux","Metadata":{},"Endpoints":{"docker":{"Host":"npipe:////./pipe/dockerDesktopLinuxEngine","SkipTLSVerify":false}}}`,
	}
	env := entorno{home: `C:\u`, readFile: func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("no existe")
	}}
	env.getenv = func(k string) string {
		if k == "DOCKER_HOST" {
			return "npipe:////./pipe/mi_motor"
		}
		return ""
	}
	pipes, nota := pipesCandidatos(env)
	want := []string{`\\.\pipe\mi_motor`, `\\.\pipe\dockerDesktopLinuxEngine`, `\\.\pipe\docker_engine`, `\\.\pipe\podman-machine-default`}
	if strings.Join(pipes, "|") != strings.Join(want, "|") || nota != "" {
		t.Fatalf("candidatos = %v (nota %q)", pipes, nota)
	}

	env.getenv = func(k string) string {
		switch k {
		case "DOCKER_HOST":
			return "tcp://10.0.0.5:2376"
		case "DOCKER_CONTEXT":
			return "default"
		}
		return ""
	}
	pipes, nota = pipesCandidatos(env)
	if len(pipes) != 3 || !strings.Contains(nota, "tcp://10.0.0.5:2376") {
		t.Fatalf("con DOCKER_HOST tcp: %v (nota %q)", pipes, nota)
	}
}

func TestNombreMotor(t *testing.T) {
	for _, tc := range []struct {
		pipe, server string
		libpod       bool
		want         string
	}{
		{`\\.\pipe\docker_engine`, "com.docker.backend.exe", false, "Docker Desktop"},
		{`\\.\pipe\dockerDesktopLinuxEngine`, "", false, "Docker Desktop"},
		{`\\.\pipe\docker_engine`, "dockerd.exe", false, "Docker"},
		{`\\.\pipe\docker_engine`, "win-sshproxy.exe", true, "Podman"},
		{`\\.\pipe\podman-machine-default`, "", false, "Podman"},
	} {
		if got := nombreMotor(tc.pipe, tc.server, tc.libpod); got != tc.want {
			t.Errorf("%s/%s/%v = %q, esperaba %q", tc.pipe, tc.server, tc.libpod, got, tc.want)
		}
	}
}

// La política de reinicio se muestra siempre que se sepa, con lo que implica
// DESPUÉS de detenerlo; solo always merece una advertencia (es la única que
// lo vuelve a levantar, cuando arranca el motor).
func TestPoliticaReinicio(t *testing.T) {
	m := &motor{Nombre: "Docker Desktop"}
	for _, tc := range []struct{ politica, muestra, cautela string }{
		{"", "", ""},
		{"no", "no (queda detenido)", ""},
		{"always", "always", "tiene restart=always: vuelve solo la próxima vez que arranque Docker Desktop"},
		{"unless-stopped", "unless-stopped (detenido a mano, no vuelve solo)", ""},
		{"on-failure", "on-failure (detenido a mano, no vuelve solo)", ""},
		{"una-nueva", "una-nueva", ""},
	} {
		c := contenedor{Motor: m, Reinicio: tc.politica}
		if got := c.politicaReinicio(); got != tc.muestra {
			t.Errorf("%q: muestra %q, esperaba %q", tc.politica, got, tc.muestra)
		}
		if got := c.cautela(); got != tc.cautela {
			t.Errorf("%q: cautela %q, esperaba %q", tc.politica, got, tc.cautela)
		}
	}
}

// Q-02: la validación del id (antes una expresión regular) acepta lo mismo que
// ^[0-9a-f]{12,64}$ y nada más.
func TestIDContenedor(t *testing.T) {
	for s, want := range map[string]bool{
		"0123456789ab": true, "c0ffee000000c0ffee000000c0ffee000000c0ffee000000c0ffee000000c0ff": true,
		"0123456789a": false, "0123456789AB": false, "0123456789ag": false, "": false,
		"c0ffee000000c0ffee000000c0ffee000000c0ffee000000c0ffee000000c0ffe": false, // 65
		"../../x/y/z": false, "0123456789ab/stop": false, "0123456789ab\n": false,
	} {
		if got := idContenedor(s); got != want {
			t.Errorf("idContenedor(%q) = %v, esperaba %v", s, got, want)
		}
	}
}
