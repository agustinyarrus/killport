//go:build windows

// escuchador es el fixture de las pruebas de punta a punta: un
// servidor TCP que escucha en UNA dirección (127.0.0.1, ::1, 0.0.0.0 o ::,
// siempre de una sola familia: [::] es solo IPv6) y a cada conexión le
// contesta su etiqueta.
//
//	-dir DIRECCIÓN -puerto N (0 = cualquiera) -etiqueta X [-marca ARCHIVO]
//	-huerfano: se relanza desprendido (sin consola, en otro grupo) con los
//	  mismos datos y termina enseguida: el hijo queda escuchando SIN PADRE.
//
// Escribe "PUERTO n PID m" en la salida estándar, o en -marca si la hay. No
// termina solo: lo mata la prueba.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func main() {
	dir := flag.String("dir", "127.0.0.1", "dirección donde escuchar")
	puerto := flag.Int("puerto", 0, "puerto (0 = cualquiera)")
	etiqueta := flag.String("etiqueta", "?", "lo que contesta")
	marca := flag.String("marca", "", "archivo donde anotar el puerto")
	huerfano := flag.Bool("huerfano", false, "relanzarse desprendido y terminar")
	flag.Parse()

	if *huerfano {
		cmd := exec.Command(os.Args[0], "-dir", *dir, "-puerto", strconv.Itoa(*puerto), "-etiqueta", *etiqueta, "-marca", *marca)
		// DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP: sin consola y sin nada
		// que lo ate a este proceso, que termina ya.
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200, HideWindow: true}
		if err := cmd.Start(); err != nil {
			fmt.Println("ERROR", err)
			os.Exit(1)
		}
		fmt.Printf("HIJO %d\n", cmd.Process.Pid)
		cmd.Process.Release()
		return
	}

	red := "tcp4"
	if ip := net.ParseIP(*dir); ip != nil && ip.To4() == nil {
		red = "tcp6"
	}
	l, err := net.Listen(red, net.JoinHostPort(*dir, strconv.Itoa(*puerto)))
	if err != nil {
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
	linea := fmt.Sprintf("PUERTO %d PID %d\n", l.Addr().(*net.TCPAddr).Port, os.Getpid())
	if *marca != "" {
		os.WriteFile(*marca, []byte(linea), 0o644)
	} else {
		fmt.Print(linea)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Write([]byte(*etiqueta))
			c.Close()
		}
	}()
	for {
		time.Sleep(time.Hour)
	}
}
