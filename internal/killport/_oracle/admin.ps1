# Prueba MANUAL de killport con administrador: lo que ninguna prueba automática puede hacer.
#
#   pwsh internal\tools\killport\_oracle\admin.ps1                 (en una consola DE ADMINISTRADOR)
#   pwsh internal\tools\killport\_oracle\admin.ps1 -Preparar       deja los fixtures puestos (para la parte del UAC)
#   pwsh internal\tools\killport\_oracle\admin.ps1 -Limpiar        los saca
#   pwsh internal\tools\killport\_oracle\admin.ps1 -ConWinnat      además, reserva un puerto tapado por WinNAT
#   pwsh internal\tools\killport\_oracle\admin.ps1 -ConDinamico    además, corre y arregla el rango dinámico
#
# Las pruebas de la suite (go test) no detienen servicios, no tocan la configuración de red y no
# pasan por el UAC: corren sin administrador. Este guion cubre esa mitad, con una persona delante.
# El procedimiento completo, con lo que tiene que salir en cada paso, está en ADMIN.md.
#
# TODO lo que toca lo crea él, con nombres y puertos de prueba, y lo saca en finally:
#   - la regla de portproxy 127.0.0.1:47999 -> 127.0.0.1:9;
#   - el servicio KillportPrueba (el doble testdata\e2e\serviciofalso, que escucha el 47997);
#   - la reserva del puerto 47998 (exclusión administrada, TCP, IPv4 e IPv6).
# No detiene ningún servicio de la máquina, no borra ninguna regla ni reserva que no sea suya.
# Las dos pruebas que SÍ tocan algo de todos (WinNAT, el rango dinámico) son opcionales, avisan
# antes y dejan todo como estaba.
#
# Las acciones de red (--reserve, --unreserve, --fix-dynamic) preguntan SIEMPRE y exigen una
# consola: por eso ahí killport corre pegado a la consola y contestás vos. El oráculo
# independiente es netsh, sc.exe y el registro.
[CmdletBinding()]
param(
    [string] $Killport = '',
    [switch] $Preparar,
    [switch] $Limpiar,
    [switch] $ConWinnat,
    [switch] $ConDinamico
)
$ErrorActionPreference = 'Continue'

# killport escribe UTF-8 cuando su salida va a un pipe, y pwsh decodifica lo que captura de un
# programa con [Console]::OutputEncoding, que en una consola recién abierta es la página OEM
# (850 en un Windows en castellano: medido). Sin esto "automático" llega como "automÃ¡tico" y
# cada comparación con acentos daría FALLA aunque killport anduviera bien. Se fija UTF-8
# mientras corre el guion y se devuelve la de antes al salir (la consola es compartida con quien
# lo lanzó).
$encAntes = [Console]::OutputEncoding
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
function Salir([int] $codigo) {
    [Console]::OutputEncoding = $encAntes
    exit $codigo
}

$here = $PSScriptRoot
$raiz = (Resolve-Path (Join-Path $here '..\..\..\..')).Path
$fail = 0

$PuertoRegla = 47999
$PuertoServicio = 47997
$PuertoReserva = 47998
$Servicio = 'KillportPrueba'
$trabajo = Join-Path $env:TEMP 'navaja-killport-admin'
$exeServicio = Join-Path $trabajo 'serviciofalso.exe'

function Paso([string] $t) { Write-Host "`n===== $t" }
function Verifica([bool] $ok, [string] $texto) {
    if ($ok) { Write-Host "  ok    $texto" -ForegroundColor Green }
    else { Write-Host "  FALLA $texto" -ForegroundColor Red; $script:fail++ }
}
# Corre killport capturando la salida (sin consola: no pregunta).
function Corre([string[]] $a) {
    $script:salida = (& $Killport @a 2>&1 | Out-String)
    $script:codigo = $LASTEXITCODE
    Write-Host "> killport $($a -join ' ')   [código $script:codigo]"
    Write-Host $script:salida.TrimEnd()
}
# Corre killport PEGADO A LA CONSOLA: pregunta, y contesta la persona.
function CorreConVos([string[]] $a, [string] $queContestar) {
    Write-Host ""
    Write-Host ">>> killport $($a -join ' ')" -ForegroundColor Cyan
    Write-Host ">>> cuando pregunte, contestá: $queContestar" -ForegroundColor Cyan
    & $Killport @a
    $script:codigo = $LASTEXITCODE
    Write-Host ">>> [código $script:codigo]" -ForegroundColor Cyan
}
function Escucha([int] $p) { [bool](Get-NetTCPConnection -LocalPort $p -State Listen -ErrorAction SilentlyContinue) }
function Espera([scriptblock] $cond, [int] $segundos = 20) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    while (-not (& $cond) -and $sw.Elapsed.TotalSeconds -lt $segundos) { Start-Sleep -Milliseconds 200 }
    [bool](& $cond)
}
# Las filas "inicio fin [*]" de netsh ... show excludedportrange (la salida sale traducida: se
# leen los números).
function Exclusiones([string] $ipv, [string] $proto) {
    netsh interface $ipv show excludedportrange protocol=$proto | ForEach-Object {
        if ($_ -match '^\s*(\d+)\s+(\d+)\s*(\*?)\s*$') {
            [pscustomobject]@{ Desde = [int]$Matches[1]; Hasta = [int]$Matches[2]; Administrada = ($Matches[3] -eq '*') }
        }
    }
}
function Reservado([string] $ipv, [int] $p) {
    [bool](Exclusiones $ipv 'tcp' | Where-Object { $_.Administrada -and $_.Desde -le $p -and $_.Hasta -ge $p })
}
function Dinamico([string] $ipv, [string] $proto) {
    $n = @(netsh interface $ipv show dynamicport $proto | ForEach-Object { if ($_ -match ':\s*(\d+)\s*$') { [int]$Matches[1] } })
    [pscustomobject]@{ Inicio = $n[0]; Cantidad = $n[1] }
}
function ReglaDePrueba {
    $k = 'HKLM:\SYSTEM\CurrentControlSet\Services\PortProxy\v4tov4\tcp'
    if (-not (Test-Path $k)) { return $null }
    (Get-ItemProperty -Path $k -ErrorAction SilentlyContinue)."127.0.0.1/$PuertoRegla"
}
function InicioDe([string] $svc) {
    # START_TYPE : 2 AUTO_START / 3 DEMAND_START / 4 DISABLED (los números no se traducen)
    $l = sc.exe qc $svc | Where-Object { $_ -match ':\s*([0-9])\s+(AUTO_START|DEMAND_START|DISABLED|BOOT_START|SYSTEM_START)' } | Select-Object -First 1
    if ($l -match ':\s*([0-9])\s+([A-Z_]+)') { $Matches[2] } else { '' }
}
function EstadoDe([string] $svc) {
    $l = sc.exe query $svc | Where-Object { $_ -match ':\s*([0-9])\s+(RUNNING|STOPPED|START_PENDING|STOP_PENDING)' } | Select-Object -First 1
    if ($l -match ':\s*([0-9])\s+([A-Z_]+)') { $Matches[2] } else { '' }
}

function PonerRegla {
    netsh interface portproxy add v4tov4 listenport=$PuertoRegla listenaddress=127.0.0.1 connectport=9 connectaddress=127.0.0.1 | Out-Null
    Espera { Escucha $PuertoRegla } | Out-Null
}
function SacarRegla {
    if (ReglaDePrueba) { netsh interface portproxy delete v4tov4 listenport=$PuertoRegla listenaddress=127.0.0.1 | Out-Null }
}
function PonerServicio {
    New-Item -ItemType Directory -Force $trabajo | Out-Null
    Push-Location $raiz
    go build -o $exeServicio ./internal/tools/killport/testdata/e2e/serviciofalso
    $ok = $LASTEXITCODE -eq 0
    Pop-Location
    if (-not $ok) { throw 'no compila el servicio de prueba' }
    sc.exe create $Servicio binPath= "`"$exeServicio`" -puerto $PuertoServicio" start= auto DisplayName= 'killport: servicio de prueba' | Out-Null
    sc.exe start $Servicio | Out-Null
    Espera { Escucha $PuertoServicio } | Out-Null
}
function SacarServicio {
    if (sc.exe query $Servicio 2>$null | Select-String 'SERVICE_NAME|NOMBRE_SERVICIO') {
        sc.exe stop $Servicio *> $null
        Espera { (EstadoDe $Servicio) -ne 'RUNNING' -and (EstadoDe $Servicio) -ne 'STOP_PENDING' } 10 | Out-Null
        sc.exe delete $Servicio | Out-Null
    }
    Remove-Item -LiteralPath $exeServicio -ErrorAction SilentlyContinue
}
function SacarReserva {
    foreach ($ipv in 'ipv4', 'ipv6') {
        if (Reservado $ipv $PuertoReserva) {
            netsh interface $ipv delete excludedportrange protocol=tcp startport=$PuertoReserva numberofports=1 store=persistent | Out-Null
        }
    }
}

# ---------- requisitos ----------
$admin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $admin) {
    Write-Host "SALTEADO: esta consola no es de administrador."
    Write-Host "  Abrí una (Windows Terminal > clic derecho > Ejecutar como administrador) y corré de nuevo:"
    Write-Host "  pwsh $($MyInvocation.MyCommand.Path)"
    Salir 0
}
if ($Limpiar) {
    SacarRegla; SacarServicio; SacarReserva
    Write-Host "listo: sin la regla del $PuertoRegla, sin el servicio $Servicio y sin la reserva del $PuertoReserva"
    Salir 0
}
if (-not $Killport) {
    New-Item -ItemType Directory -Force $trabajo | Out-Null
    $Killport = Join-Path $trabajo 'killport.exe'
    Push-Location $raiz
    go build -o $Killport ./cmd/killport
    $ok = $LASTEXITCODE -eq 0
    Pop-Location
    if (-not $ok) { Write-Host "no compila killport" -ForegroundColor Red; Salir 1 }
}
if ($Preparar) {
    # Repone lo que falte (se puede correr entre una prueba y otra de la parte B).
    if (-not (ReglaDePrueba)) { PonerRegla }
    if (-not (sc.exe query $Servicio 2>$null | Select-String 'RUNNING|STOPPED|PENDING')) { PonerServicio }
    else {
        sc.exe config $Servicio start= auto | Out-Null
        if ((EstadoDe $Servicio) -ne 'RUNNING') { sc.exe start $Servicio | Out-Null }
        Espera { Escucha $PuertoServicio } | Out-Null
    }
    $ok = (Escucha $PuertoRegla) -and (Escucha $PuertoServicio) -and (InicioDe $Servicio) -eq 'AUTO_START'
    if (-not $ok) { Write-Host "no pude dejar los fixtures puestos (regla: $(Escucha $PuertoRegla), servicio: $(EstadoDe $Servicio) / $(InicioDe $Servicio))" -ForegroundColor Red; Salir 1 }
    Write-Host "listo: la regla de portproxy del $PuertoRegla y el servicio $Servicio (puerto $PuertoServicio, inicio automático) están puestos."
    Write-Host "killport para la parte del UAC: $Killport"
    Write-Host "Seguí con la PARTE B de ADMIN.md, en una consola SIN administrador. Al terminar: admin.ps1 -Limpiar"
    Salir 0
}
foreach ($p in $PuertoRegla, $PuertoServicio, $PuertoReserva) {
    if (Escucha $p) { Write-Host "el puerto de prueba $p está ocupado: corré -Limpiar, o liberalo, y volvé a probar" -ForegroundColor Red; Salir 1 }
    # Los bloques de WinNAT cambian en cada arranque y salen del rango dinámico (49152 en
    # adelante, si nadie lo corrió): los puertos de prueba quedan abajo a propósito. Si igual
    # cayeran en una exclusión, la regla y el servicio no podrían escuchar y el paso 3 vería a
    # WinNAT: fallaría la máquina, no killport. Se avisa antes de tocar nada.
    foreach ($ipv in 'ipv4', 'ipv6') {
        $tapa = Exclusiones $ipv 'tcp' | Where-Object { $_.Desde -le $p -and $_.Hasta -ge $p } | Select-Object -First 1
        if ($tapa) {
            Write-Host "el puerto de prueba $p cae en una exclusión de $ipv ($($tapa.Desde)–$($tapa.Hasta)$(if ($tapa.Administrada) { ' *' })): con esta máquina así la prueba no dice nada; reiniciá WinNAT o la máquina, o corré -Limpiar si es una reserva de una prueba anterior" -ForegroundColor Red
            Salir 1
        }
    }
}

$winnatAntes = EstadoDe 'winnat'
$iphlpAntes = (Get-Process -Id (Get-CimInstance Win32_Service -Filter "Name='iphlpsvc'").ProcessId -ErrorAction SilentlyContinue).StartTime
$reglasAntes = (netsh interface portproxy show all | Out-String)
$dinamicoAntes = Dinamico 'ipv4' 'tcp'

try {
    # ================= función 3: la regla de portproxy =================
    Paso "1. una regla de portproxy (el puerto figura a nombre de svchost / iphlpsvc)"
    PonerRegla
    Verifica (Escucha $PuertoRegla) "la regla de prueba escucha el $PuertoRegla"
    Corre @("$PuertoRegla", '--list', '--no-color')
    Verifica ($salida -match "regla de portproxy 127\.0\.0\.1:$PuertoRegla") 'el objetivo es LA REGLA, no el svchost'
    Verifica ($salida -match 'netsh interface portproxy delete v4tov4') 'dice el comando que la borra'
    Verifica ($salida -notmatch '(?i)net stop iphlpsvc') 'no aconseja detener el IP Helper'
    Corre @("$PuertoRegla", '--json')
    $doc = $salida | ConvertFrom-Json
    Verifica ($doc.objetivos[0].tipo -eq 'regla' -and $doc.objetivos[0].regla.puerto -eq $PuertoRegla -and $doc.objetivos[0].regla.destino -eq '127.0.0.1:9') 'JSON: objetivo de tipo regla, con su destino'
    Corre @("$PuertoRegla", '-y', '--no-color')
    Verifica ($codigo -eq 0) 'sale 0'
    Verifica ($salida -match 'quedó borrada') 'narra que la borró'
    Verifica ($salida -match 'Reglas borradas\s+1') 'la tarjeta cuenta la regla'
    Verifica (-not (ReglaDePrueba)) 'la regla ya no está en el registro'
    Verifica (Espera { -not (Escucha $PuertoRegla) } 10) "nadie escucha el $PuertoRegla"
    $iphlp = Get-CimInstance Win32_Service -Filter "Name='iphlpsvc'"
    Verifica ($iphlp.State -eq 'Running' -and (Get-Process -Id $iphlp.ProcessId).StartTime -eq $iphlpAntes) 'el IP Helper sigue corriendo, y es el mismo proceso'
    Verifica ((netsh interface portproxy show all | Out-String) -eq $reglasAntes) 'las demás reglas de la máquina quedaron como estaban'

    # ================= función 9: el servicio =================
    Paso "2. un servicio de terceros (detener, y que no vuelva)"
    PonerServicio
    Verifica ((EstadoDe $Servicio) -eq 'RUNNING' -and (Escucha $PuertoServicio)) "el servicio de prueba corre y escucha el $PuertoServicio"
    Verifica ((InicioDe $Servicio) -eq 'AUTO_START') 'arranca en automático'
    Corre @("$PuertoServicio", '--list', '--no-color')
    Verifica ($salida -match "servicio $Servicio · inicio automático: vuelve al reiniciar Windows") 'el bloque dice cómo arranca y que vuelve'
    Verifica ($salida -match "para que no vuelva: killport $PuertoServicio --manual") 'y cómo evitarlo'
    Corre @("$PuertoServicio", '-y', '--no-color')
    Verifica ($codigo -eq 0 -and $salida -match "Servicio $Servicio detenido") 'sin --manual: lo detiene'
    Verifica ((EstadoDe $Servicio) -eq 'STOPPED' -and (InicioDe $Servicio) -eq 'AUTO_START') 'quedó detenido y SIGUE en automático'
    Verifica ($salida -match "El servicio $Servicio va a volver") 'avisa que va a volver'
    sc.exe start $Servicio | Out-Null
    Verifica (Espera { Escucha $PuertoServicio }) 'el servicio volvió a arrancar (a mano)'
    Corre @("$PuertoServicio", '-y', '--manual', '--no-color')
    Verifica ($codigo -eq 0) 'con --manual: sale 0'
    Verifica ((EstadoDe $Servicio) -eq 'STOPPED') 'quedó detenido'
    Verifica ((InicioDe $Servicio) -eq 'DEMAND_START') 'quedó en inicio MANUAL'
    Verifica ($salida -match "se deshace con ``sc config $Servicio start= auto``") 'dice cómo deshacerlo'
    Verifica ($salida -match 'En inicio manual\s+1') 'la tarjeta lo cuenta'
    sc.exe start $Servicio | Out-Null
    Espera { Escucha $PuertoServicio } | Out-Null
    Corre @("$PuertoServicio", '-y', '--manual', '--no-color')
    Verifica ($salida -match 'ya estaba en inicio manual') 'otra vez con --manual: ya estaba'
    SacarServicio

    # ================= función 4: reservar =================
    Paso "3. reservar un puerto (no está tapado por WinNAT: WinNAT no se toca)"
    Corre @("$PuertoReserva", '--reserve', '--list', '--no-color')
    Verifica ($salida -notmatch 'detener WinNAT') 'el plan no detiene WinNAT'
    Corre @("$PuertoReserva", '--reserve', '--yes', '--no-color')
    Verifica ($codigo -eq 3 -and $salida -match 'No cambié nada' -and -not (Reservado 'ipv4' $PuertoReserva)) 'sin consola no hace nada, ni con --yes'
    CorreConVos @("$PuertoReserva", '--reserve') 's'
    Verifica ($codigo -eq 0) 'sale 0'
    Verifica ((Reservado 'ipv4' $PuertoReserva) -and (Reservado 'ipv6' $PuertoReserva)) "netsh muestra el $PuertoReserva como exclusión administrada (*) en IPv4 y en IPv6"
    Verifica ((EstadoDe 'winnat') -eq $winnatAntes) 'WinNAT quedó como estaba'
    Corre @("$PuertoReserva", '--no-color')
    Verifica ($codigo -eq 0 -and $salida -match 'libre' -and $salida -match 'exclusión administrada') 'killport dice LIBRE (una exclusión administrada se puede usar)'
    $l = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Any, $PuertoReserva)
    try { $l.Start(); $ato = $true } catch { $ato = $false } finally { $l.Stop() }
    Verifica $ato 'un servidor lo puede tomar'
    CorreConVos @("$PuertoReserva", '--reserve') '(no pregunta: ya está reservado)'
    Verifica ($codigo -eq 0) 'reservar dos veces: no hay nada que hacer, sale 0'
    CorreConVos @("$PuertoReserva", '--unreserve') 's'
    Verifica ($codigo -eq 0 -and -not (Reservado 'ipv4' $PuertoReserva) -and -not (Reservado 'ipv6' $PuertoReserva)) 'la reserva se quitó de las dos familias'
    CorreConVos @("$PuertoReserva", '--reserve') 'n'
    Verifica ($codigo -eq 130 -and -not (Reservado 'ipv4' $PuertoReserva)) 'decir que no: no cambia nada, sale 130'

    if ($ConWinnat) {
        Paso "4. reservar un puerto TAPADO por WinNAT (corta un instante la red de WSL, Docker y Hyper-V)"
        $bloque = Exclusiones 'ipv4' 'tcp' | Where-Object { -not $_.Administrada } | Select-Object -First 1
        if (-not $bloque) { Write-Host "  SALTEADO: WinNAT no tiene ningún bloque reservado en esta máquina" }
        else {
            $p = $bloque.Desde + 1
            CorreConVos @("$p", '--reserve') 's'
            Verifica ($codigo -eq 0) 'sale 0'
            Verifica ((EstadoDe 'winnat') -eq 'RUNNING') 'WinNAT volvió a arrancar'
            Verifica ((Reservado 'ipv4' $p) -and (Reservado 'ipv6' $p)) "el $p quedó reservado"
            $l = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Any, $p)
            try { $l.Start(); $ato = $true } catch { $ato = $false } finally { $l.Stop() }
            Verifica $ato 'un servidor lo puede tomar'
            CorreConVos @("$p", '--unreserve') 's'
            Verifica (-not (Reservado 'ipv4' $p) -and -not (Reservado 'ipv6' $p)) 'la reserva se quitó'
            foreach ($ipv in 'ipv4', 'ipv6') { if (Reservado $ipv $p) { netsh interface $ipv delete excludedportrange protocol=tcp startport=$p numberofports=1 store=persistent | Out-Null } }
        }
    }

    Paso "5. el rango dinámico"
    Corre @('--fix-dynamic', '--list', '--no-color')
    if ($dinamicoAntes.Inicio -eq 49152) { Verifica ($salida -match 'No hay nada que hacer') 'con el rango de fábrica no hay nada que hacer' }
    if ($ConDinamico) {
        if ($dinamicoAntes.Inicio -ne 49152 -or $dinamicoAntes.Cantidad -ne 16384) {
            Write-Host "  SALTEADO: el rango dinámico de TCP IPv4 no es el de fábrica ($($dinamicoAntes.Inicio) + $($dinamicoAntes.Cantidad)): no lo toco"
        }
        else {
            Write-Host "  corro el rango dinámico de TCP IPv4 a 1025-5000 (el de Windows XP) para que haya algo que arreglar"
            netsh interface ipv4 set dynamicport tcp start=1025 num=3976 | Out-Null
            Corre @('--fix-dynamic', '--tcp', '--list', '--no-color')
            Verifica ($salida -match 'rango dinámico TCP IPv4: 1025–5000') 'killport ve el rango corrido'
            Verifica ($salida -match 'netsh interface ipv4 set dynamicport protocol=tcp startport=49152 numberofports=16384') 'el plan dice el comando exacto'
            Verifica ($salida -notmatch 'interface ipv6') 'y no toca el de IPv6, que está bien'
            CorreConVos @('--fix-dynamic', '--tcp') 's'
            $d = Dinamico 'ipv4' 'tcp'
            Verifica ($codigo -eq 0 -and $d.Inicio -eq 49152 -and $d.Cantidad -eq 16384) 'el rango volvió a 49152 + 16384'
        }
    }
}
catch { Write-Host "  ERROR: $_" -ForegroundColor Red; $fail++ }
finally {
    Paso "limpieza"
    SacarRegla; SacarServicio; SacarReserva
    $d = Dinamico 'ipv4' 'tcp'
    if ($d.Inicio -ne $dinamicoAntes.Inicio -or $d.Cantidad -ne $dinamicoAntes.Cantidad) {
        netsh interface ipv4 set dynamicport tcp start=$($dinamicoAntes.Inicio) num=$($dinamicoAntes.Cantidad) | Out-Null
    }
    if ($winnatAntes -eq 'RUNNING' -and (EstadoDe 'winnat') -ne 'RUNNING') { sc.exe start winnat | Out-Null }
    Verifica (-not (ReglaDePrueba)) 'sin la regla de prueba'
    Verifica (-not (sc.exe query $Servicio 2>$null | Select-String 'RUNNING|STOPPED')) 'sin el servicio de prueba'
    Verifica (-not (Reservado 'ipv4' $PuertoReserva) -and -not (Reservado 'ipv6' $PuertoReserva)) 'sin la reserva de prueba'
    Verifica ((EstadoDe 'winnat') -eq $winnatAntes) "WinNAT como estaba ($winnatAntes)"
    $d = Dinamico 'ipv4' 'tcp'
    Verifica ($d.Inicio -eq $dinamicoAntes.Inicio -and $d.Cantidad -eq $dinamicoAntes.Cantidad) "el rango dinámico como estaba ($($dinamicoAntes.Inicio) + $($dinamicoAntes.Cantidad))"
}
if ($fail -eq 0) { Write-Host "`nADMIN: TODO OK" -ForegroundColor Green; Salir 0 }
Write-Host "`nADMIN: $fail fallos" -ForegroundColor Red; Salir 1
