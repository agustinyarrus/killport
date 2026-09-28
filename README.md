# killport

Para cuando un servidor de desarrollo quedó ocupando un puerto, o algo dice "el puerto ya está en uso" y no se sabe quién. Dice qué lo ocupa —un proceso, un servicio, un contenedor, un proceso dentro de WSL— y lo cierra como lo harías a mano: Ctrl+C en su ventana y, solo si no cede, en seco. Después comprueba que el puerto haya quedado libre de verdad.

Un único `.exe` portable para Windows, escrito en Go puro: sin instalador, sin dependencias, sin nube. Antes vivía en navaja, la suite de herramientas de consola para Windows; ahora es un proyecto propio.

```powershell
killport                          # qué escucha en cada puerto, con su proyecto (oculta lo de Windows)
killport 3000                     # quién tiene el 3000; pregunta y lo cierra
killport localhost:5173 -y        # solo el que contesta localhost, sin preguntar
killport 8080                     # si lo publica un contenedor, detiene el contenedor
killport --orphans -n node        # los node que quedaron escuchando después de cerrar la terminal
$port = killport --free 3000      # el primer puerto libre de verdad desde el 3000
killport 5173 --wait 60s          # espera a que el servidor escuche (para scripts)
```

- **Cierre prolijo y por identidad**: un Ctrl+C por consola, que un ayudante manda solo si todos los que la comparten se pueden cerrar; la salida se espera sin sondear (avisa el kernel) y recién al vencer `--timeout` se termina en seco. Todo se refiere a PID + hora de creación: un PID reciclado nunca se toca. Nunca cierra la terminal desde donde lo corrés ni un proceso crítico, y un "svchost" que no está en System32 sale marcado como posible impostor.
- **Lo que hay detrás del dueño**: si el puerto figura a nombre de Docker Desktop, detiene el contenedor por la API del motor; si es `wslrelay`, cierra el proceso Linux que escucha adentro (SIGINT, SIGTERM y recién después SIGKILL); si es System, busca qué programa registró la URL en http.sys; si es el IP Helper, la regla de `portproxy`. Un servicio (SQL Server, PostgreSQL) se detiene como servicio —si no, Windows lo revive— y con `--manual` no vuelve al reiniciar.
- **Verifica, no supone**: relee la tabla hasta que el puerto quede libre y se sostenga así. Si otro proceso lo retoma, nombra al supervisor que lo relanza (nodemon, pm2) y ofrece cerrarlo; si un hijo heredó el socket del que murió, lo encuentra.
- **localhost no es 127.0.0.1**: dos procesos pueden escuchar el mismo puerto en `[::1]` y en `0.0.0.0` sin que ninguno dé error (el clásico "el navegador muestra la versión vieja"). El listado los marca, dice a cuál llega cada destino, y `killport localhost:5173` apunta solo a ese.
- **Puertos que reserva Windows**: distingue un puerto ocupado de uno excluido por WinNAT o Hyper-V (lo confirma con un bind exclusivo), y `--reserve` lo reserva para vos; `--fix-dynamic` arregla un rango dinámico corrido. Cambian la red de la máquina: muestran el plan y preguntan siempre.
- **Para scripts**: `--json` estable y con el mismo código de salida que el texto; `--free` sin consola imprime solo el número; `--wait` y `--wait-free` reemplazan a wait-on sin conectarse a nadie.
- **Administrador, solo si hace falta**: lo que lo pide (detener un servicio, borrar una regla, reservar) se ofrece por el UAC. El proceso elevado recibe una selección, no órdenes: vuelve a mirar el sistema y solo toca lo que su propio plan permite.

## Instalar

Con [Go](https://go.dev/dl) 1.24 o más nuevo (el `go.mod` pide 1.26 y Go descarga solo esa versión la primera vez):

```powershell
go install github.com/agustinyarrus/killport@latest
```

O desde el repo:

```powershell
.\build.ps1          # compila a dist\killport.exe (con versión, commit y SHA256)
.\build.ps1 -Test    # antes corre go vet y todos los tests
```

`killport` usa Docker Desktop (o Podman) y WSL si están instalados, para ver qué contenedor o proceso de Linux hay detrás de un puerto; sin ellos anda igual, y nunca arranca ni una distro ni el motor.

Opciones comunes: `-h/--help`, `-V/--version`, `--no-color` (también respeta la variable `NO_COLOR`). Si la salida no es una consola (pipe o archivo), no se emite ningún escape de color.

Códigos de salida: `0` listo (o ya estaba libre) · `1` una parte · `2` línea de comandos inválida · `3` no se pudo (o venció la espera) · `130` cancelado con Ctrl+C. Con `--json`, los mismos.

## Cómo se verifica

Cada capa se contrasta con una fuente independiente, no consigo misma: netstat, `Win32_Process`, `Win32_Service`, `CommandLineToArgvW`, el CLI de docker, `ss` y `pgrep` dentro de WSL, y el marcador que deja un servidor al recibir SIGINT. Sockets 42 de 42 y 109 de 109; procesos 417 de 417; servicios 147 de 147; 270 de 270 verificaciones de punta a punta contra la máquina (80 con Docker y WSL de verdad); 290 de 290 mutaciones atrapadas.

Además hay 590 tests de Go (`go test ./...`), con un simulador de eventos discretos de reloj virtual y propiedades contra implementaciones ingenuas. El detalle: [docs/VERIFICACION.md](docs/VERIFICACION.md). La arquitectura y las decisiones de diseño: [docs/ARQUITECTURA.md](docs/ARQUITECTURA.md). Lo que falta: [docs/PENDIENTE.md](docs/PENDIENTE.md).

## Estructura

```
main.go               abre la consola y llama a killport.Main
internal/
  killport/           la herramienta: flags, dominios (Docker, WSL, http.sys, portproxy, servicios), salida, trabajo elevado
  reaper/             el algoritmo, puro: identidad, bosque de procesos, plan, escalada, verificación, espera
  ports/              puro: conjuntos de puertos, direcciones, puerto libre, parsers (netsh, ss, http.sys)
  win/                la API de Windows: sockets, procesos, servicios, consola ajena, ejecutor acotado
    pipeio/           named pipes overlapped con la forma de un net.Conn (la API de Docker)
    desk/             modo y tamaño de la consola propia
  tui/                consola: paleta, progreso vivo, tarjetas, tablas, formato es-AR
  cli/                flags estilo GNU, ayuda, sugerencias "¿quisiste decir…?"
  textdist/           distancia de edición para las sugerencias
  version/            versión y commit estampados por build.ps1
docs/                 arquitectura, verificación y pendientes
```

## Licencia

MIT.
