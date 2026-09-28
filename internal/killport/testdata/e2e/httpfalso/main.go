//go:build windows

// httpfalso es un servidor de http.sys para las pruebas de punta a punta de
// killport: le registra una URL al servidor HTTP del kernel, que es lo que
// hacen IIS, Reporting Services o un HttpListener de .NET. El puerto queda
// escuchando a nombre de System (PID 4), no de este proceso: es el caso que
// killport tiene que desenmascarar.
//
// No necesita administrador: una URL de localhost (o atada a 127.0.0.1 o a
// [::1]) la puede registrar cualquier usuario. Escribe "PUERTO n" apenas la registró, o "ERROR código"
// si http.sys no lo dejó. No termina solo: lo mata la prueba.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"
)

func main() {
	camino := flag.String("camino", "killport-e2e", "el camino de la URL")
	host := flag.String("host", "localhost", "el host de la URL: localhost, o una IP para atarla a esa dirección (127.0.0.1, [::1])")
	puerto := flag.Int("puerto", 0, "el puerto (0 = uno libre)")
	flag.Parse()

	if *puerto == 0 {
		// Un puerto libre: se lo pide prestado al sistema y se lo suelta.
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			fmt.Println("ERROR", err)
			os.Exit(1)
		}
		*puerto = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}

	api := syscall.NewLazyDLL(os.Getenv("SystemRoot") + `\System32\httpapi.dll`)
	// HTTPAPI_VERSION 1.0 son dos USHORT por valor: caben en un registro.
	const version1, servidor = 0x00000001, 0x00000001 // HTTP_INITIALIZE_SERVER
	if r, _, _ := api.NewProc("HttpInitialize").Call(version1, servidor, 0); r != 0 {
		fmt.Println("ERROR", r)
		os.Exit(1)
	}
	var cola syscall.Handle
	if r, _, _ := api.NewProc("HttpCreateHttpHandle").Call(uintptr(unsafe.Pointer(&cola)), 0); r != 0 {
		fmt.Println("ERROR", r)
		os.Exit(1)
	}
	url, _ := syscall.UTF16PtrFromString(fmt.Sprintf("http://%s:%d/%s/", *host, *puerto, *camino))
	if r, _, _ := api.NewProc("HttpAddUrl").Call(uintptr(cola), uintptr(unsafe.Pointer(url)), 0); r != 0 {
		fmt.Println("ERROR", r)
		os.Exit(1)
	}
	fmt.Printf("PUERTO %d\n", *puerto)

	// Vivo hasta que lo maten, con la cola abierta.
	for {
		time.Sleep(time.Hour)
	}
}
