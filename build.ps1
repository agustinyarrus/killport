<#
.SYNOPSIS
  Compila killport: un único .exe portable en dist\killport.exe.

.DESCRIPTION
  Go puro (sin CGO), sin símbolos de depuración (-s -w) y sin rutas locales
  (-trimpath), con la versión y el commit estampados (se ven con
  killport --version). Al final muestra el tamaño y el SHA256 del .exe.

.EXAMPLE
  .\build.ps1           # compila a dist\killport.exe
  .\build.ps1 -Test     # antes corre go vet y todos los tests
  .\build.ps1 -Out C:\herramientas
#>
[CmdletBinding()]
param(
    [switch] $Test,
    [string] $Out = 'dist'
)
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

# ESC como [char]27 (y no `e) para que corra también en Windows PowerShell 5.1.
$e = [char]27
function Paint([string] $rgb, [string] $s) { "$e[38;2;${rgb}m$s$e[0m" }
$lav = '196;181;253'; $sage = '181;223;168'; $rose = '242;167;184'; $sub = '138;143;168'; $faint = '85;88;107'
$ar = [Globalization.CultureInfo]::GetCultureInfo('es-AR')

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host "  $(Paint $rose '✗') falta Go: https://go.dev/dl (o winget install GoLang.Go)"
    exit 1
}

$module = 'github.com/agustinyarrus/killport'
$version = ([regex]::Match((Get-Content internal\version\version.go -Raw), 'Version\s*=\s*"([^"]+)"')).Groups[1].Value
$commit = 'dev'
if (Get-Command git -ErrorAction SilentlyContinue) {
    # 7 fijo, no el largo automático de git (crece con los objetos del repo):
    # el mismo commit tiene que estampar lo mismo en cualquier clon para que
    # el .exe sea reproducible byte a byte (ver docs\RELEASE.md).
    $c = git rev-parse --short=7 HEAD 2>$null
    if ($LASTEXITCODE -eq 0 -and $c) {
        $commit = $c
        if (git status --porcelain 2>$null) { $commit += '-mod' } # hay cambios sin commitear
    }
}

Write-Host ""
Write-Host "  $(Paint $lav 'killport')$(Paint $faint '  ·  ')$(Paint $sub "compilando $version+$commit")"
Write-Host ""

if ($Test) {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    # La salida completa solo se muestra si algo falla; si todo pasa, un resumen.
    $vet = go vet ./... 2>&1
    if ($LASTEXITCODE -ne 0) { $vet | Write-Host; Write-Host "  $(Paint $rose '✗') go vet encontró problemas"; exit 1 }
    $tests = go test ./... 2>&1
    if ($LASTEXITCODE -ne 0) { $tests | Write-Host; Write-Host "  $(Paint $rose '✗') hay tests que fallan"; exit 1 }
    $pkgs = @($tests | Select-String '^ok\s').Count
    Write-Host "  $(Paint $sage '✓') vet + tests  $(Paint $sub "$pkgs paquetes con tests, todos en verde · $($sw.Elapsed.TotalSeconds.ToString('N1', $ar)) s")"
    Write-Host ""
}

New-Item -ItemType Directory -Force $Out | Out-Null
$env:CGO_ENABLED = '0'
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'
$ldflags = "-s -w -X $module/internal/version.Version=$version -X $module/internal/version.Commit=$commit"

$exe = Join-Path $Out 'killport.exe'
$sw = [Diagnostics.Stopwatch]::StartNew()
go build -trimpath -ldflags $ldflags -o $exe .
if ($LASTEXITCODE -ne 0) { Write-Host "  $(Paint $rose '✗') no compila killport"; exit 1 }
$size = (Get-Item $exe).Length
$mb = ($size / 1MB).ToString('N1', $ar)
$secs = $sw.Elapsed.TotalSeconds.ToString('N1', $ar)
$sha = (Get-FileHash -Algorithm SHA256 $exe).Hash.ToLowerInvariant()
Write-Host ("  {0} {1} {2}  {3}" -f (Paint $sage '✓'), 'killport.exe', (Paint $sub "$mb MB".PadLeft(8)), (Paint $faint "$secs s"))
Write-Host "  $(Paint $faint 'sha256') $(Paint $sub $sha)"
Write-Host ""
Write-Host "  $(Paint $sub "listo en $((Resolve-Path $Out).Path)")"
Write-Host ""
