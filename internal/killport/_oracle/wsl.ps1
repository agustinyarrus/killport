# Oráculo de punta a punta de killport contra WSL DE VERDAD (una distro real, el wslrelay real).
#
#   pwsh internal\tools\killport\_oracle\wsl.ps1 -Distro Ubuntu [-Killport ruta\killport.exe]
#
# OJO: este script ARRANCA la distro que se le pasa (killport jamás lo hace; los fixtures sí
# necesitan correr adentro). Por eso -Distro es obligatorio: sin él se saltea. Los fixtures son
# procesos python3 propios dentro de la distro, lanzados sin ventana, y se matan en finally pase lo
# que pase. Si la distro estaba detenida, al terminar se espera a que WSL la vuelva a detener sola
# (lo hace a los pocos segundos de quedar sin sesiones).
#
# Los oráculos independientes son `pgrep`/`ss` dentro de la distro, `wsl -l --running` y
# Get-NetTCPConnection del lado de Windows.
[CmdletBinding()]
param(
    [string] $Distro = '',
    [string] $Killport = ''
)
$ErrorActionPreference = 'Continue'
$env:WSL_UTF8 = '1'
$here = $PSScriptRoot
$fail = 0
$wsl = "$env:SystemRoot\System32\wsl.exe"

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
function Espera([scriptblock] $cond, [int] $segundos = 40) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    while (-not (& $cond) -and $sw.Elapsed.TotalSeconds -lt $segundos) { Start-Sleep -Milliseconds 250 }
    [bool](& $cond)
}
function PuertoLibre {
    $l = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    $l.Start(); $p = $l.LocalEndpoint.Port; $l.Stop(); $p
}
function Corriendo { @((& $wsl -l --running -q) -split "`r?`n" | ForEach-Object { $_.Trim() } | Where-Object { $_ }) }
function EnLinux([string] $cmd) { (& $wsl -d $Distro -u root -e sh -c $cmd 2>&1 | Out-String).Trim() }
function Relays {
    (Get-Process -Name 'wslrelay', 'wslhost', 'wslservice', 'vmmemWSL' -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -eq 'wslrelay' } |
        Sort-Object Id | ForEach-Object { "$($_.Id)@$($_.StartTime.Ticks)" }) -join ' '
}
# Lanza un fixture dentro de la distro, sin ventana. (Start-Process une la lista de argumentos con
# espacios y SIN comillas: el script de sh va como un solo argumento citado.)
function Fixture([string] $sh) {
    Start-Process -FilePath $wsl -ArgumentList "-d $Distro -u root -e sh -c `"$sh`"" -WindowStyle Hidden -PassThru
}
function PidLinux([int] $p) { EnLinux "pgrep -f '^python3 -m http.server $p'" }

# ---------- requisitos ----------
if (-not (Test-Path $wsl)) { Write-Host "SALTEADO: no hay wsl.exe"; exit 0 }
if (-not $Distro) { Write-Host "SALTEADO: pasá -Distro NOMBRE (el oráculo ARRANCA esa distro para correr sus fixtures)"; exit 0 }
$instaladas = @((& $wsl -l -q) -split "`r?`n" | ForEach-Object { $_.Trim() } | Where-Object { $_ })
if ($instaladas -notcontains $Distro) { Write-Host "SALTEADO: la distro $Distro no está instalada (hay: $($instaladas -join ', '))"; exit 0 }
if (-not $Killport) {
    $Killport = Join-Path $here 'killport.exe'
    Push-Location (Join-Path $here '..\..\..\..')
    go build -o $Killport ./cmd/killport
    Pop-Location
    if ($LASTEXITCODE -ne 0) { Write-Host "no compila killport" -ForegroundColor Red; exit 1 }
}

$P1 = PuertoLibre; $P2 = PuertoLibre; $P3 = PuertoLibre
$antes = Corriendo
$estabaCorriendo = $antes -contains $Distro
Paso "estado previo"
Write-Host "distros corriendo: [$($antes -join ', ')] · puertos de prueba: $P1 $P2 $P3"

$fixtures = @()
try {
    if (-not $estabaCorriendo) {
        Paso "0. killport NUNCA arranca una distro detenida"
        Corre @('--wsl', '--json')
        $doc = $salida | ConvertFrom-Json
        Verifica ($doc.wsl.distros -notcontains $Distro) "--wsl no la cuenta entre las que corren"
        Corre @("$P1", '--list')
        Verifica ((Corriendo) -notcontains $Distro) "después de --wsl y de mirar un puerto, $Distro sigue detenida"
    }

    Paso "fixtures (arrancan la distro: lo hace este script, no killport)"
    if ((EnLinux 'command -v python3 >/dev/null && echo SI') -ne 'SI') { Write-Host "SALTEADO: la distro no tiene python3 para los fixtures"; exit 0 }
    $fixtures += Fixture "exec python3 -m http.server $P1 --bind 0.0.0.0"
    # Sordo a SIGINT y SIGTERM (lo ignorado se hereda a través de exec): hay que llegar a SIGKILL.
    $fixtures += Fixture "trap '' INT TERM; exec python3 -m http.server $P2 --bind 0.0.0.0"
    $fixtures += Fixture "exec python3 -m http.server $P3 --bind 127.0.0.1"
    Verifica (Espera { (Escucha $P1) -and (Escucha $P2) -and (Escucha $P3) }) "los tres puertos escuchan en Windows (los reenvía WSL)"
    $duenos = @(Get-NetTCPConnection -LocalPort $P1, $P2, $P3 -State Listen | ForEach-Object { (Get-Process -Id $_.OwningProcess).ProcessName } | Sort-Object -Unique)
    Write-Host "dueño según Windows: $($duenos -join ', ')"
    Verifica (($duenos | Where-Object { $_ -notin 'wslrelay', 'wslhost' }).Count -eq 0) "el dueño en Windows es el reenvío de WSL"
    $relaysAntes = Relays
    $pid1 = PidLinux $P1; $pid2 = PidLinux $P2; $pid3 = PidLinux $P3
    Write-Host "PID Linux (pgrep): $P1 -> $pid1 · $P2 -> $pid2 · $P3 -> $pid3"

    Paso "1. describir (--json sin --yes): el objetivo es el proceso Linux, no el relay"
    Corre @("$P1", '--json')
    $doc = $salida | ConvertFrom-Json
    $o = $doc.objetivos | Select-Object -First 1
    Verifica ($codigo -eq 0 -and $doc.solo_listado) "sale 0 y no actuó"
    Verifica (@($doc.objetivos).Count -eq 1 -and $o.tipo -eq 'linux') "un objetivo de tipo linux"
    Verifica ("$($o.wsl.pid)" -eq $pid1) "el PID Linux coincide con pgrep ($pid1)"
    Verifica ($o.wsl.distro -eq $Distro -and $o.wsl.proceso -eq 'python3' -and $o.wsl.usuario -eq 'root') "distro, proceso y usuario"
    Verifica ($o.wsl.comando -eq "python3 -m http.server $P1 --bind 0.0.0.0") "línea de comandos"
    Verifica ($o.wsl.arranque -gt 0 -and $o.wsl.desde) "identidad (hora de arranque) y fecha"
    Verifica ($o.via.proceso -in 'wslrelay.exe', 'wslhost.exe') "via = el relay de Windows ($($o.via.proceso))"

    Paso "2. --wsl en el listado: la fila del relay dice quién escucha adentro"
    Corre @('--wsl', '--json')
    $doc = $salida | ConvertFrom-Json
    $filas = @($doc.escuchando | Where-Object { $_.puerto -in $P1, $P2, $P3 })
    Verifica ($doc.wsl.distros -contains $Distro) "el resumen nombra la distro que miró"
    Verifica ($filas.Count -eq 3 -and @($filas | Where-Object { $_.intermediario -eq 'wsl' -and $_.via -and $_.wsl.pid }).Count -eq 3) "tres filas con intermediario, via y proceso Linux"
    Verifica ("$(@($filas | Where-Object { $_.puerto -eq $P3 })[0].wsl.pid)" -eq $pid3) "también lo que escucha en loopback adentro ($P3 -> $pid3)"
    $sin = @((& $Killport --json | ConvertFrom-Json).escuchando | Where-Object { $_.puerto -eq $P1 })
    Verifica ($sin.Count -eq 1 -and $sin[0].intermediario -eq 'wsl' -and -not $sin[0].wsl -and $sin[0].proceso -in 'wslrelay.exe', 'wslhost.exe') "sin --wsl la fila es la del relay, sin mirar adentro"

    Paso "3. cerrar el proceso Linux (SIGINT)"
    Corre @("$P1", '-y')
    Verifica ($codigo -eq 0) "sale 0"
    Verifica ($salida -match 'SIGINT' -and $salida -match 'se detuvo') "lo cerró con SIGINT"
    Verifica ((PidLinux $P1) -eq '') "pgrep: el proceso murió"
    Verifica (Espera { -not (Escucha $P1) } 5) "el relay soltó el $P1"
    Verifica ((Escucha $P2) -and (Escucha $P3)) "los otros dos siguen"
    Verifica ((Relays) -eq $relaysAntes) "el relay no se tocó (mismo PID, misma hora de inicio)"

    Paso "4. un proceso sordo a SIGINT y SIGTERM: la escalera llega a SIGKILL"
    Corre @("$P2", '-y', '--timeout', '2s', '--json')
    $doc = $salida | ConvertFrom-Json
    $x = $doc.externos | Select-Object -First 1
    Verifica ($codigo -eq 0 -and $doc.libres -eq 1) "sale 0 con el puerto libre"
    Verifica ($x.tipo -eq 'linux' -and $x.destino -eq 'terminado' -and $x.clave -like "$Distro/$pid2@*") "el externo figura terminado en seco, con su identidad"
    Verifica ((PidLinux $P2) -eq '') "pgrep: el proceso murió"

    Paso "5. --force: directo a SIGKILL"
    Corre @("$P3", '-y', '--force')
    Verifica ($codigo -eq 0) "sale 0"
    Verifica ($salida -match 'SIGKILL' -and $salida -notmatch 'SIGINT') "la narración dice SIGKILL y no promete un SIGINT"
    Verifica ((PidLinux $P3) -eq '') "pgrep: el proceso murió"
    Verifica ((Relays) -eq $relaysAntes) "el relay sigue siendo el mismo"

    Paso "6. cancelar: matar al wsl.exe del lado de Windows corta el script de adentro"
    EnLinux "rm -f /tmp/navaja-oraculo-marca" | Out-Null
    $psi = [Diagnostics.ProcessStartInfo]::new($wsl, "-d $Distro -u root -e /bin/sh -s")
    $psi.RedirectStandardInput = $true; $psi.RedirectStandardOutput = $true; $psi.RedirectStandardError = $true
    $psi.UseShellExecute = $false; $psi.CreateNoWindow = $true
    $p = [Diagnostics.Process]::Start($psi)
    $p.StandardInput.Write("i=0; while [ `$i -lt 40 ]; do sleep 0.1; i=`$((i+1)); done; touch /tmp/navaja-oraculo-marca`n")
    $p.StandardInput.Close()
    Start-Sleep -Milliseconds 1200
    Stop-Process -Id $p.Id -Force
    Start-Sleep -Seconds 5
    Verifica ((EnLinux 'ls /tmp/navaja-oraculo-marca 2>/dev/null') -eq '') "el script no siguió corriendo después de morir su wsl.exe"
}
catch {
    Write-Host "  ERROR del oráculo: $_" -ForegroundColor Red
    $fail++
}
finally {
    Paso "limpieza"
    if ((Corriendo) -contains $Distro) {
        EnLinux "pkill -KILL -f '^python3 -m http.server'; rm -f /tmp/navaja-oraculo-marca" | Out-Null
        Verifica ((EnLinux "pgrep -f '^python3 -m http.server ($P1|$P2|$P3)'") -eq '') "ningún fixture vivo dentro de la distro"
    }
    foreach ($f in $fixtures) { if ($f -and -not $f.HasExited) { Stop-Process -Id $f.Id -Force -ErrorAction SilentlyContinue } }
    Verifica (-not ((Escucha $P1) -or (Escucha $P2) -or (Escucha $P3))) "ningún puerto de prueba quedó ocupado"
    if (-not $estabaCorriendo) {
        $sola = Espera { (Corriendo) -notcontains $Distro } 90
        Write-Host "la distro $Distro estaba detenida: $(if ($sola) { 'WSL la volvió a detener sola' } else { 'sigue corriendo (¿alguien más la usa?); no la detengo' })"
    }
    Write-Host "distros corriendo al terminar: [$((Corriendo) -join ', ')] (antes: [$($antes -join ', ')])"
}

if ($fail -eq 0) { Write-Host "`nWSL: TODO OK" -ForegroundColor Green; exit 0 }
Write-Host "`nWSL: $fail fallos" -ForegroundColor Red; exit 1
