<p align="right"><sub><b>English</b> · <a href="VERIFICACION.md">Español</a></sub></p>

# Verification

The rule: each layer of killport is checked against something **independent**. A parser that reads what its own writer wrote proves nothing; netstat, `Win32_Process`, the docker CLI or `ss` inside the distro do. The [README](../README.en.md#how-it-was-verified) has the summary; here are the commands, the numbers and what was learned. The architecture being verified is in [ARQUITECTURA.en.md](ARQUITECTURA.en.md).

## Go tests

```powershell
go test ./...
.\build.ps1 -Test     # go vet + go test, and then builds
```

613 tests: `killport` 253, `reaper` 166, `win` 96, `ports` 49, `tui` 25, `cli` 12, `win/pipeio` 9, `textdist` 2 and `win/desk` 1. They don't ask for administrator, don't change the machine's configuration and don't need Docker or WSL (whatever depends on them skips itself, saying why). The end-to-end tests build the real `killport.exe` (with the `killport_e2e` tag, which only allows changing which `wsl.exe` runs) and the test servers in `internal/killport/testdata/e2e`.

## In the CI

[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) runs on `windows-latest` with the minimum Go from the `go.mod` (1.26.0): `gofmt`, `go mod tidy -diff`, `go vet` (also with `GOOS=linux`: the pure parts build outside Windows), all the tests with `-count=1`, that the tests leave no files in the tree and `build.ps1`, with the version and the commit checked in the `.exe` and its SHA256. The tests run with [`.github/pruebas.ps1`](../.github/pruebas.ps1): the usual output and, at the end, the list of skipped ones with their reason, also in the run's summary. It can be run the same way on any PC (`pwsh .github\pruebas.ps1`).

The runner isn't the development PC, and the tests know it:

- **It runs as administrator.** The ones that test the path without administrator are skipped (the elevated child that configures the network, stopping a service without permission); none of the others changes the machine.
- **It has WSL with no distros.** `wsl -l --running -q` exits 0 with no text (WSL's code says so, `WslClient.cpp`): the test against the real `wsl.exe` sees "none running". The rest of WSL is tested with doubles and with Git for Windows' `dash`, which the runner also has.
- **It has a Docker engine of its own** on `\\.\pipe\docker_engine`, with no containers. killport queries it along with each test's fake engine; it was checked on the PC with an empty engine of our own on that pipe: every Docker and end-to-end test green, and the engine received its queries.
- **http.sys:** if it doesn't let the test URL be registered, the tests about what's behind System are skipped stating the code (checked by triggering the rejection: the same URL registered twice gives 183).

And the other way around, what happens to a development PC and not to the runner: the probe's `/proc` path lists the fds of every process, and in MSYS that's asked of each Git Bash process, with a one-second wait for each one that doesn't answer. With many consoles open the list can go past the 30 s cap; `TestShSondeoRespaldoProc` is then skipped saying so (it was reproduced with 36 suspended MSYS processes), and that path stays covered by the recorded outputs.

### Timing thresholds

A test that measures time separates what's correct from what it wants to rule out, and the threshold goes far from both. What's ruled out is almost always "it waited for the whole deadline" or "it didn't notice": the bound is half the deadline, or a couple of seconds against something that without the fix would never end; not a few hundred milliseconds from the good case, which a loaded machine eats up in a couple of scheduler quanta. The fine-grained stuff (noticing at most 200 ms later, waking instantly with the owner's exit) is tested in `reaper` with a fake clock, where it doesn't depend on the machine.

Two more details. The fake systems tell their story from the first read, not from when they're built: told from the build, a slow machine started the wait with the socket already there (that's how `TestEsperaJSON` became flaky; with a simulated 300 ms delay, the old helper fails the same way as that time and the new one passes). And a cost is measured in batches: Go's monotonic clock on Windows advances in system ticks (between 0.5 and 15.6 ms), so a single half-millisecond reading measures 0 or one tick.

It was checked by pinning `go test` to 4 cores and keeping them busy with twice as many threads: all green, with the timing tests repeated up to 40 times. With Go 1.26.0 and empty caches, as in the CI, too.

## Against the machine

```powershell
pwsh internal\win\_oracle\all.ps1                   # win layer against netstat, WMI and a real Ctrl+C
pwsh internal\killport\_oracle\docker.ps1           # against real Docker Desktop
pwsh internal\killport\_oracle\wsl.ps1 -Distro Ubuntu
```

A program that closes processes can't take anything for granted because Windows' documentation says so: the offsets, the error codes and the timings were measured on the machine, and each layer is checked against another source. Everything runs without administrator and without changing the machine's configuration. The fixtures of killport's tests and scripts are our own, without a window, and are closed by identity (PID + creation time) in a `finally`; the test containers are named `kp-prueba-*` and deleted at the end; `wsl.ps1` starts the distro for its fixtures (killport never does) and lets WSL stop it again. `all.ps1`, the win layer's, is older and rougher: its Ctrl+C case opens windows (a node in a new console, a notepad) and checks that stopping W32Time without administrator is refused without touching it; it's best run with nothing else open.

The tests also run clean with `go test -race` (it needs a gcc for CGO). Docker is tested there with a fake engine on a named pipe of our own; whatever depends on the machine (WSL, Git's `dash`, there being excluded ranges, 445 listening) skips itself, saying why, if it isn't there.

## Oracles

Results from Sep 28, 2026, on the development PC (Windows 11, Docker Desktop 4.91 with engine 29.8.0, WSL 2.7 with Ubuntu 26.04):

| What | Independent oracle | Result |
|---|---|---|
| socket tables | `netstat -ano` (the same API, another program) | what's listening on TCP, 42 of 42 identical; UDP, 109 of 109 |
| processes (`NtQuerySystemInformation`) | `Win32_Process` | 417 of 417 in PID, parent, session and creation time; name, 416 plus the "System Idle Process" |
| each process's path | `QueryFullProcessImageNameW` (another API, another format) | 0 differences in all the ones both can read (226 in the first measurement) |
| services | `Win32_Service`, `sc query`, `sc qc` | 147 of 147 in PID and state; whether it can be stopped or not, startup type |
| command line | `Win32_Process.CommandLine` and `CommandLineToArgvW` | identical; split into arguments the same as Windows on 30,000 random lines |
| working folder (PEB) | the folder the process was launched with, and a `cd` in a 32-bit cmd | the same, with ñ and spaces; on WOW64 it follows the `cd` |
| interfaces | `Get-NetIPAddress` | each address with its interface |
| excluded ranges and bind probe | `netsh … show excludedportrange` plus a real bind to each range | WinNAT's blocks give `WSAEACCES` and the administered exclusion lets you bind |
| whose each exclusion is | the three classes triggered at once, without administrator: an `HttpListener` on three ports, a program's reservation (`SIO_ACQUIRE_PORT_RESERVATION`) and WinNAT's blocks; and SSDP serving 2869 | http.sys: a single-port row for each port it serves, on IPv4 and on IPv6, which goes away with it; the program: a row of its size that dies with it; WinNAT: 12 blocks of exactly 100, unaligned. The tables were recorded in `internal/ports/testdata` and the end-to-end tests repeat it with a real reservation and a real URL |
| graceful close | the marker the test server writes when it receives SIGINT | it appears when it closed through Ctrl+C; it doesn't appear with `--force` or with the shared console |
| Docker | the `docker` CLI (`ps --filter publish`, `inspect`) and the socket table | 49 of 49 |
| WSL | `ss`, `pgrep` and `wsl -l --running` inside the distro | 31 of 31 |
| the scripts that run inside Linux | Git for Windows' `dash` (Ubuntu's `/bin/sh`) over MSYS's `/proc` | tests `wsl_sh_windows_test.go` |

`Get-NetTCPConnection` and `Get-NetUDPEndpoint` turned out to be poor oracles: they always miss four listening sockets (445, 7680, 8000 and 50052, from System and from services), round the creation time to the second and merge into one the UDP sockets several processes share (mDNS). They were kept for what they do well: measuring how long a supervisor takes to take a port back (241 ms, hence the verification's 350 ms window).

## The simulator and the properties

The core (`reaper`) is tested against a discrete-event simulator: a virtual clock that only moves forward to the next event, scripted processes (how long they take to give in to Ctrl+C, whether they ignore it, whether their supervisor relaunches them, whether they inherited a socket), shared consoles, services, containers and Linux processes. No threads and no real clock: each scenario checks the sequence of events, the fate of each victim and each port, and the exact time. A Ctrl+C that closes at 420 ms leaves the clock at +420 ms; an ignored one is terminated exactly when the deadline expires; a cancellation at +1 s stops there and doesn't terminate anyone by force.

On top of that, properties with a fixed seed against independent implementations:

- the port set against a map of 65,536 booleans, with the edges 0 and 65,535 (where `uint16 + 1` wraps around);
- the forest against a naive implementation on 500 random forests with recycled PIDs: ancestors, subtree and the common ancestor of every pair;
- the plan, on 400 forests that always bring the kernel, critical processes, killport and its terminal: they're never victims, and with `--tree` nobody shows up twice; and what gets unmasked behind an intermediary is classified with the same yardstick;
- the escalation on 300 random scenarios; and the Ctrl+C warning before asking against what the engine does afterwards, on 600 random process families;
- the free port against a port-by-port oracle (400 seeds), and the wait against another one that doesn't use the state machine (300 histories);
- the elevated process's job: 82 texts that have to be rejected (and 8 more sent to the real `killport.exe`, which exits with `2` without touching anything) and 60,000 random mutations of the text; the report's path, 42 paths that have to be rejected and 40,000 mutations;
- the listing's table on 20,000 random layouts;
- the http.sys parsers (the queues and the URL reservations) on 300 outputs each, with the labels in an invented language, different every time: they read the same, because they don't read words;
- the `--reserve` planner on 3000 random states with the four classes of exclusion: WinNAT is stopped only if one of its blocks covers what was asked, and whatever is covered by an http.sys or a program exclusion is an obstacle.

## Mutations

Each stage ended with a script that changes a line of the code on purpose (inverting a condition, skipping a step, confusing v4 with v6), runs the tests, and restores the file checking the hash. There were 290 mutations: all caught. Six survived their first run and each one uncovered a weak test that was strengthened; the most interesting one, "on WOW64 the 64-bit PEB rules", lived until a test did a `cd` in a 32-bit cmd after starting it. Five were discarded with their reason: equivalent (on Windows `IPV6_V6ONLY` is already 1 out of the box) or impossible to trigger without injecting failures into advapi32. On top of that, each of the 67 fixes from the review and the final regression came with a test that fails on the tree from before.

## Final regression

With that day's `killport.exe` from `dist` (version 1.1.0 of the suite killport came out of), each stage's end-to-end scripts against the real machine. The code of this 1.0.0 is that one except for the internal names and six later changes, each with its test: four of presentation (the "usuario:" line only when the owner is another account; the supervisor closed in the second round counted only once, the last lesson; the listing's footer and long errors wrapped by words at the console's width) and two of diagnosis (http.sys URLs bound to an IP and whose each port exclusion is, the first two lessons). The stages' scripts live in the working folder, outside the repo, because they depend on this PC (fixed paths, the user's processes that have to stay alive); Docker's and WSL's are in the repo.

| Script | What it covers | Result |
|---|---|---|
| basic regression | listing, graceful close with Ctrl+C (the marker appears), `--force` (it doesn't appear), the kernel's 445 | 17 of 17 |
| http.sys | our own HttpListener behind System: who registered the URL, closing it, several on the same port | 29 of 29 |
| impostors | an `svchost.exe` and an `lsass.exe` outside System32: flagged, not protected, not hidden | 31 of 31 |
| localhost, project, orphans | the port shared on `[::1]` and `0.0.0.0` with the oracle of real connections, `--orphans`, `--project`, `-n`, parent and child on the same console | 45 of 45 |
| free and waiting | `--free` with the oracle of binding on all four addresses, `--wait`, `--wait-free`, `--new`, WinNAT's fast failure, the question in a real console (ConPTY) and one case of each function | 68 of 68 |
| `_oracle\docker.ps1` | real containers: names, stops (SIGTERM and SIGKILL), `restart=always`, `--no-docker`, wslrelay's `[::1]` | 49 of 49 |
| `_oracle\wsl.ps1 -Distro Ubuntu` | real Linux processes: names, SIGINT, escalates to SIGKILL, relay with nobody behind, doesn't start distros | 31 of 31 |

270 checks, 0 failures; no fixture alive at the end, the user's processes untouched, Docker and WSL as they were. The first run gave 7 failures in the localhost script, all of them the script's: it expected texts and exit codes that the review had changed on purpose. Looking at those outputs a real defect showed up, which no check asked for (below, in the lessons: the warning before asking); it was fixed with its test and everything was run again.

## The README's outputs

The ones in the [walkthrough](../README.en.md#walkthrough) and the [demo](../demo/LEEME.en.md) are real runs, not made-up text. On Sep 28, 2026, with killport 1.0.0 built from this repo:

- **The scene**: our own servers (node, python, a .NET `HttpListener` in `pwsh`, a nodemon-style supervisor that relaunches its child and a server that ignores Ctrl+C) in two toy projects with their git repo (`tienda` on the `feat/carrito` branch, `landing` on `main`), mounted with `subst` as `X:\dev\…`. Each server runs in its own hidden console, launched by a hidden terminal that detached from its parent: the chain killport shows is `pwsh.exe → node.exe`, with no trace of who launched them. Everything was closed at the end by PID + start time.
- **The console**: each command ran inside a 100-column pseudoconsole (ConPTY), without a window, with a harness that rebuilds the screen and answers the questions (`[s/N]`) when they appear. For killport it's a real console: colors, live region, questions. The README's blocks are that screen, copied by a script; the only things removed are the repeated header and, in the general listing's table, the rows of other programs on the machine (marked `⋮`). That table's footer was captured when killport didn't wrap it yet (the console cut it at `--` / `all-states`); after the fix those two lines were replaced with the ones killport writes today for the same numbers, which `TestPieDelListadoSeParte` pins at 100 columns, and the wrapped footer was checked with a real run in the same pseudoconsole.
- **WSL**: a `python3 -m http.server` inside Ubuntu (WSL 2), closed by killport with SIGINT; the distro stopped again on its own.
- **Docker**: an `nginx:alpine` named `kp-prueba-web`, with the shop's compose labels (`com.docker.compose.project=tienda`, `…service=web`), `restart=unless-stopped` and two published ports (8088 and 8443), on Docker Desktop 4.91 (engine 29.8.0) started for the capture with `docker desktop start`. killport stopped it through the engine's API after answering `y`; the container was deleted and Docker Desktop was stopped again (`docker desktop stop`), as it was.
- **What wasn't captured**: an impostor (the image's real path comes out whole, even if the folder is mounted with `subst`, and it would have shown a folder from the machine).

Reading those outputs in full, as in the final regression, found one more defect (the last lesson).

## Lessons

The oracles and the tests against the machine found defects the internal tests didn't see:

- **Not every exclusion is WinNAT's.** The separation's verifier saw 2869 excluded and listened to by System at the same time, and minutes later neither of the two. Measuring: http.sys sets aside with a one-port row (on IPv4 and on IPv6, even if it listens on `127.0.0.1`) every port it serves, while it serves it; and any program, without administrator, can reserve ports (`SIO_ACQUIRE_PORT_RESERVATION`) with a row that doesn't carry `*` either. killport took everything for WinNAT and advised `--reserve`, which stops WinNAT without freeing any of that. Now the shape of the row and what's seen of http.sys say whose it is (`TestE2EReservaDeUnPrograma` fails with the diagnosis from before).
- **A URL bound to an IP has a third segment.** `HTTP://127.0.0.1:8080:127.0.0.1/`: the queue parser didn't read it, and the 8080 of an `HttpListener` on `127.0.0.1` was left with no owner in sight (`TestE2EHTTPSysAtadaAUnaIP` fails with the parser from before).

- **Services: the PID is at 0x2C.** In `ENUM_SERVICE_STATUS_PROCESSW`, `dwProcessId` is the eighth DWORD of the embedded structure: 0x2C, not 0x28, where `dwWaitHint` is (0 in a running service). With 0x28 every PID came out 0. The byte test passed (it was built with the same wrong offset); `Win32_Service` caught it.
- **`SOCKET_ERROR` arrives unsigned.** `bind` returns -1 on failure, but in the 64-bit register it arrives as 0xFFFFFFFF, without sign extension: `int(r) == -1` was never true and every failure read as success (135 and 445 came out "free"). It's compared with `r != 0`. It holds for any call that returns a negative 32-bit integer.
- **`//go:uintptrescapes`.** A buffer passed as a `uintptr` can live on the goroutine's stack; if the stack grows between the conversion and the call, Windows writes to the old stack and the result comes back empty. It was reproduced every time with 64 new goroutines calling `GetFinalPathNameByHandleW`: it fails without the directive, it passes with it.
- **`time.Time` as a key.** A process's identity is PID + time, used as a map key and compared with `==`. `time.Time` also compares the zone and the monotonic reading: the time coming from a handle (FILETIME → local time) didn't match the forest's (UTC), the wait never found the victim again and the engine spun idly until the deadline. The simulator couldn't see it (everything is UTC there). Now every identity is canonicalized when it enters.
- **The `os.File` offset race on a pipe.** `go test -race` flagged 18 races, all inside the standard library: `os.File` updates a per-file offset with different locks for reading and writing, and an HTTP client reads and writes at the same time. Harmless (the kernel ignores the offset on a pipe) but it hid any real race; hence `win/pipeio`.
- **A service's path isn't opened, it's asked for.** `QueryFullProcessImageNameW` refuses in 50 of 51 services even without asking for anything else; `NtQuerySystemInformation` with class 88 gives the path of all of them without opening any.
- **The helper sees itself.** When it attaches to the target's console, the Ctrl+C helper shows up in the member list: without excluding it, it was always detected as an intruder. And "no console to handle" is three codes (5, 6 and 87), not one.
- **netsh speaks Windows' language** and writes UTF-8 to a pipe even if the console is 850. The parsers read the shape, not the words; and a PowerShell script that compares the output has to set `[Console]::OutputEncoding` (in a new console it decodes with 850 and every accent gave a failure with killport working fine).
- **A port published by Docker is three sockets from two owners** (`com.docker.backend` on `0.0.0.0` and `[::]`, `wslrelay` on `[::1]`), and `wslrelay` releases its own 315 ms after the container: the verification waits for both, and when it advises `wsl --shutdown` it warns that it also shuts down the container engine if it runs in WSL (or if it couldn't be determined).
- **Chrome and the dynamic range.** The fixtures bind TCP on a port the system hands out, and Chrome has dozens of ephemeral UDP sockets in the same range: without `--tcp`, a tests' `killport P -y` could close the user's Chrome. Every test that closes by port number carries `--tcp` and picks a port that isn't in any table.
- **Ctrl+C from Git Bash arrives ignored**: a process launched from bash inherits Ctrl+C disabled, killport sends it, it doesn't arrive, and after 3 s it terminates it by force. That's not a killport bug: the Ctrl+C scripts are run from PowerShell, and the Go test that sends a real Ctrl+C (`TestE2EEsperaCancelada`) re-enables it before launching whoever receives it.
- **The warning before asking has to come from the same rule as the action.** With a parent on `0.0.0.0` and its child on `[::1]` on the same console, `killport localhost:P` warned "the Ctrl+C also reaches its parent"; the engine didn't send it (the parent was left out of the request) and closed the child by force without having said so. Now the warning and the engine use the same function, and a property compares them on 600 random families.
- **Two rounds, a single fate per process.** With a supervisor that shares the server's console, the first round's Ctrl+C reaches it without it being a victim; if closing it is accepted afterwards, in the second round it's a victim. When merging both reports, the card counted it twice ("En seco 1" and "También cerraron 1", with the line "le llegó el Ctrl+C", when it had actually been terminated by force). It was seen while capturing the README's output; now one reached in the first round that's a victim in the second is counted by its result (`fusionarAlcanzados`, with `TestSupervisorCerradoNoSeCuentaComoAlcanzado`, which fails with the merge from before).

## Pending by hand: the administrator test

Whatever needs administrator —deleting a portproxy rule, stopping a service and leaving it on manual start, `--reserve`, `--unreserve`, `--fix-dynamic`, the path through UAC— is tested with doubles, with the plan it would print and with the elevated process run unelevated against our own processes, but **it has never run as administrator**. The procedure and the script are ready:

- [`internal\killport\_oracle\ADMIN.md`](../internal/killport/_oracle/ADMIN.en.md): part A (guided, in an administrator console) and part B (without administrator, through UAC), with what has to happen at each step and what to write down.
- `pwsh internal\killport\_oracle\admin.ps1` (part A; `-Preparar` and `-Limpiar` for B; `-ConWinnat` and `-ConDinamico`, optional, touch the whole machine's networking and leave it as it was). Without administrator it skips itself (code 0).

The first run is also the script's test: if a step fails, first look at whether killport failed or the script did.

<!-- i18n source=VERIFICACION.md sha256=fddfe974f0b6 -->
