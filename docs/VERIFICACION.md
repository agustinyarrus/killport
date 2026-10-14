<p align="right"><sub><a href="VERIFICACION.en.md">English</a> · <b>Español</b></sub></p>

# Verificación

La regla: cada capa de killport se contrasta con algo **independiente**. Un parser que lee lo que escribió su propio escritor no prueba nada; netstat, `Win32_Process`, el CLI de docker o `ss` dentro de la distro sí. El [README](../README.md#cómo-se-verificó) tiene el resumen; acá están los comandos, los números y lo que se aprendió. La arquitectura que se verifica está en [ARQUITECTURA.md](ARQUITECTURA.md).

## Tests de Go

```powershell
go test ./...
.\build.ps1 -Test     # go vet + go test, y después compila
```

613 tests: `killport` 253, `reaper` 166, `win` 96, `ports` 49, `tui` 25, `cli` 12, `win/pipeio` 9, `textdist` 2 y `win/desk` 1. No piden administrador, no cambian la configuración de la máquina y no necesitan Docker ni WSL (lo que depende de ellos se saltea solo, diciendo por qué). Las pruebas de punta a punta compilan el `killport.exe` de verdad (con la etiqueta `killport_e2e`, que solo permite cambiar qué `wsl.exe` corre) y los servidores de prueba de `internal/killport/testdata/e2e`.

## En la CI

[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) corre en `windows-latest` con el Go mínimo del `go.mod` (1.26.0): `gofmt`, `go mod tidy -diff`, `go vet` (también con `GOOS=linux`: lo puro compila fuera de Windows), todas las pruebas con `-count=1`, que las pruebas no dejen archivos en el árbol y `build.ps1`, con la versión y el commit comprobados en el `.exe` y su SHA256. Las pruebas corren con [`.github/pruebas.ps1`](../.github/pruebas.ps1): la salida de siempre y, al final, la lista de las salteadas con su motivo, también en el resumen de la corrida. Se puede correr igual en cualquier PC (`pwsh .github\pruebas.ps1`).

El runner no es la PC de desarrollo, y las pruebas lo saben:

- **Corre como administrador.** Se saltean las que prueban el camino sin administrador (el hijo elevado que configura la red, detener un servicio sin permiso); ninguna de las demás cambia la máquina.
- **Tiene WSL sin distros.** `wsl -l --running -q` sale 0 y sin texto (lo dice el código de WSL, `WslClient.cpp`): la prueba contra el `wsl.exe` real ve "ninguna corriendo". El resto de WSL se prueba con dobles y con el `dash` de Git for Windows, que el runner también tiene.
- **Tiene un motor de Docker propio** en `\\.\pipe\docker_engine`, sin contenedores. killport lo consulta junto con el motor falso de cada prueba; se comprobó en la PC con un motor vacío propio en ese pipe: todas las pruebas de Docker y de punta a punta en verde, y el motor recibió sus consultas.
- **http.sys:** si no deja registrar la URL de prueba, las pruebas de lo que hay detrás de System se saltean diciendo el código (se comprobó provocando el rechazo: la misma URL registrada dos veces da 183).

Y al revés, lo que le pasa a una PC de desarrollo y no al runner: el camino `/proc` del sondeo lista los fd de todos los procesos, y en MSYS eso se le pregunta a cada proceso de Git Bash, con un segundo de espera por cada uno que no contesta. Con muchas consolas abiertas la lista puede pasar el tope de 30 s; `TestShSondeoRespaldoProc` se saltea entonces diciéndolo (se reprodujo con 36 procesos MSYS suspendidos), y ese camino queda cubierto por las salidas grabadas.

### Umbrales de tiempo

Una prueba que mide tiempo separa lo correcto de lo que quiere descartar, y el umbral va lejos de los dos. Lo que se descarta casi siempre es "esperó el plazo entero" o "no se enteró": la cota es la mitad del plazo, o un par de segundos contra algo que sin el arreglo no terminaría nunca; no unos cientos de milisegundos del caso bueno, que una máquina cargada se come en un par de cuantos del planificador. Lo fino (enterarse a lo sumo 200 ms después, despertar al instante con la salida del dueño) se prueba en `reaper` con reloj falso, donde no depende de la máquina.

Dos detalles más. Los sistemas falsos cuentan su historia desde la primera lectura, no desde que se arman: contada desde el armado, una máquina lenta arrancaba la espera con el socket ya ahí (así se volvió inestable `TestEsperaJSON`; con 300 ms de demora simulada, el helper viejo falla igual que aquella vez y el nuevo pasa). Y un costo se mide por tandas: el reloj monotónico de Go en Windows avanza de a ticks del sistema (entre 0,5 y 15,6 ms), así que una lectura suelta de medio milisegundo mide 0 o un tick.

Se comprobó atando `go test` a 4 núcleos y ocupándolos con el doble de hilos: todo en verde, con los tests de tiempo repetidos hasta 40 veces. Con Go 1.26.0 y las cachés vacías, como en la CI, también.

## Contra la máquina

```powershell
pwsh internal\win\_oracle\all.ps1                   # capa win contra netstat, WMI y un Ctrl+C real
pwsh internal\killport\_oracle\docker.ps1           # contra Docker Desktop de verdad
pwsh internal\killport\_oracle\wsl.ps1 -Distro Ubuntu
```

Un programa que cierra procesos no puede dar nada por cierto porque lo diga la documentación de Windows: los offsets, los códigos de error y los tiempos se midieron en la máquina, y cada capa se contrasta con otra fuente. Todo corre sin administrador y sin cambiar la configuración de la máquina. Los fixtures de los tests y de los guiones de killport son propios, sin ventana, y se cierran por identidad (PID + hora de creación) en un `finally`; los contenedores de prueba se llaman `kp-prueba-*` y se borran al final; `wsl.ps1` arranca la distro para sus fixtures (killport nunca lo hace) y deja que WSL la vuelva a detener. `all.ps1`, el de la capa win, es más viejo y más brusco: su caso de Ctrl+C abre ventanas (un node en una consola nueva, un notepad) y comprueba que detener W32Time sin administrador se niega sin tocarlo; conviene correrlo sin nada más abierto.

Los tests corren limpios también con `go test -race` (hace falta un gcc para CGO). Docker se prueba ahí con un motor falso en un named pipe propio; lo que depende de la máquina (WSL, el `dash` de Git, que haya rangos excluidos, que el 445 escuche) se saltea solo, diciendo por qué, si no está.

## Oráculos

Resultados del 28/09/2026, en la PC de desarrollo (Windows 11, Docker Desktop 4.91 con motor 29.8.0, WSL 2.7 con Ubuntu 26.04):

| Qué | Oráculo independiente | Resultado |
|---|---|---|
| tablas de sockets | `netstat -ano` (la misma API, otro programa) | lo que escucha por TCP, 42 de 42 idénticos; UDP, 109 de 109 |
| procesos (`NtQuerySystemInformation`) | `Win32_Process` | 417 de 417 en PID, padre, sesión y hora de creación; nombre, 416 más el "System Idle Process" |
| ruta de cada proceso | `QueryFullProcessImageNameW` (otra API, otro formato) | 0 diferencias en todos los que las dos pueden leer (226 en la primera medición) |
| servicios | `Win32_Service`, `sc query`, `sc qc` | 147 de 147 en PID y estado; se puede detener o no, tipo de inicio |
| línea de comandos | `Win32_Process.CommandLine` y `CommandLineToArgvW` | idéntica; partida en argumentos igual que Windows en 30.000 líneas al azar |
| carpeta de trabajo (PEB) | la carpeta con que se lanzó el proceso, y un `cd` en un cmd de 32 bits | la misma, con ñ y espacios; en WOW64 sigue el `cd` |
| interfaces | `Get-NetIPAddress` | cada dirección con su interfaz |
| rangos excluidos y sondeo de bind | `netsh … show excludedportrange` más un bind real a cada rango | los bloques de WinNAT dan `WSAEACCES` y la exclusión administrada deja atar |
| de quién es cada exclusión | las tres clases provocadas a la vez, sin administrador: un `HttpListener` en tres puertos, una reserva de un programa (`SIO_ACQUIRE_PORT_RESERVATION`) y los bloques de WinNAT; y SSDP atendiendo el 2869 | http.sys: una fila de un solo puerto por cada puerto que atiende, en IPv4 y en IPv6, que se va con él; el programa: una fila de su tamaño que muere con él; WinNAT: 12 bloques de exactamente 100, sin alinear. Las tablas quedaron grabadas en `internal/ports/testdata` y las pruebas de punta a punta lo repiten con una reserva y una URL de verdad |
| cierre prolijo | el marcador que escribe el servidor de prueba al recibir SIGINT | aparece cuando cerró por Ctrl+C; no aparece con `--force` ni con la consola compartida |
| Docker | el CLI de `docker` (`ps --filter publish`, `inspect`) y la tabla de sockets | 49 de 49 |
| WSL | `ss`, `pgrep` y `wsl -l --running` dentro de la distro | 31 de 31 |
| los scripts que corren dentro de Linux | el `dash` de Git for Windows (el `/bin/sh` de Ubuntu) sobre el `/proc` de MSYS | tests `wsl_sh_windows_test.go` |

`Get-NetTCPConnection` y `Get-NetUDPEndpoint` resultaron oráculos pobres: pierden siempre cuatro sockets que escuchan (445, 7680, 8000 y 50052, de System y de servicios), redondean la hora de creación al segundo y juntan en uno los UDP que varios procesos comparten (mDNS). Quedaron para lo que sí hacen bien: medir cuánto tarda un supervisor en retomar un puerto (241 ms, de ahí la ventana de 350 ms de la verificación).

## El simulador y las propiedades

El núcleo (`reaper`) se prueba contra un simulador de eventos discretos: un reloj virtual que solo avanza hasta el próximo evento, procesos guionados (cuánto tardan en ceder al Ctrl+C, si lo ignoran, si su supervisor los relanza, si heredaron un socket), consolas compartidas, servicios, contenedores y procesos de Linux. Sin hilos ni reloj real: cada escenario comprueba la secuencia de eventos, el destino de cada víctima y de cada puerto, y el tiempo exacto. Un Ctrl+C que cierra a los 420 ms deja el reloj en +420 ms; uno que se ignora se termina exactamente al vencer el plazo; una cancelación a +1 s corta ahí y no termina a nadie en seco.

Encima, propiedades con semilla fija contra implementaciones independientes:

- el conjunto de puertos contra un mapa de 65.536 booleanos, con los bordes 0 y 65.535 (donde `uint16 + 1` da la vuelta);
- el bosque contra una implementación ingenua en 500 bosques al azar con PID reciclados: ancestros, subárbol y el ancestro común de todos los pares;
- el plan, en 400 bosques que siempre traen el kernel, procesos críticos, killport y su terminal: nunca son víctimas, y con `--tree` nadie aparece dos veces; y lo que se desenmascara detrás de un intermediario se clasifica con la misma vara;
- la escalada en 300 escenarios al azar; y el aviso del Ctrl+C de antes de preguntar contra lo que el motor hace después, en 600 familias de procesos al azar;
- el puerto libre contra un oráculo puerto por puerto (400 semillas), y la espera contra otro que no usa la máquina de estados (300 historias);
- el trabajo del proceso elevado: 82 textos que se tienen que rechazar (y 8 más mandados al `killport.exe` de verdad, que sale con `2` sin tocar nada) y 60.000 mutaciones al azar del texto; la ruta del informe, 42 rutas que se tienen que rechazar y 40.000 mutaciones;
- la tabla del listado en 20.000 disposiciones al azar;
- los parsers de http.sys (las colas y las reservas de URL) en 300 salidas cada uno, con las etiquetas en un idioma inventado distinto cada vez: se leen igual, porque no leen palabras;
- el planificador de `--reserve` en 3000 estados al azar con las cuatro clases de exclusión: WinNAT se detiene solo si un bloque suyo tapa lo pedido, y lo que tapa una de http.sys o de un programa es un impedimento.

## Mutaciones

Cada etapa terminó con un script que cambia una línea del código a propósito (invertir una condición, saltear un paso, confundir v4 con v6), corre los tests, y restaura el archivo comprobando el hash. Fueron 290 mutaciones: todas atrapadas. Seis sobrevivieron a su primera corrida y cada una destapó un test flojo que se reforzó; la más interesante, "en WOW64 manda el PEB de 64 bits", vivió hasta que un test hizo `cd` en un cmd de 32 bits después de arrancarlo. Cinco se descartaron con su motivo: equivalentes (en Windows `IPV6_V6ONLY` ya vale 1 de fábrica) o imposibles de provocar sin inyectar fallas en advapi32. Además, cada uno de los 67 arreglos de la revisión y de la regresión final llegó con un test que falla en el árbol de antes.

## Regresión final

Con el `killport.exe` de `dist` de ese día (la 1.1.0 de la suite de la que salió killport), los guiones de punta a punta de cada etapa contra la máquina real. El código de esta 1.0.0 es ese salvo los nombres internos y seis cambios posteriores, cada uno con su prueba: cuatro de presentación (la línea "usuario:" solo cuando el dueño es otra cuenta; el supervisor que se cierra en la segunda ronda contado una sola vez, la última lección; el pie del listado y los errores largos partidos en palabras al ancho de la consola) y dos de diagnóstico (las URL de http.sys atadas a una IP y de quién es cada exclusión de puertos, las dos primeras lecciones). Los de las etapas viven en la carpeta de trabajo, fuera del repo, porque dependen de esta PC (rutas fijas, los procesos del usuario que tienen que seguir vivos); los de Docker y WSL están en el repo.

| Guion | Qué cubre | Resultado |
|---|---|---|
| regresión básica | listado, cierre prolijo con Ctrl+C (el marcador aparece), `--force` (no aparece), el 445 del kernel | 17 de 17 |
| http.sys | un HttpListener propio detrás de System: quién registró la URL, cerrarlo, varios en el mismo puerto | 29 de 29 |
| impostores | un `svchost.exe` y un `lsass.exe` fuera de System32: marcados, no protegidos, no ocultos | 31 de 31 |
| localhost, proyecto, huérfanos | el puerto compartido en `[::1]` y `0.0.0.0` con el oráculo de conexiones de verdad, `--orphans`, `--project`, `-n`, padre e hijo en la misma consola | 45 de 45 |
| libre y espera | `--free` con el oráculo de atar en las cuatro direcciones, `--wait`, `--wait-free`, `--new`, la falla rápida de WinNAT, la pregunta en una consola de verdad (ConPTY) y un caso de cada función | 68 de 68 |
| `_oracle\docker.ps1` | contenedores de verdad: nombra, detiene (SIGTERM y SIGKILL), `restart=always`, `--no-docker`, el `[::1]` de wslrelay | 49 de 49 |
| `_oracle\wsl.ps1 -Distro Ubuntu` | procesos Linux de verdad: nombra, SIGINT, escala a SIGKILL, relay sin nadie detrás, no arranca distros | 31 de 31 |

270 verificaciones, 0 fallas; ningún fixture vivo al terminar, los procesos del usuario intactos, Docker y WSL como estaban. La primera corrida dio 7 fallas en el guion de localhost, todas del guion: esperaba textos y códigos de salida que la revisión cambió a propósito. Mirando esas salidas apareció un defecto de verdad, que ningún chequeo pedía (abajo, en las lecciones: el aviso de antes de preguntar); se arregló con su test y se volvió a correr todo.

## Las salidas del README

Las del [recorrido](../README.md#recorrido) y la [demo](../demo/LEEME.md) son corridas de verdad, no texto armado. El 28/09/2026, con killport 1.0.0 compilado desde este repo:

- **La escena**: servidores propios (node, python, un `HttpListener` de .NET en `pwsh`, un supervisor al estilo nodemon que relanza a su hijo y un servidor que ignora el Ctrl+C) en dos proyectos de juguete con su repo git (`tienda` en la rama `feat/carrito`, `landing` en `main`), montados con `subst` como `X:\dev\…`. Cada servidor corre en su propia consola oculta, lanzado por una terminal oculta que se soltó de su padre: la cadena que muestra killport es `pwsh.exe → node.exe`, sin rastro de quién los lanzó. Todo se cerró al final por PID + hora de inicio.
- **La consola**: cada comando corrió dentro de una pseudoconsola (ConPTY) de 100 columnas, sin ventana, con un arnés que reconstruye la pantalla y contesta las preguntas (`[s/N]`) cuando aparecen. Para killport es una consola de verdad: colores, región viva, preguntas. Los bloques del README son esa pantalla, copiada por un script; lo único que se sacó es la cabecera repetida y, en la tabla del listado general, las filas de otros programas de la máquina (marcadas `⋮`). El pie de esa tabla se capturó cuando killport todavía no lo partía (la consola lo cortaba en `--` / `all-states`); después del arreglo se reemplazaron esas dos líneas por las que killport escribe hoy para los mismos números, que `TestPieDelListadoSeParte` fija a 100 columnas, y se comprobó el pie partido con una corrida de verdad en la misma pseudoconsola.
- **WSL**: un `python3 -m http.server` dentro de Ubuntu (WSL 2), cerrado por killport con SIGINT; la distro volvió a detenerse sola.
- **Docker**: un `nginx:alpine` llamado `kp-prueba-web`, con las etiquetas de compose de la tienda (`com.docker.compose.project=tienda`, `…service=web`), `restart=unless-stopped` y dos puertos publicados (8088 y 8443), en Docker Desktop 4.91 (motor 29.8.0) arrancado para la captura con `docker desktop start`. killport lo detuvo por la API del motor después de contestar `y`; el contenedor se borró y Docker Desktop se volvió a detener (`docker desktop stop`), como estaba.
- **Lo que no se capturó**: un impostor (la ruta real de la imagen sale entera, aunque la carpeta esté montada con `subst`, y habría mostrado una carpeta de la máquina).

Leer esas salidas enteras, como en la regresión final, encontró un defecto más (la última lección).

## Lecciones

Los oráculos y las pruebas contra la máquina encontraron defectos que los tests internos no veían:

- **No toda exclusión es de WinNAT.** El verificador de la separación vio el 2869 excluido y escuchado por System a la vez, y minutos después sin ninguna de las dos cosas. Midiendo: http.sys aparta con una fila de un puerto (en IPv4 y en IPv6, aunque escuche en `127.0.0.1`) cada puerto que atiende, mientras lo atiende; y cualquier programa, sin administrador, puede reservar puertos (`SIO_ACQUIRE_PORT_RESERVATION`) con una fila que tampoco lleva `*`. killport tomaba todo por WinNAT y aconsejaba `--reserve`, que detiene WinNAT sin liberar nada de eso. Ahora la forma de la fila y lo que se ve de http.sys dicen de quién es (`TestE2EReservaDeUnPrograma` falla con el diagnóstico de antes).
- **Una URL atada a una IP tiene un tercer tramo.** `HTTP://127.0.0.1:8080:127.0.0.1/`: el parser de las colas no lo leía, y el 8080 de un `HttpListener` en `127.0.0.1` quedaba sin dueño a la vista (`TestE2EHTTPSysAtadaAUnaIP` falla con el parser de antes).

- **Servicios: el PID está en 0x2C.** En `ENUM_SERVICE_STATUS_PROCESSW`, `dwProcessId` es el octavo DWORD de la estructura embebida: 0x2C, no 0x28, donde está `dwWaitHint` (0 en un servicio que corre). Con 0x28 todos los PID daban 0. El test de bytes pasaba (se armaba con el mismo offset equivocado); lo atrapó `Win32_Service`.
- **`SOCKET_ERROR` llega sin signo.** `bind` devuelve -1 al fallar, pero en el registro de 64 bits llega como 0xFFFFFFFF, sin extender el signo: `int(r) == -1` nunca era cierto y toda falla se leía como éxito (el 135 y el 445 daban "libre"). Se compara con `r != 0`. Vale para cualquier llamada que devuelve un entero de 32 bits negativo.
- **`//go:uintptrescapes`.** Un búfer pasado como `uintptr` puede vivir en la pila de la goroutine; si la pila crece entre la conversión y la llamada, Windows escribe en la pila vieja y el resultado vuelve vacío. Se reprodujo siempre con 64 goroutines nuevas llamando a `GetFinalPathNameByHandleW`: falla sin la directiva, pasa con ella.
- **`time.Time` como clave.** La identidad de un proceso es PID + hora, usada como clave de mapa y comparada con `==`. `time.Time` compara también la zona y la lectura monotónica: la hora que venía de un handle (FILETIME → hora local) no coincidía con la del bosque (UTC), la espera nunca reencontraba a la víctima y el motor giraba en falso hasta el plazo. El simulador no lo podía ver (ahí todo es UTC). Ahora toda identidad se canoniza al entrar.
- **La carrera del offset de `os.File` en un pipe.** `go test -race` marcaba 18 carreras, todas dentro de la biblioteca estándar: `os.File` actualiza un offset por archivo con candados distintos para leer y escribir, y un cliente HTTP lee y escribe a la vez. Inofensiva (el kernel ignora el offset en un pipe) pero tapaba cualquier carrera de verdad; de ahí `win/pipeio`.
- **La ruta de un servicio no se abre, se pregunta.** `QueryFullProcessImageNameW` se niega en 50 de 51 servicios aun sin pedir nada más; `NtQuerySystemInformation` con la clase 88 da la ruta de todos sin abrir ninguno.
- **El ayudante se ve a sí mismo.** Al engancharse a la consola del objetivo, el ayudante del Ctrl+C aparece en la lista de miembros: sin excluirlo, siempre se detectaba como intruso. Y "no hay consola que manejar" son tres códigos (5, 6 y 87), no uno.
- **netsh habla el idioma de Windows** y escribe UTF-8 a un pipe aunque la consola sea 850. Los parsers leen la forma, no las palabras; y un guion de PowerShell que compara la salida tiene que fijar `[Console]::OutputEncoding` (en una consola nueva decodifica con la 850 y todo acento daba falla con killport andando bien).
- **Un puerto publicado por Docker son tres sockets de dos dueños** (`com.docker.backend` en `0.0.0.0` y `[::]`, `wslrelay` en `[::1]`), y `wslrelay` suelta el suyo 315 ms después del contenedor: la verificación espera a los dos, y cuando aconseja `wsl --shutdown` avisa que apaga también el motor de contenedores si corre en WSL (o si no se pudo saber).
- **Chrome y el rango dinámico.** Los fixtures atan TCP en un puerto que da el sistema, y Chrome tiene decenas de sockets UDP efímeros en el mismo rango: sin `--tcp`, un `killport P -y` de las pruebas podía cerrar el Chrome del usuario. Toda prueba que cierra por número de puerto lleva `--tcp` y elige un puerto que no está en ninguna tabla.
- **El Ctrl+C desde Git Bash llega ignorado**: un proceso lanzado desde bash hereda el Ctrl+C desactivado, killport lo manda, no llega, y a los 3 s lo termina en seco. No es un error de killport: los guiones de Ctrl+C se corren desde PowerShell, y la prueba de Go que manda un Ctrl+C de verdad (`TestE2EEsperaCancelada`) lo reactiva antes de lanzar a quien lo recibe.
- **El aviso de antes de preguntar tiene que salir de la misma regla que la acción.** Con un padre en `0.0.0.0` y su hijo en `[::1]` en la misma consola, `killport localhost:P` avisaba "el Ctrl+C le llega también a su padre"; el motor no lo mandaba (el padre quedó al margen del pedido) y cerraba al hijo en seco sin haberlo dicho. Ahora el aviso y el motor usan la misma función, y una propiedad los compara en 600 familias al azar.
- **Dos rondas, un solo destino por proceso.** Con un supervisor que comparte la consola del servidor, el Ctrl+C de la primera ronda le llega sin ser víctima; si después se acepta cerrarlo, en la segunda ronda es víctima. Al fusionar los dos reportes, la tarjeta lo contaba dos veces ("En seco 1" y "También cerraron 1", con el renglón "le llegó el Ctrl+C", cuando en realidad había terminado en seco). Se vio capturando la salida del README; ahora un alcanzado de la primera ronda que en la segunda es víctima se cuenta por su resultado (`fusionarAlcanzados`, con `TestSupervisorCerradoNoSeCuentaComoAlcanzado`, que falla con la fusión de antes).

## Pendiente manual: la prueba con administrador

Lo que pide administrador —borrar una regla de portproxy, detener un servicio y dejarlo en inicio manual, `--reserve`, `--unreserve`, `--fix-dynamic`, el camino por el UAC— está probado con dobles, con el plan que imprimiría y con el proceso elevado corrido sin elevar contra procesos propios, pero **nunca corrió como administrador**. El procedimiento y el guion están listos:

- [`internal\killport\_oracle\ADMIN.md`](../internal/killport/_oracle/ADMIN.md): parte A (guiada, en una consola de administrador) y parte B (sin administrador, por el UAC), con lo que tiene que pasar en cada paso y qué anotar.
- `pwsh internal\killport\_oracle\admin.ps1` (parte A; `-Preparar` y `-Limpiar` para la B; `-ConWinnat` y `-ConDinamico`, opcionales, tocan la red de toda la máquina y la dejan como estaba). Sin administrador se saltea solo (código 0).

La primera corrida es también la prueba del guion: si un paso falla, mirar primero si falló killport o el guion.
