<#
.SYNOPSIS
  Arma (o desarma con -Limpiar) la escena de la demo de killport: una tienda
  con tres servidores de desarrollo que escuchan de verdad.

.DESCRIPTION
  1. Compila killport desde este repo a demo\.escena\bin\killport.exe (versión
     limpia, sin commit estampado).
  2. Copia demo\tienda a demo\.escena\dev\tienda y la vuelve un repo git en
     la rama feat/carrito: killport muestra "tienda · feat/carrito".
  3. Monta demo\.escena como una unidad con subst (la primera libre de X, Y,
     Z, W, V): en pantalla la carpeta es X:\dev\tienda y no la ruta real de
     quien graba.
  4. Lanza, ocultos, web (node, 3000), api (python, 8080) y dev (node, 5173,
     solo localhost) desde servidores.ps1, y espera a que los tres escuchen.

  Imprime la carpeta de la tienda (X:\dev\tienda). Antes de lanzar nada
  comprueba que los tres puertos estén libres: si alguno está ocupado, lo dice
  y sale sin tocar nada.

  -Limpiar cierra lo que haya quedado de la escena, siempre por PID + hora de
  inicio (nunca por nombre), desmonta la unidad y borra demo\.escena.

.EXAMPLE
  .\preparar.ps1            # arma la escena y dice dónde quedó la tienda
  .\preparar.ps1 -Limpiar   # la desarma
#>
[CmdletBinding()]
param(
    [switch] $Limpiar
)
$ErrorActionPreference = 'Stop'

$demo    = $PSScriptRoot
$repo    = Split-Path $demo -Parent
$escena  = Join-Path $demo '.escena'
$estado  = Join-Path $escena 'estado.json'
$unidadF = Join-Path $escena 'unidad.txt'
$puertos = 3000, 8080, 5173

function Get-Unidad {
    if (Test-Path -LiteralPath $unidadF) { (Get-Content -LiteralPath $unidadF -Raw).Trim() }
}

function Stop-Escena {
    # Por identidad: el PID y la hora de inicio tienen que coincidir con lo
    # anotado; un PID reciclado no se toca. Primero la terminal, después los
    # servidores (que no dependen de ella).
    if (Test-Path -LiteralPath $estado) {
        foreach ($f in @(Get-Content -LiteralPath $estado -Raw | ConvertFrom-Json)) {
            $p = Get-Process -Id $f.pid -ErrorAction SilentlyContinue
            if ($p -and $p.StartTime.ToFileTimeUtc() -eq [long]$f.inicio) {
                Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
                $p.WaitForExit(5000) | Out-Null
            }
        }
    }
    $u = Get-Unidad
    if ($u) { subst.exe "${u}:" /d 2>$null | Out-Null }
    if (Test-Path -LiteralPath $escena) {
        # El .git de la tienda tiene archivos de solo lectura.
        Get-ChildItem -LiteralPath $escena -Recurse -Force -File -ErrorAction SilentlyContinue |
            ForEach-Object { $_.Attributes = 'Normal' }
        Remove-Item -LiteralPath $escena -Recurse -Force
    }
}

if ($Limpiar) {
    Stop-Escena
    Write-Host 'escena desarmada'
    exit 0
}

# Una escena vieja (una grabación cortada) se desarma antes.
Stop-Escena

# Los tres puertos tienen que estar libres: la demo no cierra nada ajeno.
$ocupados = Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue |
    Where-Object { $_.LocalPort -in $puertos } | Select-Object -ExpandProperty LocalPort -Unique
if ($ocupados) {
    Write-Host "la demo necesita libres los puertos $($puertos -join ', '); ocupados: $($ocupados -join ', ')"
    Write-Host "(killport dice quién: killport $($ocupados -join ' ') --list)"
    exit 1
}

foreach ($cmd in 'go', 'git', 'node') {
    if (-not (Get-Command $cmd -ErrorAction SilentlyContinue)) { Write-Host "falta $cmd en el PATH"; exit 1 }
}
if (-not (Get-Command python -ErrorAction SilentlyContinue) -and -not (Get-Command py -ErrorAction SilentlyContinue)) {
    Write-Host 'falta python en el PATH'; exit 1
}

New-Item -ItemType Directory -Path (Join-Path $escena 'bin') -Force | Out-Null

# 1. killport de este repo, con la versión limpia (sin +commit).
Push-Location $repo
$cgoAntes = $env:CGO_ENABLED
try {
    $env:CGO_ENABLED = '0'
    go build -trimpath -ldflags '-s -w' -o (Join-Path $escena 'bin\killport.exe') .
    if ($LASTEXITCODE -ne 0) { throw 'no compiló killport' }
} finally {
    $env:CGO_ENABLED = $cgoAntes
    Pop-Location
}

# 2. La tienda, como repo propio en su rama. Identidad y fecha fijas: la
#    escena sale igual en cada máquina.
$tienda = Join-Path $escena 'dev\tienda'
New-Item -ItemType Directory -Path $tienda -Force | Out-Null
Copy-Item -Path (Join-Path $demo 'tienda\*') -Destination $tienda -Recurse -Force
$env:GIT_AUTHOR_DATE = $env:GIT_COMMITTER_DATE = '2026-09-28T12:00:00-03:00'
$ident = @('-c', 'core.autocrlf=false', '-c', 'user.name=tienda', '-c', 'user.email=tienda@example.com', '-c', 'commit.gpgsign=false')
git -C $tienda init -q -b feat/carrito
git -C $tienda @ident add -A
git -C $tienda @ident commit -q -m 'tienda: carrito'
Remove-Item Env:GIT_AUTHOR_DATE, Env:GIT_COMMITTER_DATE

# 3. La unidad.
$libres = 'X', 'Y', 'Z', 'W', 'V' | Where-Object { -not (Test-Path "${_}:\") }
if (-not $libres) { throw 'no hay una unidad libre entre X, Y, Z, W y V para montar la escena' }
$u = @($libres)[0]
subst.exe "${u}:" $escena
if ($LASTEXITCODE -ne 0) { throw "subst ${u}: no anduvo" }
Set-Content -LiteralPath $unidadF -Value $u -Encoding ascii
$carpeta = "${u}:\dev\tienda"

# 4. Los servidores, desde su terminal oculta (que se suelta de este proceso).
#    (Start-Process -Wait no sirve: espera también a los descendientes, es
#    decir, a los servidores.)
$shell = (Get-Process -Id $PID).Path
$soltar = Start-Process $shell -WindowStyle Hidden -PassThru -ArgumentList @(
    '-NoLogo', '-NoProfile', '-ExecutionPolicy', 'Bypass',
    '-File', "`"$(Join-Path $demo 'servidores.ps1')`"", '-Carpeta', "`"$carpeta`"", '-Estado', "`"$estado`"", '-Soltar')
$soltar.WaitForExit()

# Hasta que los tres escuchen (como mucho 20 s).
$limite = (Get-Date).AddSeconds(20)
do {
    Start-Sleep -Milliseconds 200
    $escuchan = @(Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue |
        Where-Object { $_.LocalPort -in $puertos } | Select-Object -ExpandProperty LocalPort -Unique)
} until ($escuchan.Count -eq $puertos.Count -or (Get-Date) -gt $limite)
if ($escuchan.Count -ne $puertos.Count) {
    Stop-Escena
    throw "los servidores no llegaron a escuchar (escuchan: $($escuchan -join ', '))"
}
$carpeta
