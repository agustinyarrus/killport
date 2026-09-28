# Arquitectura

killport es un solo programa, pero no un solo paquete: la API cruda de Windows, el algoritmo y la
interfaz viven separados, y lo que decide qué se cierra no sabe nada de Windows. Este documento
recorre las capas de abajo hacia arriba. Cómo se verifica cada una está en
[VERIFICACION.md](VERIFICACION.md); lo que falta, en [PENDIENTE.md](PENDIENTE.md).

## Principios

- **Go puro, sin CGO ni dependencias.** Un único `.exe` estático; las llamadas a Windows van por `syscall`, sin bindings de C. El `go.mod` no tiene ningún `require`: todo es biblioteca estándar o de este repo.
- **Una responsabilidad por paquete.** La API de Windows (`win`) y el algoritmo (`reaper`, `ports`) no se conocen; el pegamento vive en un solo lugar (`internal/killport`).
- **Identidad, no PID.** Todo proceso se refiere por PID + hora de creación. Un PID reciclado nunca se toca.
- **Verificar, no suponer.** Después de actuar, se relee el sistema: un puerto está libre cuando la tabla lo dice y se sostiene así, no cuando el proceso murió.
- **Validar en la frontera.** Los flags se interpretan y validan una sola vez al parsear; adentro los valores ya son del tipo correcto. Lo mismo el trabajo que recibe el proceso elevado: una gramática cerrada que se rechaza entera si no se cumple.
- **Salida visual que degrada.** Colores pastel sobre negro, progreso vivo y tarjeta de resumen en la consola; texto plano sin un solo escape cuando la salida es un pipe o un archivo, y `--json` estable para scripts.

## Paquetes

```
main.go              abre la consola, llama a killport.Main y sale con su código
internal/
  killport/          la herramienta: flags, el adaptador de reaper.OS sobre win, los dominios
                     (Docker, WSL, http.sys, portproxy, servicios), la presentación y el trabajo elevado
  reaper/            el algoritmo, puro: identidad, bosque, plan, desenmascarar, escalada, verificación, espera
  ports/             puro: conjuntos de puertos, direcciones, quién contesta, puerto libre, parsers
  win/               la API cruda de Windows: sockets, procesos, servicios, consola ajena, ejecutor acotado, UAC
    pipeio/          named pipes overlapped con la forma de un net.Conn (la API de Docker)
    pipetest/        servidor de named pipe para las pruebas
    desk/            el modo y el tamaño de la consola propia (lo que necesita tui)
  tui/               consola: paleta, región viva, tarjetas, tablas, formato es-AR
  cli/               flags estilo GNU, ayuda al ancho de la ventana, "¿quisiste decir…?"
  textdist/          distancia de Damerau–Levenshtein
  version/           versión y commit, que build.ps1 estampa con -ldflags
```

Quién importa a quién (nada más):

```
main       → killport, tui, version
killport   → reaper, ports, win, tui, cli
reaper     → ports
win        → win/pipeio
tui        → win/desk
cli        → tui, textdist
ports, textdist, version, win/desk, win/pipeio → (solo la biblioteca estándar)
```

El corazón son cuatro paquetes con una regla de dependencias: `win` (la API cruda de Windows, sin lógica de dominio) y `reaper`/`ports` (el algoritmo, puro) no se importan entre sí. El pegamento vive en `internal/killport`: `sistema.go` implementa la interfaz `reaper.OS` con `win`, y los tests la implementan con un simulador. Lo puro va sin etiqueta de compilación y se prueba en cualquier sistema (un test pasa `go vet` al paquete de la herramienta con `GOOS=linux` para que no se cuele nada de Windows); lo que llama a la API, con `//go:build windows`. Son unas 28.000 líneas de código y 24.000 de tests en esos cuatro paquetes; con el núcleo de consola, el repo suma unas 30.000 y 24.700.

## Núcleo de consola

### tui

- **Región viva**: el progreso se redibuja en el lugar a 20 cuadros por segundo y las líneas permanentes se imprimen por encima. Hay un orden de candados fijo para que no haya deadlock:
  - la función de dibujo corre sin el candado del terminal (puede tomar el del estado de la herramienta);
  - `Println` nunca llama a la función de dibujo: reusa el último cuadro.
- **Recorte seguro**: `ClipANSI` recorta una línea con escapes a N columnas visibles sin romper los colores. Las líneas vivas se cortan una columna antes del borde, para que el terminal no las parta y el conteo de líneas no se desfase.
- **Barra**: resolución de 1/8 de columna (bloques `▏▎▍▌▋▊▉█`) con degradé.
- **Tarjeta**: fondo apenas teñido, sin bordes.
- **Formato es-AR**: miles con punto, decimales con coma, bytes en unidades decimales (las que usan los servicios para sus límites).
- **Barra de tareas**: en Windows Terminal, el avance también se publica en el ícono y la pestaña (OSC 9;4).
- **Tabla**: cada columna tiene un mínimo, las opcionales se sacan en orden si no entran, y las flexibles ceden espacio por "llenado de agua" (la más ancha primero). El recorte por el medio respeta el principio que importa (el programa y su script) o cambia a una forma corta. Sin consola no recorta nada: un script recibe el texto entero. La disposición es una función pura, probada con 20.000 tablas al azar.

### cli

- Flags al estilo GNU: `-o x`, `--out=x`, `-abc`, `--no-x`, `--`.
- Binders tipados con validación: enteros acotados, enumerados, tamaños (`25MB`, `10MiB`) y tiempos (`1:30`, `1m30s`).
- La ayuda se genera con el mismo lenguaje visual que el resto de la salida.
- Un flag mal escrito sugiere el más parecido ("¿quisiste decir --force?") por distancia de Damerau–Levenshtein. Es programación dinámica en O(n·m) con tres filas rodantes, y la transposición de dos letras cuenta como un solo error (`textdist`).

### win/desk

- Lo único de la API de Windows que necesita `tui`: leer y cambiar el modo de la consola propia (para activar las secuencias de escape) y su tamaño. Vive aparte de `win` para que la presentación no dependa de la capa de sistema; un test lo comprueba con `go list -deps`.
- kernel32 se nombra sin ruta porque es una KnownDLL; `win` carga el resto de las DLL del sistema por ruta absoluta de System32. Con el nombre pelado, `LoadLibrary` buscaría primero junto al exe, y un DLL plantado ahí ganaría.
- El envoltorio de las llamadas lleva `//go:uintptrescapes`: sin esa directiva, un búfer que viaja como `uintptr` puede quedar en la pila de la goroutine, y si la pila crece entre la conversión y la llamada, Windows escribe en la pila vieja.

## La capa win

- **Tablas de sockets**: `GetExtendedTcpTable` y `GetExtendedUdpTable` con las clases `OWNER_MODULE`, que además del PID traen la hora de creación del socket y lo necesario para preguntar el módulo dueño; en un svchost ese dueño es el nombre del SERVICIO (lo que agrega `netstat -b`), y averiguarlo no pide abrir el proceso. El layout se verificó contra la máquina: un DWORD con la cantidad, 4 bytes de relleno, filas desde el offset 8 (TCP4 160, TCP6 192, UDP4 160 y UDP6 176 bytes); el puerto es un DWORD con los 2 bytes bajos en orden de red y basura en los altos; la IPv4 está en orden de red tal cual en memoria. La tabla puede crecer entre la llamada que mide y la que llena: se repite mientras dé `ERROR_INSUFFICIENT_BUFFER`. Para `--wait` hay una tabla liviana (solo lo que escucha, sin dueños): 0,34 ms contra 6,95 ms de la completa.
- **Todos los procesos en una llamada**: `NtQuerySystemInformation(SystemProcessInformation)` da PID, padre, hora de creación, sesión y nombre de TODOS, incluidos los protegidos y los de otras sesiones, sin abrir ninguno. La ruta sale de la clase 88 (`SystemProcessIdInformation`): `QueryFullProcessImageNameW` se niega en 50 de 51 servicios, la clase 88 contesta los 52. Los parsers de estos búferes están en `rawbuf.go`: ninguna lectura sin validar antes que la ventana cabe entera, y los punteros que el sistema escribe dentro del búfer se leen como desplazamientos, nunca como punteros de Go. Un búfer corrupto termina en error, no en pánico.
- **El detalle de uno**: la línea de comandos (`ProcessCommandLineInformation`, alcanza con `QUERY_LIMITED_INFORMATION`), si está marcado como crítico, el usuario y la carpeta de trabajo, que Windows no expone: se lee del PEB con tres `ReadProcessMemory` (PEB → `ProcessParameters` → `CurrentDirectory`). En un proceso de 32 bits manda el PEB de 32, el único que sigue un `cd` posterior (medido). El permiso de leer memoria se pide en un segundo handle y solo si el proceso no es de Windows ni crítico.
- **Handles con identidad**: abrir compara `GetProcessTimes` con la hora esperada; si no coincide, el PID se recicló y es un error distinguible (también "ya no existe" y "acceso denegado"). Mientras el handle está abierto el PID no se puede reciclar, así que el motor lo sostiene desde que identifica a la víctima hasta que la ve morir.
- **Espera sin polling**: `WaitForMultipleObjects` sobre los handles más un evento que se señala desde `ctx.Done()`, así que cancelar despierta la espera al instante (menos de 1 ms medido). Con más de 63 procesos se reparten en tandas, cada una en su goroutine, y vuelve la primera salida de cualquiera.
- **El ayudante de Ctrl+C**: `GenerateConsoleCtrlEvent` le llega a toda la consola del que llama, y un proceso solo puede estar enganchado a una consola. Por eso killport se relanza a sí mismo como ayudante con `DETACHED_PROCESS`, que se engancha a la consola del objetivo, lee quiénes la comparten (sin contarse a sí mismo) y manda el Ctrl+C solo si TODOS están en el conjunto permitido, por identidad. La comprobación y el envío van pegados; si hay un intruso, no manda nada y lo devuelve.
- **Servicios**: `EnumServicesStatusExW` da el mapa PID → servicios; detener espera `STOPPED` guiándose por `dwWaitHint` y `dwCheckPoint`; `QueryServiceConfigW` y `QueryServiceConfig2W` dan el tipo de inicio (con el diferido), los disparadores y la política de recuperación, y dejarlo en manual es un `ChangeServiceConfigW` con todo en `SERVICE_NO_CHANGE` salvo el inicio.
- **Sondeo de bind**: `socket` + `bind` sin `listen`, que no dispara el aviso del Firewall. Es exclusivo (`SO_EXCLUSIVEADDRUSE`), porque un bind común a `0.0.0.0` no choca con alguien en `127.0.0.1`, y en IPv6 fija `IPV6_V6ONLY` para que cada familia se sondee por lo suyo. Con exclusivo, `WSAEACCES` es ambiguo (rango excluido, o un socket ajeno con `SO_REUSEADDR`): la tabla de sockets es la fuente primaria y el sondeo solo desempata un puerto sin dueño.
- **Named pipes (pipeio)**: la API de Docker va por `\\.\pipe\docker_engine`. El pipe se abre overlapped (con uno síncrono no hay forma confiable de cortar una lectura trabada) y con `SECURITY_IDENTIFICATION`, para que un servidor que se adelante con el nombre del pipe no pueda suplantar al cliente, que puede ser administrador. No usa `os.NewFile`: `os.File` lleva un offset que lectura y escritura actualizan con candados distintos, una carrera dentro de la biblioteca estándar que dejaba inservible al detector de carreras. Cada sentido tiene su OVERLAPPED y su evento, el deadline es un temporizador que cancela con `CancelIoEx`, y `Close` espera a que terminen las operaciones en vuelo antes de cerrar el handle.
- **El ejecutor acotado** (`RunBounded`): el único camino para `netsh` y `wsl.exe`. El hijo nace suspendido, entra a un Job Object con `KILL_ON_JOB_CLOSE` y `DIE_ON_UNHANDLED_EXCEPTION` y recién ahí corre: nada queda colgado aunque killport se caiga, y ningún diálogo de error aparece. Sin ventana (`CREATE_NO_WINDOW`), con plazo total y tope de salida.
- **Elevación y el archivo seguro**: `ShellExecuteExW` con `runas`, esperando el código de salida (1223 = el usuario dijo que no). El informe del proceso elevado se crea con `CreateNewExact`: archivo nuevo, sin seguir un punto de reanálisis, y con la ruta final verificada por el handle antes de escribir un byte.
- **Lo demás**: el nombre de la interfaz de una IP (`GetAdaptersAddresses`, para la columna ALCANCE), las reglas de portproxy del registro, y rutas NT (`\Device\HarddiskVolume3\…`) pasadas a `C:\…` con la tabla de `QueryDosDeviceW` (gana el dispositivo más largo).

## ports (puro)

- **Conjunto de puertos**: intervalos canónicos (ordenados, disjuntos, sin adyacentes). `Contains` en O(log k) por bisección; unión, intersección y diferencia en O(k + m) por mezcla. La forma canónica es única, así que el texto (`3000, 5000–5003`) es determinista y vuelve a entrar por el parser. La aritmética de los bordes se hace en `int`: en `uint16`, 65535 + 1 es 0.
- **Lo que escribe el usuario**: `3000`, `8080,8081`, `5000-5003`, `:3000`, `localhost:3000`, `[::1]:8080`, `http://localhost:5173/x`. La dirección APUNTA: `ParseRequest` recuerda a qué destino va cada puerto.
- **Quién contesta** (`Answer`): las reglas de Windows medidas con conexiones de verdad. La dirección exacta le gana al comodín; un `0.0.0.0` nativo, a la mitad IPv4 de un socket de doble pila; entre dos `0.0.0.0` con `SO_REUSEADDR` contesta el que se ató primero; `localhost` prueba `::1` y cae a `127.0.0.1`.
- **Parsers que no entienden el idioma**: netsh sale en el idioma de Windows, así que se lee la forma y no las palabras. Los rangos excluidos se anclan en la línea de guiones y toman las filas "entero entero [*]"; las colas de http.sys se arman por sangría (la etiqueta de la cola se repite adentro de cada grupo de URL); el rango dinámico, las reglas de portproxy (con el comando exacto que borra cada una), `ss` y `/proc/net/tcp` de Linux.
- **Puerto libre** (`FindFree`): candidatos = el rango menos lo tomado, los bloques de WinNAT, las exclusiones administradas y el rango dinámico, todo por intervalos; se sondean en orden (bind exclusivo en v4 y v6 por protocolo) hasta juntar los pedidos, y cada tramo salteado queda con su motivo.

## reaper (puro): el algoritmo

- **Identidad**: `ID{PID, Created}`. Se usa como clave de mapa y se compara con `==`, y `time.Time` compara también la zona y la lectura monotónica: dos horas iguales armadas por caminos distintos serían identidades distintas. Por eso toda identidad se canoniza (UTC, sin monotónica) al entrar al paquete.
- **Bosque de procesos**: la arista hijo → padre vale solo si el padre nació antes que el hijo; si no, el PID del padre lo reusa otro y el hijo es raíz. Sobre el bosque: ancestros, subárbol, el ancestro común más cercano (también cruzado, entre el bosque de antes y el de después, que descubre quién relanzó a un proceso) y los herederos de un socket cuyo dueño murió (`Hold`: la trampa de netstat mostrando un PID muerto). Construcción O(n), consultas O(profundidad).
- **Planificación y negativas**: los sockets de los puertos pedidos se agrupan por dueño y cada dueño pasa por una sola puerta (`classify`). Nunca se tocan el kernel, los procesos críticos, killport y su cadena de ancestros (la terminal desde donde corrés), un svchost (se nombra el servicio) ni un intermediario; cada negativa lleva motivo y consejo. Un nombre de Windows (svchost, lsass, csrss…) protege solo si la ruta es exactamente la de System32 y el padre es el esperado: si no, es un posible impostor y sigue el camino común. Un servicio de terceros se detiene como servicio. Con `--tree` las víctimas son la raíz y su subárbol, con un antichain que absorbe un objetivo en su ancestro y sin lo que cuelga de un intocable. `Focus` se queda con lo pedido (el que contesta `localhost`, o lo que eligen `--orphans`, `-n` y `--project`) y deja el resto al margen: ni se toca ni se confunde después con alguien que retomó el puerto.
- **Desenmascarar intermediarios**: a veces la tabla nombra a quien sostiene el puerto en nombre de otro: `com.docker.backend` por un contenedor, `wslrelay` por un proceso Linux, System por la URL que otro registró en http.sys, el IP Helper por una regla de portproxy. El intermediario nunca es víctima. La herramienta pregunta por `Plan.Masked`, mira detrás con el dominio que corresponde y contesta un `Unmasking` por puerto (y por socket: dos contenedores pueden publicar el mismo puerto en IP distintas); `Unmask` reescribe el plan con las MISMAS reglas de `PlanKill`, porque el plan recuerda la instantánea con que se armó. Qué clases se desenmascaran lo decide un solo lugar (`Relay.Unmaskable`): una respuesta no puede volver objetivo al kernel ni a una terminal.
- **Motor de escalada**: todas las víctimas en paralelo, un solo plazo por peldaño y sin polling. 0) abrir handles con identidad; 1) detener los servicios y, a la vez, los objetivos externos (cada dominio con su escalera); 2) cierre prolijo: un Ctrl+C por consola y `WM_CLOSE` a las ventanas; 3) esperar a todos juntos hasta `--timeout`, avisando cada salida; 4) terminar en seco a los que siguen, la raíz antes que su subárbol; 5) un Ctrl+C del usuario corta y deja todo como está. El conjunto permitido del Ctrl+C (la raíz, sus ancestros y su subárbol, sin lo que quedó al margen) sale de una sola función, que usa también el aviso que se muestra antes de preguntar: lo que se promete es lo que pasa. El motor no imprime: emite eventos tipados y devuelve un reporte con el destino de cada víctima y de cada puerto.
- **Verificación**: relee las tablas con backoff (10 → 200 ms, tope 2 s) y, cuando un puerto queda libre, exige que SIGA libre 350 ms (`Settle`): un supervisor tarda unos 240 ms en volver a tomarlo y `wslrelay` suelta el suyo unos 315 ms después del contenedor. Por puerto: libre; sigue un heredero (se ofrece como nueva ronda); lo retomó otro, con el supervisor encontrado por el ancestro común cruzado; lo están relanzando (un proceso nuevo con la imagen de la víctima, colgado de un ancestro vivo, que todavía no escucha: se lo espera hasta 3 s); la dirección pedida la contesta ahora otro que ya escuchaba; o nadie lo tiene y se diagnostica.
- **Diagnóstico**: un puerto sin dueño se sondea PRIMERO. Una exclusión administrada que deja atar es un puerto libre; un bloque de WinNAT es "lo reserva Windows", con remedios tipados que la herramienta convierte en comandos (`net stop winnat && net start winnat`, `--reserve` con el protocolo del bloque). Nunca se aconseja borrar una exclusión ajena.
- **Esperar** (`--wait`, `--wait-free`): `Watch` es la máquina de estados pura (recibe la hora y la tabla; no tiene reloj propio): por puerto, qué sockets cumplen y desde cuándo, con `--stable` (el mismo testigo sostenido, medido desde la hora de creación del socket), `--new` y `--any`. `WaitPorts` relee la tabla liviana con backoff de 25 a 200 ms, y esperando que se libere duerme sobre los handles de los dueños: una salida despierta al instante. Relee porque sin administrador el kernel no avisa los cambios de la tabla (ETW y el filtrado de WFP piden admin).

## internal/killport: el adaptador y los dominios

- **Docker y Podman**: por la API en el named pipe, con un cliente HTTP/1.1 mínimo propio (`http1.go`: `Content-Length`, chunked y hasta EOF, con topes), sin `net/http` ni expresiones regulares: el exe bajó de 8,6 a 4,9 MB. El contenedor que publica un puerto se detiene con `POST /containers/{id}/stop?t=` (`t` = `--timeout`; 0 con `--force`). Un puerto publicado son tres sockets de dos dueños (el motor en `0.0.0.0` y `[::]`, `wslrelay` en `[::1]`) y se muestran como uno.
- **WSL**: `wsl.exe -l --running` (en UTF-16) y un script de `sh` por distro que vuelca `ss` y `/proc/net`; el dueño se confirma por el inodo del socket en los descriptores del proceso, no por el nombre que dice `ss`. El PID 1 (activación de sockets de systemd) es una negativa. Se cierra con SIGINT, SIGTERM y SIGKILL, y se verifica del lado de Windows. Nunca arranca una distro, y `wsl --shutdown` siempre avisa que apaga también Docker Desktop.
- **http.sys, portproxy y servicios**: quién registró la URL (el controlador de la cola, si netsh lo dice); la regla de portproxy que tiene el puerto, que se borra por el proceso elevado y se revalida contra el registro antes y después; el tipo de inicio de cada servicio, si vuelve solo al reiniciar y una tabla chica de sospechosos de siempre (IIS en el 80, VMware en el 443) con su consejo.
- **Proyecto**: la carpeta de trabajo de cada proceso sube hasta `.git`, `package.json`, `go.mod`, `pyproject.toml` o `Cargo.toml`; la rama se lee de `.git/HEAD` (y de un worktree) sin ejecutar git. Es puro, sobre `fs.FS`.
- **La configuración de red** (`--reserve`, `--unreserve`, `--fix-dynamic`) no pasa por el motor: un planificador puro arma los pasos a partir del estado leído, un ejecutor los corre (WinNAT se vuelve a arrancar siempre que se lo detuvo) y un bind real comprueba el resultado.
- **Presentación**: la tabla del listado, los bloques de cada objetivo con la pregunta, la escalada en vivo y la tarjeta final; los renglones se parten sin cortar un comando. El JSON es un documento estable y sale con el mismo código que el texto.

## El trabajo elevado

Lo que pide administrador se hace relanzando killport por el UAC. Un proceso con token de administrador que obedece a sus argumentos es un blanco (cualquier programa del usuario puede lanzarlo, y el diálogo dirá "killport"), así que el trabajo no es una lista de órdenes sino una SELECCIÓN:

- viaja por argumentos, nunca por un archivo que otro programa pueda cambiar entre que se escribe y se lee, con una gramática cerrada (versión, alfabeto, campos en orden, topes, formas canónicas) que rechaza entero lo que no la cumple;
- el hijo toma su propia foto, arma su propio plan con las reglas de siempre y del trabajo usa solo la selección: un proceso con su identidad exacta, un servicio con el mismo proceso dueño, una regla con la misma clase, dirección y destino, un "dejar en manual" solo para lo que él mismo detuvo, y la configuración de red solo si la huella de los pasos coincide con la que confirmó el usuario;
- el informe va solo a una carpeta de trabajo del propio hijo, con un nombre que no sale de ningún argumento, y sirve solo para contar: qué murió, qué regla se borró y qué puertos quedaron libres lo comprueba el padre por su cuenta.
