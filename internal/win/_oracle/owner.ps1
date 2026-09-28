# Oráculo del módulo dueño (GetOwnerModuleFrom*Entry, vía Socket.Owner). Sin
# admin, contra procesos propios, de servicios (svchost) y del sistema:
# documenta qué devuelve cada uno (nombre de servicio en un svchost, como
# netstat -b) y si aparece acceso denegado. En esta máquina NO dio acceso
# denegado para ninguno.
param([string]$Dump = "$PSScriptRoot\dump.exe")
$ErrorActionPreference = 'Stop'
$node = (Get-Command node -ErrorAction SilentlyContinue).Source
$marker = Join-Path $PSScriptRoot 'm_owner.txt'

if ($node) {
    $srv = Start-Process $node -ArgumentList (Join-Path $PSScriptRoot 'sigint-server.js'), $marker, '39555' -PassThru -WindowStyle Hidden
    for ($i=0;$i -lt 40 -and -not (Test-Path "$marker.ready");$i++){Start-Sleep -Milliseconds 100}
    Write-Host "== proceso propio (node) puerto 39555 =="
    & $Dump owner 39555
    Stop-Process -Id $srv.Id -Force -ErrorAction SilentlyContinue
    Remove-Item $marker, "$marker.ready" -ErrorAction SilentlyContinue
}
Write-Host "== 135 (svchost: servicio RpcSs) — el nombre del SERVICIO, no del host =="
& $Dump owner 135
Write-Host "== 445 (System, PID 4) =="
& $Dump owner 445
