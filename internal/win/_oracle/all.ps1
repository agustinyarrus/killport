# Corre toda la verificación de internal/win contra oráculos INDEPENDIENTES de
# Windows (netstat, Get-NetTCPConnection/UDPEndpoint, Win32_Process,
# Win32_Service) y pruebas end-to-end con procesos reales.
#
#   pwsh internal\win\_oracle\all.ps1
#
# Los directorios con _ no los compila `go build ./...`; los .exe de prueba se
# compilan acá al vuelo. No detiene servicios ni deja procesos corriendo.
$ErrorActionPreference = 'Stop'
$here = $PSScriptRoot
Push-Location (Join-Path $here '..\..\..')  # raíz del módulo

Write-Host "### compilando los programas de prueba en _oracle ###"
go build -o "$here\dump.exe"         ./internal/win/_oracle/dump
go build -o "$here\ctrlc-helper.exe" ./internal/win/_oracle/ctrlc-helper
go build -o "$here\shellexec.exe"    ./internal/win/_oracle/shellexec
Pop-Location

Write-Host "`n### go test ./internal/win/ ###"
go test ./internal/win/ -count=1

Write-Host "`n############ SOCKETS ############";  & "$here\sockets.ps1"  -Dump "$here\dump.exe"
Write-Host "`n############ PROCESOS ############"; & "$here\procs.ps1"    -Dump "$here\dump.exe"
Write-Host "`n############ SERVICIOS ############";& "$here\services.ps1" -Dump "$here\dump.exe"
Write-Host "`n############ MODULO DUENO ############"; & "$here\owner.ps1" -Dump "$here\dump.exe"
Write-Host "`n############ CTRL+C (end-to-end) ############"; & "$here\ctrlc.ps1"
Write-Host "`n############ SHELLEXECUTE (verbo open) ############"
& "$here\shellexec.exe" 42
& "$here\shellexec.exe" 7
Write-Host "`n(el verbo 'runas' dispara UAC y no se puede automatizar: probar a mano)"
