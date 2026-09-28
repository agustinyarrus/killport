# Oráculo de punta a punta de killport contra un motor de contenedores DE VERDAD.
#
#   pwsh internal\killport\_oracle\docker.ps1 [-Image nginx:alpine] [-Killport ruta\killport.exe]
#
# Necesita Docker Desktop (o Docker Engine / Podman con el CLI `docker`) corriendo y la imagen YA
# descargada: no baja nada. Crea tres contenedores propios (kp-prueba-oraculo-*) que publican
# puertos, los usa de fixtures y los borra en finally pase lo que pase. No toca ningún otro
# contenedor ni el motor. Sin motor o sin imagen se saltea diciendo por qué (y sale 0).
#
# El oráculo independiente es el CLI de docker (docker ps --filter publish, docker inspect) y
# Get-NetTCPConnection: killport habla con el motor por su API en el named pipe, sin el CLI.
# Nada abre una ventana.
[CmdletBinding()]
param(
    [string] $Image = 'nginx:alpine',
    [string] $Killport = ''
)
$ErrorActionPreference = 'Continue'
$here = $PSScriptRoot
$fail = 0
$web = 'kp-prueba-oraculo-web'
$sordo = 'kp-prueba-oraculo-sordo'
$todas = 'kp-prueba-oraculo-todas'

function Paso([string] $t) { Write-Host "`n===== $t" }
function Verifica([bool] $ok, [string] $texto) {
    if ($ok) { Write-Host "  ok    $texto" -ForegroundColor Green }
    else { Write-Host "  FALLA $texto" -ForegroundColor Red; $script:fail++ }
}
function Corre([string[]] $a) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $script:salida = (& $Killport @a 2>&1 | Out-String)
    $script:codigo = $LASTEXITCODE
    Write-Host "> killport $($a -join ' ')   [código $script:codigo · $([int]$sw.Elapsed.TotalMilliseconds) ms]"
    Write-Host $script:salida.TrimEnd()
}
function Escucha([int] $p) { [bool](Get-NetTCPConnection -LocalPort $p -State Listen -ErrorAction SilentlyContinue) }
function Espera([scriptblock] $cond, [int] $segundos = 30) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    while (-not (& $cond) -and $sw.Elapsed.TotalSeconds -lt $segundos) { Start-Sleep -Milliseconds 200 }
    [bool](& $cond)
}
function PuertoLibre {
    $l = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $l.Start(); $p = $l.LocalEndpoint.Port; $l.Stop(); $p
}
# Los procesos del motor, con su hora de inicio: si killport matara y algo los relanzara, cambia.
function Relay { (Get-Process -Name 'wslrelay' -ErrorAction SilentlyContinue | Sort-Object Id | ForEach-Object { "$($_.Id)@$($_.StartTime.Ticks)" }) -join ' ' }
function Motor {
    (Get-Process -Name 'com.docker.backend', 'com.docker.proxy', 'vpnkit', 'gvproxy', 'win-sshproxy', 'dockerd' -ErrorAction SilentlyContinue |
        Sort-Object Id | ForEach-Object { "$($_.ProcessName):$($_.Id)@$($_.StartTime.Ticks)" }) -join ' '
}

# ---------- requisitos ----------
$docker = (Get-Command docker -ErrorAction SilentlyContinue).Source
if (-not $docker) { $docker = 'C:\Program Files\Docker\Docker\resources\bin\docker.exe' }
if (-not (Test-Path $docker)) { Write-Host "SALTEADO: no está el CLI de docker (hace de oráculo)"; exit 0 }
& $docker version --format '{{.Server.Version}}' *> $null
if ($LASTEXITCODE -ne 0) { Write-Host "SALTEADO: el motor de contenedores no contesta (¿está corriendo Docker Desktop?)"; exit 0 }
& $docker image inspect $Image *> $null
if ($LASTEXITCODE -ne 0) { Write-Host "SALTEADO: la imagen $Image no está descargada (el oráculo no baja nada; pasá -Image con una que tengas)"; exit 0 }
if (-not $Killport) {
    $Killport = Join-Path $here 'killport.exe'
    Push-Location (Join-Path $here '..\..\..')
    go build -o $Killport .
    Pop-Location
    if ($LASTEXITCODE -ne 0) { Write-Host "no compila killport" -ForegroundColor Red; exit 1 }
}

$P1 = PuertoLibre; $P2 = PuertoLibre; $P3 = PuertoLibre; $P4 = PuertoLibre
$antes = @(& $docker ps -a --format '{{.Names}}')
$motorAntes = Motor
Paso "estado previo"
Write-Host "contenedores: [$($antes -join ', ')] · puertos de prueba: $P1 $P2 $P3 $P4"
Write-Host "motor: $motorAntes"

try {
    Paso "fixtures"
    & $docker rm -f $web $sordo $todas *> $null
    & $docker run -d --name $web --restart unless-stopped `
        --label com.docker.compose.project=killport --label com.docker.compose.service=web `
        -p "127.0.0.1:${P1}:80" -p "127.0.0.1:${P2}:8080" $Image | Out-Null
    # PID 1 que ignora las señales: docker stop tiene que esperar t y forzar (código 137).
    & $docker run -d --name $sordo --restart always --stop-signal SIGTERM -p "127.0.0.1:${P3}:80" $Image `
        sh -c "trap '' TERM QUIT INT; while :; do sleep 1; done" | Out-Null
    Verifica (Espera { (Escucha $P1) -and (Escucha $P2) -and (Escucha $P3) }) "los tres puertos escuchan en Windows"
    $duenos = @(Get-NetTCPConnection -LocalPort $P1, $P2, $P3 -State Listen | ForEach-Object { (Get-Process -Id $_.OwningProcess).ProcessName } | Sort-Object -Unique)
    Write-Host "dueño según Windows: $($duenos -join ', ')"
    $idWeb = (& $docker ps --no-trunc --filter "publish=$P1" --format '{{.ID}}')
    $idSordo = (& $docker ps --no-trunc --filter "publish=$P3" --format '{{.ID}}')

    Paso "1. describir (--json sin --yes): el objetivo es el contenedor, no el relay"
    Corre @("$P1", '--json')
    $doc = $salida | ConvertFrom-Json
    $o = $doc.objetivos | Select-Object -First 1
    Verifica ($codigo -eq 0 -and $doc.solo_listado) "sale 0 y no actuó"
    Verifica (@($doc.objetivos).Count -eq 1 -and $o.tipo -eq 'contenedor') "un objetivo de tipo contenedor"
    Verifica ($o.contenedor.id -eq $idWeb) "el id coincide con docker ps --filter publish=$P1"
    Verifica ($o.contenedor.nombre -eq $web -and $o.contenedor.imagen -eq $Image) "nombre e imagen"
    Verifica ($o.contenedor.proyecto -eq 'killport' -and $o.contenedor.servicio -eq 'web') "proyecto y servicio de compose"
    Verifica ($o.contenedor.reinicio -eq 'unless-stopped') "política de reinicio"
    Verifica (((@($o.contenedor.puertos.host) | Sort-Object) -join ',') -eq ((@($P1, $P2) | Sort-Object) -join ',')) "TODOS los puertos que publica ($P1 y $P2)"
    Verifica ($duenos -contains ($o.via.proceso -replace '\.exe$', '')) "via = el proceso que Windows ve como dueño ($($o.via.proceso))"
    Verifica (@($doc.negativas).Count -eq 0) "sin negativas"

    Paso "2. el bloque en texto"
    Corre @("$P1", '--list')
    Verifica ($salida -match "contenedor $web" -and $salida -match 'compose killport/web') "nombra el contenedor y su compose"
    Verifica ($salida -match "se detiene entero: libera también $P2") "avisa que detenerlo libera también el $P2"
    Verifica ($salida -match 'reinicio: unless-stopped') "muestra la política de reinicio"
    Verifica ($salida -match 'no se toca') "dice que el motor no se toca"
    Verifica ($salida -notmatch "`e\[") "sin consola no hay escapes de terminal"

    Paso "3. el listado general nombra los contenedores"
    $filas = @((& $Killport --json | ConvertFrom-Json).escuchando | Where-Object { $_.puerto -in $P1, $P2, $P3 })
    Verifica ($filas.Count -eq 3) "tres filas (una por puerto publicado)"
    Verifica (@($filas | Where-Object { $_.intermediario -eq 'contenedores' -and $_.contenedor.nombre -and $_.via }).Count -eq 3) "cada una con intermediario, via y contenedor"
    Verifica (@($filas | Where-Object { $_.puerto -eq $P3 })[0].contenedor.id -eq $idSordo) "el $P3 es del otro contenedor"

    Paso "4. C1: con --no-docker no se pregunta al motor y el relay NO se toca"
    Corre @("$P1", '-y', '--no-docker')
    Verifica ($codigo -eq 3) "sale 3 (no se liberó nada)"
    Verifica ($salida -match 'no se toca') "lo explica"
    Verifica ((Motor) -eq $motorAntes) "el motor sigue siendo el mismo (mismos PID y horas de inicio)"
    Verifica (Escucha $P1) "el $P1 sigue escuchando"

    Paso "5. detener el contenedor (prolijo)"
    Corre @("$P1", '-y')
    Verifica ($codigo -eq 0) "sale 0"
    Verifica ($salida -match 'se detuvo') "informa que se detuvo"
    Verifica ((& $docker inspect -f '{{.State.Running}}' $web) -eq 'false') "docker inspect: ya no corre"
    Verifica (Espera { -not (Escucha $P1) -and -not (Escucha $P2) } 5) "quedaron libres el $P1 y el $P2 (eran del mismo contenedor)"
    Verifica (Escucha $P3) "el $P3, de otro contenedor, sigue"

    Paso "6. un contenedor sordo: el motor lo fuerza al vencer --timeout"
    Corre @("$P3", '-y', '--timeout', '2s', '--json')
    $doc = $salida | ConvertFrom-Json
    $x = $doc.externos | Select-Object -First 1
    Verifica ($codigo -eq 0 -and $doc.libres -eq 1) "sale 0 con el puerto libre"
    Verifica ($x.tipo -eq 'contenedor' -and $x.clave -eq $idSordo -and $x.destino -eq 'terminado') "el externo figura terminado en seco"
    Verifica ($x.ms -ge 1800) "esperó el plazo antes de forzar ($($x.ms) ms)"
    Verifica ((& $docker inspect -f '{{.State.ExitCode}}' $sordo) -eq '137') "docker inspect: código 137 (SIGKILL)"
    Verifica (@($doc.objetivos)[0].cautela -match 'restart=always') "avisó que tiene restart=always"

    Paso "7. --force: sin cierre prolijo (t=0)"
    & $docker start $web | Out-Null
    Verifica (Espera { Escucha $P1 }) "el contenedor volvió a publicar el $P1"
    Corre @("$P1", '-y', '--force')
    Verifica ($codigo -eq 0) "sale 0"
    Verifica ($salida -match 'en seco' -and $salida -notmatch 'no cedió') "dice que fue en seco, y no que 'no cedió'"
    Verifica ((& $docker inspect -f '{{.State.ExitCode}}' $web) -eq '137') "docker inspect: código 137"

    Paso "8. publicado en TODAS las interfaces: varios sockets, dos dueños, UN contenedor"
    # Docker Desktop con WSL 2 publica con com.docker.backend (0.0.0.0 y [::]) y además con wslrelay ([::1]).
    & $docker run -d --name $todas -p "${P4}:80" $Image | Out-Null
    Verifica (Espera { Escucha $P4 }) "el $P4 escucha"
    Start-Sleep -Milliseconds 500
    $socks = @(Get-NetTCPConnection -LocalPort $P4 -State Listen | ForEach-Object { "[$($_.LocalAddress)] $((Get-Process -Id $_.OwningProcess).ProcessName)" } | Sort-Object)
    Write-Host "sockets del ${P4}: $($socks -join ' · ')"
    $dosDuenos = @($socks | ForEach-Object { ($_ -split ' ')[1] } | Sort-Object -Unique).Count -ge 2
    if (-not $dosDuenos) { Write-Host "  (en esta máquina el motor publica con un solo proceso: el caso de los dos dueños no aplica)" }
    $relayAntes = Relay
    $filas = @((& $Killport --json | ConvertFrom-Json).escuchando | Where-Object { $_.puerto -eq $P4 })
    Verifica ($filas.Count -eq 1 -and $filas[0].contenedor.nombre -eq $todas) "el listado tiene UNA fila para el $P4, con su contenedor"
    $filas = @((& $Killport --wsl --json | ConvertFrom-Json).escuchando | Where-Object { $_.puerto -eq $P4 })
    Verifica ($filas.Count -eq 1 -and $filas[0].contenedor.nombre -eq $todas -and -not $filas[0].wsl) "con --wsl también (el contenedor, no un relay colgado)"
    Corre @("$P4", '--json')
    $doc = $salida | ConvertFrom-Json
    Verifica (@($doc.objetivos).Count -eq 1 -and @($doc.negativas).Count -eq 0 -and $doc.objetivos[0].contenedor.nombre -eq $todas) "un solo objetivo (el contenedor), sin negativas"
    Verifica ($doc.objetivos[0].via.proceso -ne 'wslrelay.exe') "via es el motor, no el reenvío de WSL ($($doc.objetivos[0].via.proceso))"
    Corre @("$P4", '--list', '--no-docker')
    Verifica ($salida -notmatch 'wsl --shutdown' -and $salida -notmatch 'relay colgado') "con --no-docker no aconseja apagar WSL (apagaría la VM de Docker) ni habla de un relay colgado"
    Corre @("$P4", '-y')
    Verifica ($codigo -eq 0 -and $salida -match 'se detuvo') "lo detiene y sale 0"
    Verifica ($salida -notmatch 'retomó' -and $salida -notmatch 'sigue ocupado') "esperó a que TODOS los intermediarios suelten el puerto"
    Verifica (-not (Escucha $P4)) "el $P4 quedó libre en todas las direcciones"
    Verifica ((Relay) -eq $relayAntes) "el reenvío de WSL no se tocó (mismo PID, misma hora de inicio)"

    Paso "9. el motor no se tocó en ningún momento"
    Verifica ((Motor) -eq $motorAntes) "mismos procesos, mismas horas de inicio"
}
catch {
    Write-Host "  ERROR del oráculo: $_" -ForegroundColor Red
    $fail++
}
finally {
    Paso "limpieza"
    & $docker rm -f $web $sordo $todas *> $null
    $despues = @(& $docker ps -a --format '{{.Names}}')
    Verifica (($despues -join ',') -eq ($antes -join ',')) "los contenedores quedaron como estaban: [$($despues -join ', ')]"
    Verifica (-not ((Escucha $P1) -or (Escucha $P2) -or (Escucha $P3) -or (Escucha $P4))) "ningún puerto de prueba quedó ocupado"
}

if ($fail -eq 0) { Write-Host "`nDOCKER: TODO OK" -ForegroundColor Green; exit 0 }
Write-Host "`nDOCKER: $fail fallos" -ForegroundColor Red; exit 1
