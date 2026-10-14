<p align="right"><sub><a href="LEEME.en.md">English</a> · <b>Español</b></sub></p>

# La demo de killport

`demo.gif` y `demo.webm` son una grabación de verdad, no un mockup: una terminal de 100×30 corre
killport compilado desde este repo contra tres servidores de prueba que escuchan de verdad. Lo que
se ve es la salida real de esa corrida.

| Escena | Comando | Qué muestra |
|---|---|---|
| 1 | `killport --project . --list` | lo que escucha este proyecto: tres servidores, cada uno con su puerto, dónde escucha, su línea de comandos, su proyecto y rama (`tienda · feat/carrito`) y quién lo lanzó |
| 2 | `killport 3000` y `y` | quién ocupa el 3000, la pregunta, el Ctrl+C a node, cuánto tardó en cerrar y la verificación (`Puertos libres 1 de 1`) |
| 3 | `killport 3000 --free` | que el 3000 quedó libre de verdad: lo confirma un bind exclusivo |

## Qué hay acá

| Archivo | Para qué |
|---|---|
| `demo.tape` | el guion de [VHS](https://github.com/charmbracelet/vhs): arma la escena fuera de cámara, graba las tres escenas y la desarma |
| `preparar.ps1` | arma la escena (o la desarma con `-Limpiar`); la usan el tape y el plan B |
| `servidores.ps1` | la "terminal" oculta de la tienda: lanza los tres servidores y los espera |
| `tienda/` | el proyecto de juguete: `package.json`, `web.js` (3000), `api.py` (8080), `dev.js` (5173, solo localhost) |
| `grabar.ps1` | plan B sin VHS: la misma sesión en tu terminal, para grabarla con ScreenToGif u OBS |
| `demo.gif` · `demo.webm` | la grabación: 13,6 s a 25 cuadros por segundo, 1078×644, 223 KB y 89 KB |
| `demo.png` | un cuadro fijo del final de la escena 2 (para quien pide menos movimiento) |

## Regrabar con VHS

Hace falta Windows, PowerShell 7, Go, git, node y python, y además:

```powershell
winget install --scope user charmbracelet.vhs tsl0922.ttyd Gyan.FFmpeg
```

**Ojo con la versión de VHS.** La 0.12.0 (la de winget al 28/9/2026) graba los cuadros pero no
genera ni el GIF ni el WebM, sin dar error: cancela el contexto de la grabación y se lo pasa a
ffmpeg. Está arreglado en la 0.12.1:

```powershell
go install github.com/charmbracelet/vhs@v0.12.1     # pide Go 1.26.7+; Go baja solo esa versión
```

VHS dibuja la terminal en un Chrome o Edge sin ventana: usa el que esté instalado (go-rod lo busca
solo), no baja Chromium. Después, desde esta carpeta:

```powershell
vhs demo.tape                                                     # demo.gif y demo.webm
ffmpeg -y -sseof -4.1 -i demo.gif -frames:v 1 demo.png            # el cuadro fijo (la tarjeta de la escena 2)
```

Tarda un minuto. Mirá siempre el resultado antes de publicarlo: cuadros sueltos con
`ffmpeg -ss 3.5 -i demo.gif -frames:v 1 cuadro.png`.

### Qué hace la escena, y por qué

- **Nada de tu máquina en pantalla.** `killport` a secas lista todo lo que escucha la PC (lo tuyo
  también), así que la demo no lo usa: todo pasa en una carpeta de juguete con `--project .` y con
  puertos que son de la demo.
- **La carpeta es `X:\dev\tienda`, no tu ruta.** `preparar.ps1` copia `tienda\` a
  `demo\.escena\dev\tienda` y monta `demo\.escena` como unidad con `subst` (la primera libre de X, Y,
  Z, W, V). killport ve la carpeta de trabajo de cada proceso tal cual se la dieron: `X:\dev\tienda`.
  `-Limpiar` desmonta la unidad y borra `.escena` (está en `.gitignore`).
- **Proyecto y rama de verdad.** La copia es un repo git propio (`git init -b feat/carrito`, un
  commit con autor y fecha fijos): killport lee el nombre de `package.json` y la rama de `.git\HEAD`.
- **La cadena corta.** Los servidores los lanza `servidores.ps1`, una terminal oculta que se suelta
  de quien la llamó (se relanza a sí misma y el primer proceso sale): en el bloque, la cadena es
  `pwsh.exe → node.exe`, sin VHS, ttyd ni tu editor.
- **Cierre prolijo de verdad.** Cada servidor tiene su propia consola oculta y atiende Ctrl+C
  (`SIGINT` en node, `KeyboardInterrupt` en python): killport le manda Ctrl+C y lo ve salir.
- **killport de este repo.** `preparar.ps1` lo compila a `.escena\bin` sin commit estampado: en la
  cabecera dice `v1.0.0`, no `v1.0.0+abc1234`.
- **Ningún puerto ajeno.** Si el 3000, el 8080 o el 5173 ya están ocupados, `preparar.ps1` lo dice y
  sale sin tocar nada (killport te dice quién: `killport 3000 8080 5173 --list`).
- **Limpieza por identidad.** `servidores.ps1` anota PID y hora de inicio de cada proceso;
  `-Limpiar` cierra solo los que coinciden en las dos cosas.
- **El prompt es `>` y nada más**, sin sugerencias del historial (serían las de quien graba), con
  los colores de la línea de comandos en la paleta de killport. El tape los fija fuera de cámara.

### Estilo

Fondo `#0b0b0f`, texto `#cdd6f4` y los acentos de `internal/tui/color.go` como colores ANSI del tema;
Cascadia Mono 16; 1078×644 px, que con 24 px de margen dan 100×30 (medido con
`$Host.UI.RawUI.WindowSize`). Tipeo de 75 a 95 ms por tecla y 1,5 a 2 s para leer cada salida.
25 cuadros por segundo: a 50, el navegador no llega a capturar cada cuadro a tiempo y el video sale
acelerado.

## Plan B, sin VHS

`grabar.ps1` corre la misma sesión en la terminal que tengas abierta, con tipeo humano (80 ms ± 15)
y pausas, y contesta `y` sola (escribe la tecla en el búfer de entrada de la consola, como si la
apretaras; con `-AMano` la contestás vos). Al terminar, o si lo cortás con Ctrl+C, desarma la escena
y deja la carpeta y el PATH como estaban.

```powershell
wt --size 100,30 pwsh -NoProfile -File .\grabar.ps1 -Espera 3
```

Con [ScreenToGif](https://www.screentogif.com) (`winget install NickeManarin.ScreenToGif`): el
grabador sobre esa ventana, iniciar, y `-Espera 3` te da tres segundos antes de la primera escena.
Para que se parezca al GIF: el esquema de colores de Windows Terminal con fondo `#0b0b0f` y la fuente
Cascadia Mono.

## Desarmar a mano

Si una grabación se cortó y quedó algo (la unidad `X:`, los servidores):

```powershell
.\preparar.ps1 -Limpiar
```
