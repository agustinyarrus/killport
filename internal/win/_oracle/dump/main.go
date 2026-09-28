//go:build windows

// dump imprime las tablas de sockets, los procesos, los servicios y la
// elevación tal como los ve internal/win, en JSON, para contrastar contra los
// oráculos de PowerShell (Get-NetTCPConnection, Win32_Process, Win32_Service).
// Vive bajo _oracle/ para que `go build ./...` no lo compile.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/agustinyarrus/killport/internal/win"
)

func main() {
	what := "sockets"
	if len(os.Args) > 1 {
		what = os.Args[1]
	}
	enc := json.NewEncoder(os.Stdout)
	switch what {
	case "sockets":
		socks, err := win.AllSockets()
		must(err)
		type row struct {
			Proto, Local, Remote string
			State, PID           uint32
			Created              string
		}
		var out []row
		for _, s := range socks {
			created := ""
			if !s.Created.IsZero() {
				created = s.Created.UTC().Format("2006-01-02T15:04:05.0000000Z")
			}
			rem := ""
			if s.Remote.IsValid() {
				rem = s.Remote.String()
			}
			out = append(out, row{s.Proto.String(), s.Local.String(), rem, s.State, s.PID, created})
		}
		enc.Encode(out)
	case "procs":
		procs, err := win.AllProcesses()
		must(err)
		type row struct {
			PID, PPID, Session uint32
			Image, Created     string
		}
		var out []row
		for _, p := range procs {
			created := ""
			if !p.Created.IsZero() {
				created = p.Created.UTC().Format("2006-01-02T15:04:05.0000000Z")
			}
			out = append(out, row{p.PID, p.PPID, p.SessionID, p.Image, created})
		}
		enc.Encode(out)
	case "svcs":
		svcs, err := win.Services()
		must(err)
		enc.Encode(svcs)
	case "elevated":
		fmt.Println(win.IsElevated())
	case "svc-stop": // svc-stop NAME: solo para probar los CAMINOS DE ERROR
		name := os.Args[2]
		err := win.StopService(context.Background(), name, time.Now().Add(3*time.Second))
		fmt.Printf("StopService(%q): err=%v · esAccesoDenegado=%v · esInexistente=%v · esDependientes=%v\n",
			name, err, errors.Is(err, win.ErrAccessDenied), errors.Is(err, win.ErrServiceNotFound), errors.Is(err, win.ErrServiceDependents))
	case "detail": // detail PID: ruta, línea de comandos, crítico
		pid := atou(os.Args[2])
		d, err := win.QueryDetail(pid, time.Time{})
		must(err)
		enc.Encode(d)
	case "owner": // owner PORT: el módulo/servicio dueño del primer socket en ese puerto
		port := uint16(atou(os.Args[2]))
		socks, err := win.AllSockets()
		must(err)
		for _, s := range socks {
			if s.Local.Port() == port {
				m, err := s.Owner()
				fmt.Printf("%s %s pid=%d owner=%q path=%q err=%v\n", s.Proto, s.Local, s.PID, m.Name, m.Path, err)
			}
		}
	case "bind": // bind PORT: sondeo TCP y UDP, IPv4
		port := uint16(atou(os.Args[2]))
		for _, tcp := range []bool{true, false} {
			st, err := win.ProbeBind(port, tcp, false)
			name := map[win.BindStatus]string{win.BindFree: "libre", win.BindInUse: "en uso", win.BindForbidden: "prohibido"}[st]
			proto := "UDP"
			if tcp {
				proto = "TCP"
			}
			fmt.Printf("%s %d: %s (err=%v)\n", proto, port, name, err)
		}
	default:
		fmt.Fprintln(os.Stderr, "uso: dump [sockets|procs|svcs|elevated|detail PID|owner PORT|bind PORT]")
		os.Exit(2)
	}
}

func atou(s string) uint32 {
	var n uint32
	fmt.Sscan(s, &n)
	return n
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
