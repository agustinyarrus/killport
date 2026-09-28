# killport 1.0.0

La primera versión de killport como proyecto propio. Antes vivía en navaja, la suite de herramientas de consola para Windows; el código es el que se probó ahí, ahora con su repo, su número de versión y su CI.

killport es para cuando un servidor de desarrollo quedó ocupando un puerto, o algo dice "el puerto ya está en uso" y no se sabe quién. Dice qué lo ocupa —un proceso, un servicio, un contenedor, un proceso dentro de WSL— y lo cierra como lo harías a mano: Ctrl+C en su ventana y, solo si no cede, en seco. Después comprueba que el puerto haya quedado libre de verdad.

## Qué trae

**Cerrar con cuidado.** `killport 3000` dice quién tiene el puerto, pregunta y lo cierra: primero un Ctrl+C por su consola y, si no cede en `--timeout` (3 s), en seco. `-y` no pregunta, `--list` solo mira, `--force` va directo, `--tree` suma a los hijos. Todo se refiere a la identidad del proceso (PID + hora de creación): un PID reciclado no se toca nunca. No cierra la terminal desde donde lo corrés, ni a sí mismo, ni un proceso crítico de Windows. Si el Ctrl+C le va a llegar también a quien comparte la consola (npm, un supervisor), lo dice antes de preguntar.

**Nombres que no son identidades.** Un `svchost.exe` que no vive en System32, o que no lanzó `services.exe`, sale marcado como posible impostor: no se oculta ni se protege, se trata como el programa cualquiera que es.

**Lo que hay detrás del dueño.**
- Docker Desktop o Podman: detiene el contenedor por la API del motor, no Docker Desktop.
- WSL (`wslrelay`): cierra el proceso Linux que escucha adentro —SIGINT, SIGTERM y recién después SIGKILL— y nunca arranca una distro.
- System (PID 4): busca qué programa registró la URL en http.sys y cierra ese. El 445 y el 139 son compartir archivos y no se tocan.
- IP Helper: borra la regla de `portproxy`, no el servicio.
- Servicios (SQL Server, PostgreSQL): los detiene como servicio, porque si no Windows los revive; con `--manual` quedan en inicio manual y no vuelven al reiniciar.

**Verifica, no supone.** Relee la tabla hasta que el puerto quede libre y se sostenga así. Si otro proceso lo retoma, nombra al supervisor que lo relanza (nodemon, pm2) y ofrece cerrarlo; si un hijo heredó el socket del que murió, lo encuentra.

**localhost no es 127.0.0.1.** Dos procesos pueden escuchar el mismo puerto en `[::1]` y en `0.0.0.0` sin que ninguno dé error: el clásico "el navegador muestra la versión vieja". El listado los marca, dice a cuál llega cada destino, y `killport localhost:5173` apunta solo a ese.

**Puertos que reserva Windows.** Distingue un puerto ocupado de uno excluido por WinNAT o Hyper-V, y lo confirma con un bind exclusivo. `--reserve` lo reserva para vos, `--unreserve` lo devuelve y `--fix-dynamic` arregla un rango dinámico corrido. Cambian la red de toda la máquina: muestran el plan y preguntan siempre, también con `--yes`.

**Elegir sin nombrar puertos.** `--orphans` (lo que quedó escuchando sin su terminal), `-n node` (por nombre o ruta) y `--project .` (lo que corre en esta carpeta, con su rama). Se combinan: cada uno achica.

**Para scripts.** `--json` estable y con el mismo código de salida que el texto: `0` listo, `1` una parte, `2` línea de comandos inválida, `3` no se pudo, `130` cancelado. `--free 3000` da el primer puerto libre de verdad (sin consola, solo el número: `$port = killport --free 5173`). `--wait 60s` espera a que el puerto escuche mirando la tabla de Windows, sin conectarse a nadie; `--wait-free`, a que se libere; `--any`, `--stable` y `--new` afinan la espera.

**Administrador, solo si hace falta.** Lo que lo pide se ofrece por el UAC. El proceso elevado recibe una selección, no órdenes: vuelve a mirar el sistema y solo toca lo que su propio plan permite.

## Descargar

`killport.exe` es el programa entero: Windows 10 u 11 de 64 bits (se probó en Windows 11), un solo archivo, sin instalador ni dependencias. Copialo a una carpeta del `PATH`. O, con Go:

```powershell
go install github.com/agustinyarrus/killport@v1.0.0
```

Para comprobar la descarga, en la carpeta donde quedaron los dos archivos:

```powershell
(Get-FileHash .\killport.exe -Algorithm SHA256).Hash -eq ((Get-Content .\SHA256SUMS) -split '\s+')[0]   # True
```

En Git Bash o WSL, `sha256sum -c SHA256SUMS`. El `.exe` es reproducible: el mismo commit con el mismo Go da los mismos bytes, así que cualquiera puede compilar la etiqueta y comparar ([cómo](https://github.com/agustinyarrus/killport/blob/v1.0.0/docs/RELEASE.md#4-el-exe-es-reproducible)).

## Cómo se verificó

- Más de 590 tests de Go: el algoritmo contra un simulador de eventos discretos con reloj virtual, propiedades contra implementaciones ingenuas, y pruebas de punta a punta que compilan el `killport.exe` de verdad y lo corren contra la máquina (Docker y WSL con dobles fieles; http.sys, impostores y rangos excluidos, de verdad).
- Cada capa contrastada con una fuente independiente: sockets 42 de 42 y 109 de 109 contra netstat, procesos 417 de 417 contra `Win32_Process`, servicios 147 de 147, Docker 49 de 49 contra su CLI, WSL 31 de 31 contra `ss` y `pgrep` dentro de la distro.
- 290 de 290 mutaciones atrapadas y 270 de 270 verificaciones de punta a punta contra la máquina real.
- CI en `windows-latest` con el Go mínimo del `go.mod`: formato, `go vet` (también para Linux), todas las pruebas, y el `.exe` con su versión y su SHA256.

El detalle, en [docs/VERIFICACION.md](https://github.com/agustinyarrus/killport/blob/v1.0.0/docs/VERIFICACION.md); la arquitectura, en [docs/ARQUITECTURA.md](https://github.com/agustinyarrus/killport/blob/v1.0.0/docs/ARQUITECTURA.md).

## Lo que falta

- **La corrida manual como administrador.** Lo que pide administrador —borrar una regla de `portproxy`, detener un servicio y dejarlo en manual, `--reserve`, `--unreserve`, `--fix-dynamic`, el camino por el UAC— está probado con dobles, con el plan que imprimiría y con el proceso elevado corrido sin elevar contra procesos propios, pero nunca de punta a punta como administrador. El guion está listo: [ADMIN.md](https://github.com/agustinyarrus/killport/blob/v1.0.0/internal/killport/_oracle/ADMIN.md).
- Sin probar contra la cosa real: Podman y los contenedores de Windows (solo con respuestas grabadas), y WSL con `networkingMode=mirrored`, que no tiene `wslrelay`.
- Un nombre de host (`api.local:3000`) no se resuelve: se toma el puerto entero y se avisa.

La lista completa, en [docs/PENDIENTE.md](https://github.com/agustinyarrus/killport/blob/v1.0.0/docs/PENDIENTE.md).
