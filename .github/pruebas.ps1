<#
.SYNOPSIS
  Corre go test como en la CI y termina con un resumen: cuántas pasan, cuáles
  fallan y cuáles se saltearon, con su motivo.

.DESCRIPTION
  Mientras corre, la salida es la de siempre: una línea por paquete y el
  detalle completo de cada prueba que falla. Al final, el resumen. Las
  salteadas importan: cada máquina tiene lo suyo (el runner de GitHub corre
  como administrador, tiene WSL sin distros y un motor de Docker propio; una
  PC de desarrollo, al revés) y una prueba que se saltea en silencio es una
  prueba que no corrió. Con GITHUB_STEP_SUMMARY definido (en Actions), el
  resumen también va a la página de la corrida.

  El código de salida es el de go test. Siempre con -count=1: los resultados
  en caché no dicen nada de esta máquina. Necesita PowerShell 7 (pwsh): en la
  5.1, con $ErrorActionPreference = 'Stop', cualquier línea de go en stderr
  cortaría el script.

.EXAMPLE
  .\.github\pruebas.ps1                          # go test -count=1 ./...
  .\.github\pruebas.ps1 -Informe pruebas.json    # además guarda los eventos crudos
  .\.github\pruebas.ps1 ./internal/reaper/...    # solo esos paquetes
  .\.github\pruebas.ps1 -Short                   # la vuelta rápida
#>
#Requires -Version 7
[CmdletBinding(PositionalBinding = $false)]
param(
    [Parameter(ValueFromRemainingArguments)]
    [string[]] $Paquetes = @('./...'),
    [string] $Informe,
    [string] $Timeout = '20m',
    [switch] $Short # -short: sin las de punta a punta ni las que corren go list/vet
)
$ErrorActionPreference = 'Stop'
Set-Location (Split-Path $PSScriptRoot) # la raíz del repo
$ar = [Globalization.CultureInfo]::GetCultureInfo('es-AR')
# go escribe UTF-8: que los acentos de los motivos lleguen enteros al log.
try { [Console]::OutputEncoding = [Text.UTF8Encoding]::new($false) } catch { }

$modulo = (Select-String -Path go.mod -Pattern '^module\s+(\S+)').Matches[0].Groups[1].Value
function Corto([string] $paquete) {
    if ($paquete -eq $modulo) { return '.' }
    return $paquete.Substring($modulo.Length + 1)
}

$enCurso = @{}                                            # "paquete|prueba" → sus líneas de salida
$pasan = 0; $pasanRaiz = 0
$fallan = [Collections.Generic.List[string]]::new()
$salteadas = [Collections.Generic.List[object]]::new()
$crudo = if ($Informe) { [IO.StreamWriter]::new([IO.Path]::GetFullPath($Informe, (Get-Location).Path)) } else { $null }
$reloj = [Diagnostics.Stopwatch]::StartNew()

$flags = @('-count=1', "-timeout=$Timeout", '-json')
if ($Short) { $flags += '-short' }
try {
    go test @flags @Paquetes 2>&1 | ForEach-Object {
        $linea = "$_"
        if ($crudo) { $crudo.WriteLine($linea) }
        $e = $null
        if ($linea.StartsWith('{')) { try { $e = $linea | ConvertFrom-Json } catch { $e = $null } }
        if (-not $e) { Write-Host $linea; return } # lo que no es un evento (un error del propio go) sale tal cual

        switch ($e.Action) {
            'build-output' { Write-Host -NoNewline $e.Output; return }
            'output' {
                if ($e.Test) {
                    $k = "$($e.Package)|$($e.Test)"
                    if (-not $enCurso.ContainsKey($k)) { $enCurso[$k] = [Collections.Generic.List[string]]::new() }
                    $enCurso[$k].Add($e.Output)
                } elseif ($e.Output.Trim() -ne 'PASS') {
                    Write-Host -NoNewline $e.Output # "ok  paquete  12.3s", "FAIL …", un pánico
                }
                return
            }
        }
        # run/pause/cont no cierran nada: la salida de una prueba en paralelo se
        # sigue juntando hasta su pass, fail o skip.
        if (-not $e.Test -or $e.Action -notin 'pass', 'fail', 'skip') { return }
        $k = "$($e.Package)|$($e.Test)"
        $salida = $enCurso[$k]
        $enCurso.Remove($k)
        switch ($e.Action) {
            'pass' {
                $pasan++
                if ($e.Test -notmatch '/') { $pasanRaiz++ }
            }
            'fail' {
                $fallan.Add("$(Corto $e.Package) $($e.Test)")
                if ($salida) { Write-Host -NoNewline ($salida -join '') }
            }
            'skip' {
                $motivo = @($salida | Where-Object { $_ -notmatch '^\s*(=== (RUN|PAUSE|CONT|NAME)|--- SKIP)' } |
                        ForEach-Object { $_.Trim() } | Where-Object { $_ }) -join ' '
                $salteadas.Add([pscustomobject]@{ Paquete = (Corto $e.Package); Prueba = $e.Test; Motivo = $motivo })
            }
        }
    }
    $codigo = $LASTEXITCODE
} finally {
    if ($crudo) { $crudo.Dispose() }
}

$seg = $reloj.Elapsed.TotalSeconds.ToString('N0', $ar)
Write-Host ''
Write-Host "pruebas: $pasanRaiz pasan ($pasan contando subpruebas) · $($fallan.Count) fallan · $($salteadas.Count) salteadas · $seg s · go test salió con $codigo"
foreach ($f in $fallan) { Write-Host "  FALLA     $f" }
foreach ($s in $salteadas) { Write-Host "  SALTEADA  $($s.Paquete) $($s.Prueba): $($s.Motivo)" }

if ($env:GITHUB_STEP_SUMMARY) {
    $md = [Text.StringBuilder]::new()
    [void]$md.AppendLine('### Pruebas')
    [void]$md.AppendLine()
    [void]$md.AppendLine("**$pasanRaiz** pasan ($pasan contando subpruebas) · **$($fallan.Count)** fallan · **$($salteadas.Count)** salteadas · $seg s")
    if ($fallan.Count) {
        [void]$md.AppendLine()
        foreach ($f in $fallan) { [void]$md.AppendLine("- :x: ``$f``") }
    }
    if ($salteadas.Count) {
        [void]$md.AppendLine()
        [void]$md.AppendLine('| Paquete | Prueba | Por qué se salteó en esta máquina |')
        [void]$md.AppendLine('|---|---|---|')
        foreach ($s in $salteadas) {
            [void]$md.AppendLine("| ``$($s.Paquete)`` | ``$($s.Prueba)`` | $($s.Motivo -replace '\|', '\|') |")
        }
    }
    Add-Content -Path $env:GITHUB_STEP_SUMMARY -Value $md.ToString() -Encoding utf8
}
exit $codigo
