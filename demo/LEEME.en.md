<p align="right"><sub><b>English</b> · <a href="LEEME.md">Español</a></sub></p>

# The killport demo

`demo.gif` and `demo.webm` are a real recording, not a mockup: a 100×30 terminal runs
killport built from this repo against three test servers that are really listening. What
you see is the real output of that run.

| Scene | Command | What it shows |
|---|---|---|
| 1 | `killport --project . --list` | what this project is listening on: three servers, each with its port, where it listens, its command line, its project and branch (`tienda · feat/carrito`) and who launched it |
| 2 | `killport 3000` and `y` | who holds 3000, the question, the Ctrl+C to node, how long it took to close and the verification (`Puertos libres 1 de 1`) |
| 3 | `killport 3000 --free` | that 3000 really was freed: an exclusive bind confirms it |

## What's here

| File | What it's for |
|---|---|
| `demo.tape` | the [VHS](https://github.com/charmbracelet/vhs) script: it sets up the scene off camera, records the three scenes and tears it down |
| `preparar.ps1` | sets up the scene (or tears it down with `-Limpiar`); the tape and plan B use it |
| `servidores.ps1` | the shop's hidden "terminal": it launches the three servers and waits for them |
| `tienda/` | the toy project: `package.json`, `web.js` (3000), `api.py` (8080), `dev.js` (5173, localhost only) |
| `grabar.ps1` | plan B without VHS: the same session in your terminal, to record it with ScreenToGif or OBS |
| `demo.gif` · `demo.webm` | the recording: 13.6 s at 25 frames per second, 1078×644, 223 KB and 89 KB |
| `demo.png` | a still frame from the end of scene 2 (for those who ask for less motion) |

## Re-recording with VHS

You need Windows, PowerShell 7, Go, git, node and python, and also:

```powershell
winget install --scope user charmbracelet.vhs tsl0922.ttyd Gyan.FFmpeg
```

**Watch out for the VHS version.** 0.12.0 (winget's as of Sep 28, 2026) records the frames but
generates neither the GIF nor the WebM, without giving an error: it cancels the recording's context and passes it to
ffmpeg. It's fixed in 0.12.1:

```powershell
go install github.com/charmbracelet/vhs@v0.12.1     # needs Go 1.26.7+; Go downloads that version on its own
```

VHS draws the terminal in a windowless Chrome or Edge: it uses whichever is installed (go-rod finds it
on its own), it doesn't download Chromium. Then, from this folder:

```powershell
vhs demo.tape                                                     # demo.gif and demo.webm
ffmpeg -y -sseof -4.1 -i demo.gif -frames:v 1 demo.png            # the still frame (scene 2's card)
```

It takes a minute. Always look at the result before publishing it: individual frames with
`ffmpeg -ss 3.5 -i demo.gif -frames:v 1 cuadro.png`.

### What the scene does, and why

- **Nothing from your machine on screen.** Plain `killport` lists everything the PC is listening on (your
  stuff too), so the demo doesn't use it: everything happens in a toy folder with `--project .` and with
  ports that belong to the demo.
- **The folder is `X:\dev\tienda`, not your path.** `preparar.ps1` copies `tienda\` to
  `demo\.escena\dev\tienda` and mounts `demo\.escena` as a drive with `subst` (the first free one of X, Y,
  Z, W, V). killport sees each process's working folder exactly as it was given: `X:\dev\tienda`.
  `-Limpiar` unmounts the drive and deletes `.escena` (it's in `.gitignore`).
- **A real project and branch.** The copy is a git repo of its own (`git init -b feat/carrito`, one
  commit with a fixed author and date): killport reads the name from `package.json` and the branch from `.git\HEAD`.
- **The short chain.** The servers are launched by `servidores.ps1`, a hidden terminal that detaches
  from whoever called it (it relaunches itself and the first process exits): in the block, the chain is
  `pwsh.exe → node.exe`, without VHS, ttyd or your editor.
- **A real graceful close.** Each server has its own hidden console and handles Ctrl+C
  (`SIGINT` in node, `KeyboardInterrupt` in python): killport sends it Ctrl+C and sees it exit.
- **killport from this repo.** `preparar.ps1` builds it into `.escena\bin` without a stamped commit: the
  header says `v1.0.0`, not `v1.0.0+abc1234`.
- **No one else's port.** If 3000, 8080 or 5173 are already busy, `preparar.ps1` says so and
  exits without touching anything (killport tells you who: `killport 3000 8080 5173 --list`).
- **Cleanup by identity.** `servidores.ps1` writes down each process's PID and start time;
  `-Limpiar` closes only the ones that match on both.
- **The prompt is `>` and nothing else**, without history suggestions (they'd be the recorder's), with
  the command line's colors in killport's palette. The tape sets them off camera.

### Style

Background `#0b0b0f`, text `#cdd6f4` and the accents from `internal/tui/color.go` as the theme's ANSI colors;
Cascadia Mono 16; 1078×644 px, which with a 24 px margin gives 100×30 (measured with
`$Host.UI.RawUI.WindowSize`). Typing at 75 to 95 ms per key and 1.5 to 2 s to read each output.
25 frames per second: at 50, the browser can't capture every frame in time and the video comes out
sped up.

## Plan B, without VHS

`grabar.ps1` runs the same session in whatever terminal you have open, with human typing (80 ms ± 15)
and pauses, and answers `y` on its own (it writes the key into the console's input buffer, as if you
pressed it; with `-AMano` you answer yourself). When it ends, or if you cut it with Ctrl+C, it tears down the scene
and leaves the folder and the PATH as they were.

```powershell
wt --size 100,30 pwsh -NoProfile -File .\grabar.ps1 -Espera 3
```

With [ScreenToGif](https://www.screentogif.com) (`winget install NickeManarin.ScreenToGif`): the
recorder over that window, start, and `-Espera 3` gives you three seconds before the first scene.
To make it look like the GIF: a Windows Terminal color scheme with a `#0b0b0f` background and the
Cascadia Mono font.

## Tearing it down by hand

If a recording got cut off and something was left behind (the `X:` drive, the servers):

```powershell
.\preparar.ps1 -Limpiar
```

<!-- i18n source=LEEME.md sha256=d6bed04d1b19 -->
