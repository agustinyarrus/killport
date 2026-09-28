# Pendiente

Lo que falta, en el orden en que conviene hacerlo. Cada punto dice qué hay hoy, qué falta y cómo se verifica.

## 1. La prueba con administrador

killport está terminado y verificado contra la máquina real ([VERIFICACION.md](VERIFICACION.md)), salvo lo que pide administrador: borrar una regla de portproxy, detener un servicio y dejarlo en inicio manual, `--reserve`, `--unreserve`, `--fix-dynamic` y el camino por el UAC. Eso está probado con dobles, con el plan que imprimiría y con el proceso elevado corrido sin elevar contra procesos propios, pero nunca como administrador.

**Falta:** correr [`internal\killport\_oracle\ADMIN.md`](../internal/killport/_oracle/ADMIN.md) una vez:

1. Parte A, en una consola de administrador: `pwsh internal\killport\_oracle\admin.ps1` (con `-ConWinnat` y `-ConDinamico` si se acepta que la red de la máquina se corte un instante; la deja como estaba).
2. Parte B, sin administrador: `admin.ps1 -Preparar` como admin, killport desde una consola común aceptando y rechazando el UAC, y `admin.ps1 -Limpiar`.

La primera corrida prueba también el guion: si un paso falla, mirar primero si falló killport o el guion.

## 2. Mejoras

- Selección múltiple en la pregunta ("¿Cerrar cuáles? 1,3") para `--orphans`, `-n` y los puertos compartidos: hoy se confirma todo junto. En el listado interactivo, elegir la fila de un puerto compartido ya apunta a ese proceso.
- `--project` no elige un contenedor cuyo proyecto de compose vive en esa carpeta (la etiqueta `working_dir`).
- Un nombre de host (`api.local:3000`) no se resuelve: se toma el puerto entero y se avisa.
- `--wait` no espera la muerte de un proceso nombrado: si el servidor muere antes de escuchar, se espera hasta el plazo.
- El supervisor que relanza a un servidor se busca en su padre y su abuelo vivos. Uno más arriba, o de otra imagen que relance después de la ventana de verificación, no se ve; y un padre vivo con la misma imagen que no relanza suma ~1,5 s antes de dar el puerto por libre.
- Lo que corre desde `C:\Windows` se oculta del listado salvo en las carpetas donde escribe cualquier usuario, que son una lista fija (`Temp`, `Tasks`, `tracing`, `spool`…): mirar la ACL real sería más completo.
- WSL en modo `networkingMode=mirrored` no tiene `wslrelay`: el puerto figura en uso sin dueño en Windows y killport dice "libre" o "reservado" según el sondeo.
- Sin las tablas de `/proc/net` (no pasa en una distro de verdad), el dueño de un socket de Linux se toma de `ss` sin confirmarlo por el inodo.
- En el listado general, la nota de un puerto compartido por dos contenedores dice "procesos" (en el bloque de `killport P` ya dice "servidores").
- Sin probar contra la cosa real: Podman y los contenedores de Windows (solo con respuestas grabadas), un relay de WSL colgado de verdad (el bug WSL#10601 no se puede provocar a propósito), y cancelar `--wait` con Ctrl+C desde mintty (Git Bash).
- Menores de la revisión: el callback de `EnumWindows` se crea en cada llamada; `defer t.Close()` convive con `os.Exit` en `main.go`; `--new` toma la hora de arranque después de parsear la línea de comandos; nueve funciones largas (`planificarConfig`, `Unmask`, `Verify`, `correrPuertos`, `Focus`…) que se leerían mejor partidas; el comando de `netsh` del plan de `--reserve` no entra en 100 columnas y va en su propio renglón.

## 3. Distribución

- Releases en GitHub con el `killport.exe` que genera `build.ps1` y su SHA256 (el script lo imprime al compilar).
- Integración continua: `go vet` y `go test` en `windows-latest` con GitHub Actions.
- Opcional: un instalador (Inno Setup) o un manifiesto de winget/Scoop.
