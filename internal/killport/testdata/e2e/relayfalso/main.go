//go:build windows

// relayfalso es el DOBLE de un intermediario para las pruebas de punta a
// punta de killport. Se compila con el nombre de la imagen que imita
// (com.docker.backend.exe o wslrelay.exe): la tabla de sockets de Windows lo
// muestra así, igual que al de verdad.
//
//	-modo docker: escucha un puerto en loopback como si publicara el de un
//	  contenedor, y sirve la API de Docker en un named pipe con ESE
//	  contenedor. Un POST /containers/{id}/stop "detiene" el contenedor:
//	  suelta el puerto y lo anota ("STOP t=N" en la salida). Sigue vivo.
//	-modo wsl: escucha un puerto como wslrelay y lo suelta cuando muere el
//	  proceso -winpid (el "Linux" de atrás). Con -winpid 0 no lo suelta
//	  nunca: un relay colgado. Sigue vivo.
//
// Escribe "PUERTO n" apenas escucha. No termina solo: lo mata la prueba.
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agustinyarrus/killport/internal/win/pipetest"
)

func main() {
	modo := flag.String("modo", "docker", "docker | wsl")
	pipe := flag.String("pipe", "", "named pipe de la API (docker)")
	id := flag.String("id", "", "id del contenedor (docker)")
	winpid := flag.Int("winpid", 0, "proceso cuya muerte suelta el puerto (wsl)")
	flag.Parse()

	// v4 y v6 en loopback (no dispara el Firewall), el mismo puerto.
	l4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
	port := l4.Addr().(*net.TCPAddr).Port
	l6, _ := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	var once sync.Once
	soltar := func() {
		once.Do(func() {
			l4.Close()
			if l6 != nil {
				l6.Close()
			}
			fmt.Println("SOLTADO")
		})
	}
	fmt.Printf("PUERTO %d\n", port)

	switch *modo {
	case "docker":
		servirDocker(*pipe, *id, port, soltar)
	case "wsl":
		if *winpid != 0 {
			esperarMuerte(uint32(*winpid))
			soltar()
		}
	}
	// Vivo hasta que lo maten: un intermediario no se va solo. (Un select{}
	// pelado no sirve: sin otra goroutine viva, el runtime lo toma como
	// bloqueo mutuo y termina el proceso.)
	for {
		time.Sleep(time.Hour)
	}
}

func servirDocker(path, id string, port int, soltar func()) {
	l, err := pipetest.Listen(path, pipetest.Options{})
	if err != nil {
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
	var mu sync.Mutex
	parado := false
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/_ping":
			w.Header().Set("Api-Version", "1.47")
			io.WriteString(w, "OK")
		case r.URL.Path == "/containers/json":
			if parado {
				io.WriteString(w, "[]")
				return
			}
			fmt.Fprintf(w, `[{"Id":%q,"Names":["/shop-db-1"],"Image":"postgres:16","State":"running",`+
				`"Labels":{"com.docker.compose.project":"shop","com.docker.compose.service":"db"},`+
				`"Ports":[{"IP":"127.0.0.1","PrivatePort":5432,"PublicPort":%d,"Type":"tcp"},{"IP":"::1","PrivatePort":5432,"PublicPort":%d,"Type":"tcp"}]}]`, id, port, port)
		case r.URL.Path == "/containers/"+id+"/json":
			fmt.Fprintf(w, `{"State":{"Running":%v,"ExitCode":0,"OOMKilled":false},"HostConfig":{"RestartPolicy":{"Name":"unless-stopped"}}}`, !parado)
		case r.Method == http.MethodPost && r.URL.Path == "/containers/"+id+"/stop":
			fmt.Printf("STOP t=%s\n", r.URL.Query().Get("t"))
			if parado {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			parado = true
			soltar()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"no existe: `+strings.ReplaceAll(r.URL.Path, `"`, "")+`"}`)
		}
	}))
}

// esperarMuerte espera, sin sondear, a que el proceso salga.
func esperarMuerte(pid uint32) {
	k := syscall.NewLazyDLL("kernel32.dll")
	h, _, _ := k.NewProc("OpenProcess").Call(0x00100000 /* SYNCHRONIZE */, 0, uintptr(pid))
	if h == 0 {
		return // ya no existe
	}
	k.NewProc("WaitForSingleObject").Call(h, 0xFFFFFFFF)
	syscall.CloseHandle(syscall.Handle(h))
}
