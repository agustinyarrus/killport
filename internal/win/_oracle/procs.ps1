# Oráculo de procesos: dump.exe procs (NtQuerySystemInformation) vs
# Get-CimInstance Win32_Process (ProcessId, ParentProcessId, CreationDate,
# SessionId, Name). Reporta cuántos coinciden en cada campo y explica las
# diferencias (procesos que nacen/mueren entre las dos lecturas; PID 0 sin
# nombre = "System Idle Process" en WMI).
param([string]$Dump = "$PSScriptRoot\dump.exe")
$ErrorActionPreference = 'Stop'

$go = & $Dump procs | ConvertFrom-Json
$byPid = @{}; foreach ($p in $go) { $byPid[[uint32]$p.PID] = $p }
$cim = Get-CimInstance Win32_Process

$nPid=0;$nName=0;$nPPid=0;$nSess=0;$nCreate=0;$total=0;$missing=0;$idle=0
foreach ($w in $cim) {
    $g = $byPid[[uint32]$w.ProcessId]; $total++
    if (-not $g) { $missing++; continue }
    if ($g.PID -eq $w.ProcessId) { $nPid++ }
    if ($g.Image -eq $w.Name) { $nName++ }
    elseif ($w.ProcessId -eq 0) { $idle++ } # PID 0: go="" WMI="System Idle Process"
    if ($g.PPID -eq $w.ParentProcessId) { $nPPid++ }
    if ($g.Session -eq $w.SessionId) { $nSess++ }
    if ($w.CreationDate) {
        $goUtc = if ($g.Created) { ([DateTime]$g.Created).ToUniversalTime() } else { [DateTime]::MinValue }
        if ([Math]::Abs(($goUtc - $w.CreationDate.ToUniversalTime()).TotalSeconds) -lt 1.0) { $nCreate++ }
    } else { $nCreate++ }
}
$comunes = $total - $missing
Write-Host "Win32_Process total=$total · en go faltan=$missing (nacieron entre las dos lecturas)"
Write-Host "coinciden (de $comunes comunes): PID=$nPid Name=$nName(+$idle idle) PPID=$nPPid Session=$nSess Creation(<1s)=$nCreate"
if (($nPid -eq $comunes) -and (($nName+$idle) -eq $comunes) -and ($nPPid -eq $comunes) -and ($nSess -eq $comunes) -and ($nCreate -eq $comunes)) {
    Write-Host "TODOS los campos coinciden en los procesos comunes" -ForegroundColor Green
}
