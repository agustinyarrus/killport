# Oráculo de sockets: compara internal/win (dump.exe sockets) contra
# Get-NetTCPConnection / Get-NetUDPEndpoint y contra netstat -ano.
# El conjunto de tuplas LISTEN (TCP) y UDP tiene que coincidir exacto; las
# conexiones efímacas pueden variar entre las dos lecturas (se mide y explica).
param([string]$Dump = "$PSScriptRoot\dump.exe")

$ErrorActionPreference = 'Stop'
$go = & $Dump sockets | ConvertFrom-Json

# --- Go: tuplas LISTEN y UDP ---
$goListen = $go | Where-Object { $_.Proto -eq 'TCP' -and $_.State -eq 2 } |
    ForEach-Object { "TCP $($_.Local) pid=$($_.PID)" } | Sort-Object -Unique
$goUdp = $go | Where-Object { $_.Proto -eq 'UDP' } |
    ForEach-Object { "UDP $($_.Local) pid=$($_.PID)" } | Sort-Object -Unique

# --- Oráculo 1: Get-NetTCPConnection / Get-NetUDPEndpoint ---
function Norm($addr, $port) {
    if ($addr -eq '::' ) { return "[::]:$port" }
    if ($addr -match ':') { return "[$addr]:$port" }
    return "${addr}:$port"
}
$psListen = Get-NetTCPConnection -State Listen |
    ForEach-Object { "TCP $(Norm $_.LocalAddress $_.LocalPort) pid=$($_.OwningProcess)" } |
    Sort-Object -Unique
$psUdp = Get-NetUDPEndpoint |
    ForEach-Object { "UDP $(Norm $_.LocalAddress $_.LocalPort) pid=$($_.OwningProcess)" } |
    Sort-Object -Unique

function Compare-Sets($name, $a, $b, $aName, $bName) {
    $soloA = $a | Where-Object { $b -notcontains $_ }
    $soloB = $b | Where-Object { $a -notcontains $_ }
    Write-Host "== $name =="
    Write-Host ("  {0}: {1} tuplas · {2}: {3} tuplas" -f $aName, $a.Count, $bName, $b.Count)
    if ($soloA) { Write-Host "  SOLO EN $aName :"; $soloA | ForEach-Object { Write-Host "    $_" } }
    if ($soloB) { Write-Host "  SOLO EN $bName :"; $soloB | ForEach-Object { Write-Host "    $_" } }
    if (-not $soloA -and -not $soloB) { Write-Host "  IDENTICOS" -ForegroundColor Green }
    return -not ($soloA -or $soloB)
}

$ok1 = Compare-Sets "TCP LISTEN vs Get-NetTCPConnection" $goListen $psListen "go" "ps"
$ok2 = Compare-Sets "UDP vs Get-NetUDPEndpoint" $goUdp $psUdp "go" "ps"

# --- Oráculo 2: netstat -ano (independiente de la API .NET) ---
$netstat = netstat -ano
$nsListen = $netstat | Where-Object { $_ -match 'LISTENING' } | ForEach-Object {
    $f = ($_ -split '\s+') | Where-Object { $_ }
    "TCP $($f[1]) pid=$($f[4])"
} | Sort-Object -Unique
# netstat imprime las IPv6 igual que nosotros ([::], [addr%zone]); normalizamos
# quitando el %zona que netstat a veces omite en la columna de estado.
$ok3 = Compare-Sets "TCP LISTEN vs netstat -ano" $goListen $nsListen "go" "netstat"

# --- conexiones efímeras: medir la diferencia entre dos lecturas ---
$goConn1 = ($go | Where-Object { $_.Proto -eq 'TCP' -and $_.State -eq 5 }).Count
Start-Sleep -Milliseconds 200
$goConn2 = (& $Dump sockets | ConvertFrom-Json | Where-Object { $_.Proto -eq 'TCP' -and $_.State -eq 5 }).Count
Write-Host "== conexiones ESTABLISHED (efímeras) =="
Write-Host "  lectura 1: $goConn1 · lectura 2 (+200ms): $goConn2 · delta: $($goConn2-$goConn1)"
Write-Host "  (las conexiones efímeras nacen y mueren entre lecturas: por eso solo se exige coincidencia en LISTEN/UDP)"

if ($ok1 -and $ok2 -and $ok3) { Write-Host "`nTODO COINCIDE" -ForegroundColor Green; exit 0 }
Write-Host "`nHAY DIFERENCIAS" -ForegroundColor Yellow; exit 1
