<#
.SYNOPSIS
  La "terminal" de la tienda de la demo: lanza los tres servidores y queda
  esperándolos, como la pestaña donde alguien corrió npm run dev.

.DESCRIPTION
  No se corre a mano: lo lanza preparar.ps1, oculto. Con -Soltar se relanza a
  sí mismo (oculto) y sale enseguida: así esta terminal no tiene padre vivo y
  en el bloque de killport la cadena queda en "pwsh.exe → node.exe", sin la
  historia de quién grabó la demo (vhs, ttyd, el editor).

  Cada servidor arranca en su propia consola oculta y con la carpeta de la
  tienda como carpeta de trabajo: es lo que killport usa para decir de qué
  proyecto es y lo que filtra --project. Anota PID y hora de inicio de cada
  uno (y los suyos) en el archivo de estado: la limpieza cierra por
  identidad, nunca por nombre.

  Espera con Wait-Process (sin sondear) y termina sola cuando se cierran los
  tres.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string] $Carpeta,
    [Parameter(Mandatory)] [string] $Estado,
    [switch] $Soltar
)
$ErrorActionPreference = 'Stop'

if ($Soltar) {
    $shell = (Get-Process -Id $PID).Path
    Start-Process $shell -WindowStyle Hidden -ArgumentList @(
        '-NoLogo', '-NoProfile', '-ExecutionPolicy', 'Bypass',
        '-File', "`"$PSCommandPath`"", '-Carpeta', "`"$Carpeta`"", '-Estado', "`"$Estado`"")
    exit 0
}

# python o, si no está en el PATH, el lanzador py.
$python = if (Get-Command python -ErrorAction SilentlyContinue) { 'python' } else { 'py' }

$servidores = @(
    @{ Nombre = 'web'; Exe = 'node';  Args = 'web.js' }   # 3000, toda la red
    @{ Nombre = 'api'; Exe = $python; Args = 'api.py' }   # 8080, toda la red
    @{ Nombre = 'dev'; Exe = 'node';  Args = 'dev.js' }   # 5173, solo localhost (como Vite)
)

$procs = foreach ($s in $servidores) {
    Start-Process $s.Exe -ArgumentList $s.Args -WorkingDirectory $Carpeta -WindowStyle Hidden -PassThru
}

$me = Get-Process -Id $PID
$filas = @($me) + @($procs) | ForEach-Object {
    [pscustomobject]@{ pid = $_.Id; nombre = $_.ProcessName; inicio = $_.StartTime.ToFileTimeUtc() }
}
ConvertTo-Json -InputObject @($filas) | Set-Content -LiteralPath $Estado -Encoding utf8

# La terminal queda abierta mientras viva algún servidor.
$procs | Wait-Process -ErrorAction SilentlyContinue
