# Oráculo end-to-end del Ctrl+C entre consolas.
#
# Caso 1 (feliz): un servidor node se lanza en una CONSOLA NUEVA
# (CREATE_NEW_CONSOLE) con un handler de SIGINT que escribe un marcador al
# cerrar prolijo. El ayudante, lanzado DETACHED (sin consola propia), se
# engancha a esa consola y manda Ctrl+C. El marcador tiene que aparecer.
#
# Caso 2: proceso SIN consola (ping oculto) → el ayudante devuelve SIN_CONSOLA.
#
# Caso 3: consola compartida con un intruso ajeno → el ayudante NO manda nada y
# devuelve la lista de intrusos.
$ErrorActionPreference = 'Stop'
$here = $PSScriptRoot
$helper = Join-Path $here 'ctrlc-helper.exe'
$node = (Get-Command node).Source
$server = Join-Path $here 'sigint-server.js'
$fail = 0

function Run-Helper($targetPid, $allowed, $tag) {
    $out = Join-Path $here "helper_$tag.txt"
    Remove-Item $out -ErrorAction SilentlyContinue
    $p = Start-Process $helper -ArgumentList $targetPid, $allowed -PassThru -WindowStyle Hidden `
         -RedirectStandardOutput $out
    $p.WaitForExit()
    (Get-Content $out -Raw).Trim()
}

# ---------- Caso 1: cierre prolijo por Ctrl+C ----------
$marker = Join-Path $here 'marker_ok.txt'
Remove-Item $marker, "$marker.ready" -ErrorAction SilentlyContinue
# CREATE_NEW_CONSOLE: node arranca en su propia consola, aislado de la nuestra.
$srv = Start-Process $node -ArgumentList $server, $marker, '0' -PassThru
$srvPid = $srv.Id
# esperar a que escuche
for ($i=0; $i -lt 50 -and -not (Test-Path "$marker.ready"); $i++) { Start-Sleep -Milliseconds 100 }
if (-not (Test-Path "$marker.ready")) { Write-Host "CASO1: el server no arrancó" -ForegroundColor Red; $fail++ }

# los miembros de esa consola nueva: node (y quizá su conhost). Permitimos al node.
$res = Run-Helper $srvPid $srvPid "ok"
Write-Host "CASO1 helper -> $res"
# darle un instante al node para atender el SIGINT y escribir el marcador
for ($i=0; $i -lt 30 -and -not (Test-Path $marker); $i++) { Start-Sleep -Milliseconds 100 }
if (Test-Path $marker) {
    Write-Host "CASO1: MARCADOR PRESENTE -> $((Get-Content $marker -Raw).Trim())" -ForegroundColor Green
} else {
    Write-Host "CASO1: NO apareció el marcador (el Ctrl+C no provocó cierre prolijo)" -ForegroundColor Red
    $fail++
}
if (-not $srv.HasExited) { Stop-Process -Id $srvPid -Force -ErrorAction SilentlyContinue }

# ---------- Caso 2: proceso sin consola ----------
# notepad es una app GRÁFICA: no tiene consola (ping -Hidden sí tiene una
# consola oculta, no serviría). AttachConsole(notepadPid) tiene que fallar.
# OJO: el notepad del Store en Win11 se reparte en VARIOS procesos; para no
# dejar nada corriendo, se anotan los notepad de antes y se matan los nuevos.
$before = (Get-Process notepad,Notepad -ErrorAction SilentlyContinue).Id
$noc = Start-Process notepad -PassThru
Start-Sleep -Milliseconds 500
$res = Run-Helper $noc.Id $noc.Id "noconsole"
Write-Host "CASO2 (sin consola) helper -> $res"
if ($res -notmatch 'SIN_CONSOLA') { Write-Host "CASO2: esperaba SIN_CONSOLA" -ForegroundColor Red; $fail++ }
else { Write-Host "CASO2: OK (SIN_CONSOLA)" -ForegroundColor Green }
Get-Process notepad,Notepad -ErrorAction SilentlyContinue |
    Where-Object { $before -notcontains $_.Id } |
    ForEach-Object { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue }

# ---------- Caso 3: consola compartida con un intruso ----------
# Un cmd en consola nueva; dentro arranca node (comparten la consola). Al
# ayudante le permitimos SOLO el node: el cmd (y su conhost) son intrusos.
$marker3 = Join-Path $here 'marker_shared.txt'
Remove-Item $marker3, "$marker3.ready" -ErrorAction SilentlyContinue
# un .bat (corrido por cmd) lanza node en la MISMA consola: el cmd es el intruso
$bat = Join-Path $here 'run_node.bat'
$sharedNode = Start-Process 'cmd.exe' -ArgumentList '/c', $bat, $marker3 -PassThru
for ($i=0; $i -lt 50 -and -not (Test-Path "$marker3.ready"); $i++) { Start-Sleep -Milliseconds 100 }
$nodePid = if (Test-Path "$marker3.ready") { (Get-Content "$marker3.ready").Trim() } else { 0 }
Write-Host "CASO3: cmd pid=$($sharedNode.Id) node pid=$nodePid"
if ($nodePid) {
    $res = Run-Helper $nodePid $nodePid "shared"   # permitimos solo node; el cmd es intruso
    Write-Host "CASO3 (compartida) helper -> $res"
    if ($res -match 'COMPARTIDA') { Write-Host "CASO3: OK (detectó intrusos, no mandó nada)" -ForegroundColor Green }
    else { Write-Host "CASO3: esperaba COMPARTIDA" -ForegroundColor Yellow }
    if (Test-Path $marker3) { Write-Host "CASO3: OJO, el marcador apareció: se mandó Ctrl+C aunque había intrusos" -ForegroundColor Red; $fail++ }
} else { Write-Host "CASO3: no se pudo montar (node no arrancó en el cmd)" -ForegroundColor Yellow }
Stop-Process -Id $sharedNode.Id -Force -ErrorAction SilentlyContinue
Get-Process node -ErrorAction SilentlyContinue | Where-Object { $_.Id -eq $nodePid } | Stop-Process -Force -ErrorAction SilentlyContinue

if ($fail -eq 0) { Write-Host "`nCTRL+C: TODO OK" -ForegroundColor Green; exit 0 }
Write-Host "`nCTRL+C: $fail fallos" -ForegroundColor Red; exit 1
