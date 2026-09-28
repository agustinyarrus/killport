<#
.SYNOPSIS
  Plan B de la demo, sin VHS: la misma sesión en la terminal que tengas
  abierta, para grabarla con ScreenToGif, OBS o lo que uses.

.DESCRIPTION
  Arma la escena (preparar.ps1: killport de este repo, la tienda con tres
  servidores que escuchan de verdad, montada como X:\dev\tienda), y corre las
  tres escenas de demo.tape con tipeo humano y pausas para leer:

    1. killport --project . --list
    2. killport 3000        (contesta "y" solo, como si lo tipearas)
    3. killport 3000 --free

  Todo corre de verdad: lo que se "tipea" es exactamente lo que se ejecuta,
  con la salida real de killport. Al final (también si lo cortás con Ctrl+C)
  desarma la escena y te deja la terminal como estaba (carpeta y PATH).

  Para que se vea como el GIF: una ventana de 100x30 con fondo #0b0b0f y
  Cascadia Mono, por ejemplo

    wt --size 100,30 pwsh -NoProfile -File .\grabar.ps1 -Espera 3

  y en ScreenToGif (winget install NickeManarin.ScreenToGif) el grabador
  sobre esa ventana. -Espera te da unos segundos para empezar a grabar.

.PARAMETER Espera
  Segundos antes de la primera escena (para arrancar el grabador).

.PARAMETER Tipeo
  Milisegundos por tecla (80, con una variación humana de ±15).

.PARAMETER AMano
  No contesta "y" solo: la pregunta de la escena 2 la contestás vos.
#>
[CmdletBinding()]
param(
    [ValidateRange(0, 60)] [int] $Espera = 1,
    [ValidateRange(10, 500)] [int] $Tipeo = 80,
    [switch] $AMano
)
$ErrorActionPreference = 'Stop'
$demo = $PSScriptRoot

$e = [char]27
function Color([string] $rgb) { "$e[38;2;${rgb}m" }
$lav = Color '196;181;253'; $sub = Color '138;143;168'; $peach = Color '246;192;160'; $txt = Color '205;214;244'; $fin = "$e[0m"

# Teclas de verdad en el búfer de entrada de esta consola (WriteConsoleInputW):
# killport las lee como si las tipearas.
if (-not $AMano -and -not ('KillportDemo.Teclas' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
namespace KillportDemo {
    public static class Teclas {
        [StructLayout(LayoutKind.Explicit, Size = 20)]
        struct INPUT_RECORD {
            [FieldOffset(0)] public ushort EventType;
            [FieldOffset(4)] public int KeyDown;
            [FieldOffset(8)] public ushort RepeatCount;
            [FieldOffset(10)] public ushort VirtualKeyCode;
            [FieldOffset(12)] public ushort VirtualScanCode;
            [FieldOffset(14)] public char UnicodeChar;
            [FieldOffset(16)] public uint ControlKeyState;
        }
        [DllImport("kernel32.dll", SetLastError = true, CharSet = CharSet.Unicode)]
        static extern IntPtr CreateFileW(string name, uint access, uint share, IntPtr sa, uint disposition, uint flags, IntPtr template);
        [DllImport("kernel32.dll", SetLastError = true)]
        static extern bool WriteConsoleInputW(IntPtr handle, INPUT_RECORD[] records, uint count, out uint written);
        [DllImport("kernel32.dll")]
        static extern bool CloseHandle(IntPtr handle);

        // Tecla manda "apretar y soltar" una tecla a la consola de este proceso.
        public static void Tecla(char c, ushort vk) {
            IntPtr h = CreateFileW("CONIN$", 0xC0000000, 3, IntPtr.Zero, 3, 0, IntPtr.Zero);
            if (h == new IntPtr(-1)) throw new System.ComponentModel.Win32Exception();
            try {
                var r = new INPUT_RECORD[2];
                r[0].EventType = 1; r[0].KeyDown = 1; r[0].RepeatCount = 1; r[0].VirtualKeyCode = vk; r[0].UnicodeChar = c;
                r[1] = r[0]; r[1].KeyDown = 0;
                uint n;
                if (!WriteConsoleInputW(h, r, 2, out n)) throw new System.ComponentModel.Win32Exception();
            } finally { CloseHandle(h); }
        }
    }
}
'@
}

# Tipea una línea de comandos con el prompt y los colores del GIF: el
# programa en lavanda, las opciones en gris, los números en durazno.
function Tipear([string] $linea) {
    Write-Host "$lav>$fin " -NoNewline
    $primero = $true
    foreach ($tok in $linea -split '(?<= )') {
        $c = if ($primero) { $lav } elseif ($tok.Trim() -match '^-') { $sub } elseif ($tok.Trim() -match '^\d+$') { $peach } else { $txt }
        $primero = $false
        foreach ($ch in $tok.ToCharArray()) {
            Write-Host "$c$ch$fin" -NoNewline
            Start-Sleep -Milliseconds ([Math]::Max(10, $Tipeo + (Get-Random -Minimum -15 -Maximum 16)))
        }
    }
    Start-Sleep -Milliseconds 120
    Write-Host ''
}

$pathAntes = $env:Path
$contestar = $null   # el que contesta "y" (un runspace de este proceso)
Push-Location $demo
try {
    Write-Host "$sub armando la escena…$fin"
    $tienda = & (Join-Path $demo 'preparar.ps1')
    if (-not $tienda) { throw 'preparar.ps1 no dejó la escena lista' }
    $env:Path = (Join-Path $demo '.escena\bin') + ';' + $env:Path
    Set-Location $tienda
    Clear-Host
    Start-Sleep -Seconds $Espera

    # 1. Lo que escucha este proyecto: su carpeta y su rama.
    Tipear 'killport --project . --list'
    killport --project . --list
    Start-Sleep -Milliseconds 2000
    Clear-Host

    # 2. El 3000 está ocupado: la pregunta, "y", el cierre prolijo y la
    #    verificación. La respuesta llega por el búfer de la consola, un rato
    #    después de que killport pregunta.
    Tipear 'killport 3000'
    if (-not $AMano) {
        $contestar = [powershell]::Create()
        $null = $contestar.AddScript({
                Start-Sleep -Milliseconds 1700
                [KillportDemo.Teclas]::Tecla([char]'y', 0x59)
                Start-Sleep -Milliseconds 200
                [KillportDemo.Teclas]::Tecla([char]13, 0x0D)
            })
        $pendiente = $contestar.BeginInvoke()
    }
    killport 3000
    if ($contestar) { $contestar.EndInvoke($pendiente); $contestar.Dispose(); $contestar = $null }
    Start-Sleep -Milliseconds 2000
    Clear-Host

    # 3. Libre de verdad.
    Tipear 'killport 3000 --free'
    killport 3000 --free
    Start-Sleep -Milliseconds 2000
}
finally {
    # Cortado a mitad de la escena 2: que no quede una tecla en camino.
    if ($contestar) { $contestar.Stop(); $contestar.Dispose() }
    Set-Location $demo
    & (Join-Path $demo 'preparar.ps1') -Limpiar *> $null
    Pop-Location
    $env:Path = $pathAntes
}
