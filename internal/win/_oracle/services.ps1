# Oráculo de servicios: enumeración (dump.exe svcs) vs Win32_Service
# (ProcessId, Name, State) y caminos de ERROR de StopService (sin detener nada).
param([string]$Dump = "$PSScriptRoot\dump.exe")
$ErrorActionPreference = 'Stop'

$go = & $Dump svcs | ConvertFrom-Json
$goByName = @{}; foreach ($s in $go) { $goByName[$s.Name] = $s }

# Win32_Service: comparamos SOLO los activos (dump enumera SERVICE_ACTIVE).
$cim = Get-CimInstance Win32_Service | Where-Object { $_.State -eq 'Running' }
$stateMap = @{ Stopped=1; StartPending=2; StopPending=3; Running=4 }

$nPid=0;$nState=0;$nName=0;$total=0;$missing=0
foreach ($w in $cim) {
    $g = $goByName[$w.Name]
    $total++
    if (-not $g) { $missing++; continue }
    $nName++
    if ([uint32]$g.PID -eq [uint32]$w.ProcessId) { $nPid++ }
    else { if (($total-$nPid) -le 6) { Write-Host "PID difiere $($w.Name): go=$($g.PID) cim=$($w.ProcessId)" } }
    if ($g.State -eq $stateMap[$w.State]) { $nState++ }
}
Write-Host "Win32_Service (Running): total=$total · en go faltan=$missing"
Write-Host "coinciden: Name=$nName PID=$nPid State=$nState (de $($total-$missing) comunes)"

Write-Host "`n== caminos de error de StopService (no detiene nada) =="
Write-Host "elevado? $(& $Dump elevated)"
# 1) servicio inexistente → ErrServiceNotFound
& $Dump svc-stop '__no_existe_este_servicio__'
# 2) W32Time sin admin → ErrAccessDenied (OpenService con SERVICE_STOP se niega
#    ANTES de cualquier control, así que el servicio no se toca).
$before = (Get-Service W32Time).Status
& $Dump svc-stop 'W32Time'
$after = (Get-Service W32Time).Status
Write-Host "W32Time antes=$before despues=$after (tiene que ser IGUAL: no se detuvo nada)"
if ($before -ne $after) { Write-Host "ALERTA: W32Time cambió de estado" -ForegroundColor Red; exit 1 }
