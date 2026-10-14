<p align="right"><sub><b>English</b> · <a href="ADMIN.md">Español</a></sub></p>

# killport: manual test with administrator

The Go tests (`go test ./...`) run without administrator and don't change anything on the
machine: they don't stop services, don't touch the network configuration and don't go through UAC (the click
on the dialog can't be automated). Everything that needs administrator is tested with doubles and
with the plan it would print; **the missing half is this one, and it needs a person**.

There are two parts. A runs killport *as administrator* (it does things itself). B runs
killport *without administrator* and goes through UAC (the job travels to the elevated process). Both
use the same fixtures, all our own and with test names:

| fixture | what it is | port |
| --- | --- | --- |
| portproxy rule `127.0.0.1:47999 → 127.0.0.1:9` | what a forgotten `netsh interface portproxy add` leaves behind | 47999 |
| service `KillportPrueba` | a third-party service with automatic startup (the `testdata/e2e/serviciofalso` double) | 47997 |
| reservation of `47998` | an administered exclusion (what `--reserve` leaves behind) | 47998 |

None of this touches a service, a rule or a reservation that isn't the test's. If something is left
in place (the power went out halfway): `pwsh _oracle\admin.ps1 -Limpiar`, in an administrator
console.

> **Status:** pending by hand, linked from `docs\VERIFICACION.md` (section "Pendiente manual"). This
> procedure and the script were written in a session WITHOUT administrator. What
> could be checked there: the script has no syntax errors, without administrator it skips itself,
> its `netsh` and `sc` readers read this machine correctly, the test service works when launched by
> hand, every text the script expects comes from killport's code (and most of them are also pinned by
> a test with doubles), and its comparisons read killport's output correctly in a freshly
> opened console. **It has never run under the service manager or with UAC**: the first run
> is also the script's test. If a step fails, first look at whether killport failed or the script did.
>
> A trap already solved in the script, in case someone touches it: pwsh decodes what it captures from a
> program with the console's code page, which in a new console is the OEM one (850 on a
> Spanish Windows). killport writes UTF-8 to a pipe, so without setting
> `[Console]::OutputEncoding` to UTF-8 every comparison with accents ("inicio automático",
> "quedó borrada") gave FALLA with killport working fine. The script sets it while it runs and restores
> the previous one on exit. Before touching anything, it also checks that the test ports don't fall
> into an exclusion (WinNAT's blocks change on every boot).

## Part A — as administrator (guided)

In an **administrator** console (Windows Terminal › right click › Run as
administrator), from the repo's root:

```
pwsh internal\killport\_oracle\admin.ps1
```

The script builds killport, sets up each fixture, runs killport, compares against an independent
oracle (`netsh`, `sc.exe`, the registry) and cleans up at the end. It writes `ok` or `FALLA` for
each check and ends with `ADMIN: TODO OK`.

Network actions always ask and require a console, so in step 3 killport runs
attached to the console and **you answer** what the script tells you (`s` or `n`).

| step | what's tested | what has to happen |
| --- | --- | --- |
| 1 | portproxy rule | the target is **the rule** (not the svchost); `-y` deletes it with `netsh … delete`; the IP Helper keeps running and is the same process; the machine's other rules stay the same |
| 2 | third-party service | the block says `inicio automático: vuelve al reiniciar Windows`; `-y` stops it and warns that it'll come back; `-y --manual` stops it **and** leaves it on `DEMAND_START`, and says `se deshace con sc config KillportPrueba start= auto`; the second time it says `ya estaba en inicio manual` |
| 3 | `--reserve` / `--unreserve` | without a console it does nothing, not even with `--yes` (code 3); with your `s`, `netsh` shows `47998 47998 *` on IPv4 and on IPv6; WinNAT wasn't touched; `killport 47998` says **free** (administered exclusion); a server can take it; `--unreserve` removes it; with `n` nothing changes (code 130) |
| 5 | `--fix-dynamic` | with the factory range: `No hay nada que hacer` |

Two optional steps, which touch something machine-wide (the script warns and leaves everything as it was):

- `-ConWinnat` (step 4): reserves a port **covered by a WinNAT block**. The plan has to
  be `detener WinNAT → reservar (IPv4) → reservar (IPv6) → volver a arrancar WinNAT`. It cuts WSL's, Docker's and Hyper-V's networking
  for a moment. Check by eye that WSL's/Docker's networking comes back. Among
  the final checks there has to be `WinNAT corriendo` (if it hadn't come back: `No quedó:
  WinNAT corriendo` with the `net start winnat` to start it, and a code other than 0).
- `-ConDinamico`: shifts the IPv4 TCP dynamic range to 1025–5000 and fixes it with
  `killport --fix-dynamic --tcp`. Only if the machine's range is the factory one.

## Part B — without administrator, through UAC

1. In the **administrator** console: `pwsh internal\killport\_oracle\admin.ps1 -Preparar`.
   It leaves the rule and the service in place, and says where the built `killport.exe` ended up. It can
   be run again between one test and the next: it restores whatever the previous test removed.
2. In an **ordinary** console (without administrator), with that `killport.exe`:

### B1. Deleting the portproxy rule

```
killport 47999
```

- The block: `regla de portproxy 127.0.0.1:47999 → 127.0.0.1:9`, and `Windows lo ve como svchost.exe
  (PID …): es el reenvío de puertos de Windows (portproxy), no se toca`.
- The question warns **beforehand**: `¿Borrar la regla de portproxy … y liberar el 47999? (va a pedir
  permiso de administrador) [s/N]`. Answer `s`.
- Out comes `Borrar la regla de portproxy … pide administrador` (not a "No pude…") and the UAC dialog.

**Accept the UAC prompt.** `Haciéndolo como administrador…` has to come out, and at the end
`Puertos libres 1 de 1` and `Reglas borradas 1`; code 0. Check:
`netsh interface portproxy show all` no longer shows the rule.

Put the rule back (`admin.ps1 -Preparar`, in the administrator console: it restores whatever is
missing) and repeat **rejecting the UAC prompt**: `Cancelaste el pedido de administrador`, the port
`sigue ocupado`, code 3, and the rule is still in `netsh interface portproxy show all`.

### B2. Stopping a service so it doesn't come back

```
killport 47997 --manual
```

- The block says `servicio KillportPrueba · inicio automático: vuelve al reiniciar Windows` and
  `--manual: después de detenerlo queda en inicio manual (se deshace con sc config KillportPrueba start= auto)`.
- The question: `¿Detener el servicio KillportPrueba, dejarlo en inicio manual y liberar el 47997?
  (va a pedir permiso de administrador)`. Answer `s` and accept the UAC prompt.
- At the end: `El servicio KillportPrueba quedó en inicio manual` with the command to undo it,
  `Servicios detenidos 1`, `En inicio manual 1`, code 0.
- Oracle: `sc query KillportPrueba` → `STOPPED`; `sc qc KillportPrueba` → `DEMAND_START`.

### B3. Reserving a port

```
killport 47998 --reserve
```

- It shows the plan (two `netsh … add excludedportrange` steps, without WinNAT) and asks
  `¿Hacerlo? (va a pedir permiso de administrador)`. Answer `s` and accept the UAC prompt.
- At the end, one line per step, the checks (`47998 TCP IPv4 reservado`, `… se deja atar`)
  and the card `Pasos hechos 2 de 2 · Verificado 4 de 4`; code 0.
- Oracle: `netsh interface ipv4 show excludedportrange protocol=tcp` shows `47998 47998 *`.
- Undo: `killport 47998 --unreserve` (it asks again and goes through UAC).

### B4. Another user's process (the inherited case)

With any server listening from an administrator console (for example
`python -m http.server 47996`), in the ordinary console: `killport 47996`. Closing it gives `acceso
denegado`; killport asks `¿Reintentar como administrador?`; with `s` and the UAC prompt accepted, the
process ends and the port is freed.

### B5. ANOTHER user elevates (optional)

From the session of a user **without** administrator permissions, UAC asks for an
administrator's credentials and the elevated process runs as that other user. There the elevated process **doesn't
leave its report** (it only writes to its own working folders): what has to come out is
`El proceso elevado no dejó su informe` and, below it, what killport verifies on its own (the
rule is gone, the service was left on manual, the port was freed). The job gets done anyway.

3. At the end, in the administrator console: `pwsh internal\killport\_oracle\admin.ps1 -Limpiar`.

## What is NOT tested by hand, and why

- **The critical process flag** (the last net of the impostor rule). Testing it for real
  means marking a test process as critical and asking killport to close it: if the net failed, the
  machine hangs with a blue screen. It's covered with `abierto.negativa()`'s table and
  with the real reading of the flag on ordinary processes (`TestHandleCritical`).
- **Stopping or disabling the HTTP driver, or stopping the IP Helper.** killport never does it nor
  advises it; the tests check that those commands don't show up in any output.

## Fixes from the review stage worth looking at here

These changes were tested with doubles and without administrator; as administrator they change things
worth seeing once (the script doesn't look at them):

- **No reading the memory of Windows processes.** As administrator, `killport --system --json`
  lists lsass, wininit and services. For those (and everything that's a Windows image, or critical)
  killport no longer asks for `PROCESS_VM_READ` or reads their memory: the working folder is only read for
  what can belong to the user. With Sysmon (event 10) or Process Monitor: no `0x1010` access to
  lsass; at most `0x1000` (QUERY_LIMITED).
- **A reservation left halfway** says how to undo what did get done (`lo que sí quedó hecho
  se deshace con: netsh interface ipv4 delete excludedportrange …`). It needs a `netsh` to
  fail between one step and the next, which is hard to trigger by hand: `TestConfigHacer` pins it with
  doubles. If it happens for real, write down the output.
- **Ctrl+C while a service is being stopped** (step 2 or B2, if the service takes a while): it has to say
  that you canceled while it was stopping (the request was already sent), not `context canceled`.

## What to write down

The Windows version (`winver`), whether the UAC prompt was the same user's or another's, the full output
of part A's script, and from part B the output of each command with its code (`echo
%ERRORLEVEL%` or `$LASTEXITCODE`).

<!-- i18n source=ADMIN.md sha256=3f37c51ac10b -->
