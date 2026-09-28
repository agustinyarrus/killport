# demo.tape: la demo del README de killport, grabada de verdad con VHS.
#
#   cd demo
#   vhs demo.tape            # escribe demo.gif y demo.webm
#
# Hace falta VHS 0.12.1 o más nuevo (la 0.12.0 no llega a llamar a ffmpeg),
# ttyd, ffmpeg, Go, git, node y python. Todo lo que se ve corre de verdad:
# preparar.ps1 compila killport desde este repo, arma la tienda de la demo con
# tres servidores que escuchan y la monta como X:\dev\tienda. Al final se
# desarma sola. Detalles y plan B (sin VHS): LEEME.md.

Output demo.gif
Output demo.webm

Require pwsh
Require go
Require git
Require node

Set Shell "pwsh"
Set FontFamily "Cascadia Mono"
Set FontSize 16
Set Width 1078
Set Height 644
Set Padding 24
# 1078x644 con Cascadia Mono 16 y 24 de margen: 100 columnas x 30 filas (medido).
Set Framerate 25
Set CursorBlink false
Set TypingSpeed 80ms
Set WaitTimeout 90s
# La paleta de killport (internal/tui/color.go) sobre #0b0b0f.
Set Theme { "name": "killport", "background": "#0b0b0f", "foreground": "#cdd6f4", "cursor": "#c4b5fd", "selection": "#2c2e38", "black": "#2c2e38", "red": "#f2a7b8", "green": "#b5dfa8", "yellow": "#eedfb8", "blue": "#c4b5fd", "magenta": "#f3b9d2", "cyan": "#8fd6cc", "white": "#cdd6f4", "brightBlack": "#55586b", "brightRed": "#f2a7b8", "brightGreen": "#b5dfa8", "brightYellow": "#eedfb8", "brightBlue": "#c4b5fd", "brightMagenta": "#f3b9d2", "brightCyan": "#8fd6cc", "brightWhite": "#ffffff" }

# Fuera de cámara: la escena, un prompt ">" limpio, sin sugerencias del
# historial (serían del que graba) y los colores de la línea de comandos en
# la misma paleta.
Hide
Type@5ms "$demo = $PWD.Path; $tienda = ./preparar.ps1; $env:Path = (Join-Path $demo '.escena\bin') + ';' + $env:Path; Set-Location $tienda"
Enter
Wait
Type@5ms 'Set-PSReadLineOption -PredictionSource None -Colors @{ Command = "`e[38;2;196;181;253m"; Parameter = "`e[38;2;138;143;168m"; Number = "`e[38;2;246;192;160m"; Operator = "`e[38;2;138;143;168m"; Default = "`e[38;2;205;214;244m" }; function prompt { "`e[38;2;196;181;253m>`e[0m " }; Clear-Host'
Enter
Wait
Show

Sleep 300ms

# 1. Lo que escucha este proyecto: su carpeta y su rama.
Type "killport"
Type@90ms " --project ."
Type@75ms " --list"
Sleep 100ms
Enter
Sleep 2s

Ctrl+L
Sleep 100ms

# 2. El 3000 está ocupado: quién lo tiene y la pregunta; "y", el cierre
#    prolijo (Ctrl+C) y la verificación de que quedó libre.
Type "killport"
Type@95ms " 3000"
Sleep 100ms
Enter
Sleep 1.5s
Type "y"
Sleep 200ms
Enter
Sleep 2s

Ctrl+L
Sleep 100ms

# 3. Libre de verdad: lo confirma un bind exclusivo.
Type "killport"
Type@85ms " 3000 --free"
Sleep 100ms
Enter
Sleep 1.7s

# Fuera de cámara: se desarma la escena.
Hide
Type@5ms "Set-Location $demo; ./preparar.ps1 -Limpiar"
Enter
Wait
