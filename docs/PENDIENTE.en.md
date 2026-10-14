<p align="right"><sub><b>English</b> · <a href="PENDIENTE.md">Español</a></sub></p>

# To do

What's missing, in the order it's best done. Each point says what's there today, what's missing and how it's verified.

## 1. The administrator test

killport is finished and verified against the real machine ([VERIFICACION.en.md](VERIFICACION.en.md)), except for what needs administrator: deleting a portproxy rule, stopping a service and leaving it on manual start, `--reserve`, `--unreserve`, `--fix-dynamic` and the path through UAC. That's tested with doubles, with the plan it would print and with the elevated process run unelevated against our own processes, but never as administrator.

**Missing:** running [`internal\killport\_oracle\ADMIN.md`](../internal/killport/_oracle/ADMIN.en.md) once:

1. Part A, in an administrator console: `pwsh internal\killport\_oracle\admin.ps1` (with `-ConWinnat` and `-ConDinamico` if you accept that the machine's networking gets cut for a moment; it leaves it as it was).
2. Part B, without administrator: `admin.ps1 -Preparar` as admin, killport from an ordinary console accepting and rejecting the UAC prompt, and `admin.ps1 -Limpiar`.

The first run also tests the script: if a step fails, first look at whether killport failed or the script did.

## 2. Improvements

- Multiple selection in the question ("¿Cerrar cuáles? 1,3") for `--orphans`, `-n` and shared ports: today everything is confirmed together. In the interactive listing, picking the row of a shared port already aims at that process.
- `--project` doesn't pick a container whose compose project lives in that folder (the `working_dir` label).
- A host name (`api.local:3000`) isn't resolved: the whole port is taken and it warns about it.
- `--wait` doesn't wait for the death of a named process: if the server dies before listening, it waits until the deadline.
- The supervisor that relaunches a server is looked for in its living parent and grandparent. One further up, or one with another image that relaunches after the verification window, isn't seen; and a living parent with the same image that doesn't relaunch adds ~1.5 s before considering the port free.
- Whatever runs from `C:\Windows` is hidden from the listing except in the folders where any user writes, which are a fixed list (`Temp`, `Tasks`, `tracing`, `spool`…): looking at the real ACL would be more complete.
- Whose an exclusion is gets deduced from its shape and from what's seen of http.sys ([ARQUITECTURA](ARQUITECTURA.en.md#diagnosing-a-port-without-an-owner)). Two things can't be known that way: a program that reserved exactly 100 ports would pass for WinNAT, and for a program's reservation Windows doesn't say whose it is (the socket could be searched for among each process's handles, with administrator).
- WSL in `networkingMode=mirrored` mode has no `wslrelay`: the port shows up in use without an owner on Windows and killport says "free" or "reserved" depending on the probe.
- Without the `/proc/net` tables (it doesn't happen on a real distro), the owner of a Linux socket is taken from `ss` without confirming it by the inode.
- In the general listing, the note about a port shared by two containers says "procesos" (in `killport P`'s block it already says "servidores").
- Not tested against the real thing: Podman and Windows containers (only with recorded responses), a really hung WSL relay (bug WSL#10601 can't be triggered on purpose), and canceling `--wait` with Ctrl+C from mintty (Git Bash).
- Minor ones from the review: the `EnumWindows` callback is created on every call; `defer t.Close()` coexists with `os.Exit` in `main.go`; `--new` takes the start time after parsing the command line; nine long functions (`planificarConfig`, `Unmask`, `Verify`, `correrPuertos`, `Focus`…) that would read better split up; the `netsh` command in `--reserve`'s plan doesn't fit in 100 columns and goes on its own line.

## 3. Distribution

- The first release: everything is ready to create it ([RELEASE.en.md](RELEASE.en.md): `build.ps1`, `SHA256SUMS`, `gh release create` with the notes from [release-v1.0.0.en.md](release-v1.0.0.en.md)); publishing it is what's missing.
- The CI's first run on GitHub ([`.github/workflows/ci.yml`](../.github/workflows/ci.yml)): it's tested step by step on the PC, with Go 1.26.0, empty caches, a Docker engine on the runner's pipe and the machine under load, but the runner runs as administrator and there the tests run elevated for the first time. Look at the list of skipped ones in the summary.
- Optional: an installer (Inno Setup) or a winget/Scoop manifest.

<!-- i18n source=PENDIENTE.md sha256=c81641b05c30 -->
