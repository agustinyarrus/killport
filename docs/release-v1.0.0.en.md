<p align="right"><sub><b>English</b> · <a href="release-v1.0.0.md">Español</a></sub></p>

# killport 1.0.0

The first version of killport as a project of its own. It used to live in navaja, the suite of console tools for Windows; the code is the one that was tested there (plus four presentation fixes and two diagnosis fixes, each with its test), now with its own repo, its version number and its CI.

killport is for when a development server was left holding a port, or something says "the port is already in use" and nobody knows who. It tells you what's holding it —a process, a service, a container, a process inside WSL— and closes it the way you'd do it by hand: Ctrl+C in its window and, only if it doesn't give in, by force. Then it checks that the port really was freed.

## What it brings

**Closing carefully.** `killport 3000` tells you who has the port, asks and closes it: first a Ctrl+C through its console and, if it doesn't give in within `--timeout` (3 s), by force. `-y` doesn't ask, `--list` only looks, `--force` goes straight there, `--tree` adds the children. Everything refers to the process's identity (PID + creation time): a recycled PID is never touched. It doesn't close the terminal you run it from, or itself, or a critical Windows process. If the Ctrl+C is also going to reach whoever shares the console (npm, a supervisor), it says so before asking.

**Names that aren't identities.** An `svchost.exe` that doesn't live in System32, or that `services.exe` didn't launch, is flagged as a possible impostor: it isn't hidden or protected, it's treated as the ordinary program it is.

**What's behind the owner.**
- Docker Desktop or Podman: it stops the container through the engine's API, not Docker Desktop.
- WSL (`wslrelay`): it closes the Linux process listening inside —SIGINT, SIGTERM and only then SIGKILL— and never starts a distro.
- System (PID 4): it looks for which program registered the URL in http.sys (also if it bound it to an IP, like `http://127.0.0.1:8080/`) and closes that one. 445 and 139 are file sharing and aren't touched.
- IP Helper: it deletes the `portproxy` rule, not the service.
- Services (SQL Server, PostgreSQL): it stops them as services, because otherwise Windows revives them; with `--manual` they're left on manual start and don't come back after a reboot.

**It verifies, it doesn't assume.** It rereads the table until the port is free and stays that way. If another process takes it back, it names the supervisor that relaunches it (nodemon, pm2) and offers to close it; if a child inherited the socket of the one that died, it finds it.

**localhost isn't 127.0.0.1.** Two processes can listen on the same port on `[::1]` and on `0.0.0.0` without either one giving an error: the classic "the browser shows the old version". The listing flags them, says which one each destination reaches, and `killport localhost:5173` aims only at that one.

**Ports Windows reserves.** It tells a busy port from an excluded one, confirms it with an exclusive bind and says whose the exclusion is: a WinNAT block (Hyper-V, WSL, Docker), the port http.sys sets aside while a service handles it (SSDP's 2869, WSD's 5357) or a program's reservation. Only to WinNAT's does it offer WinNAT's ways out: `--reserve` reserves it for you, `--unreserve` gives it back and `--fix-dynamic` fixes a shifted dynamic range. They change the whole machine's networking: they show the plan and always ask, also with `--yes`.

**Picking without naming ports.** `--orphans` (what was left listening without its terminal), `-n node` (by name or path) and `--project .` (what runs in this folder, with its branch). They combine: each one narrows it down.

**For scripts.** Stable `--json` with the same exit code as the text: `0` done, `1` partly, `2` invalid command line, `3` it couldn't be done, `130` canceled. `--free 3000` gives the first truly free port (without a console, only the number: `$port = killport --free 5173`). `--wait 60s` waits for the port to listen by looking at Windows' table, without connecting to anyone; `--wait-free`, for it to be freed; `--any`, `--stable` and `--new` fine-tune the wait.

**Administrator, only if needed.** Whatever needs it is offered through UAC. The elevated process receives a selection, not orders: it looks at the system again and only touches what its own plan allows.

## Download

`killport.exe` is the whole program: 64-bit Windows 10 or 11 (it was tested on Windows 11), a single file, with no installer and no dependencies. Copy it to a folder on your `PATH`. Or, with Go:

```powershell
go install github.com/agustinyarrus/killport@v1.0.0
```

To check the download, in the folder where the two files ended up:

```powershell
(Get-FileHash .\killport.exe -Algorithm SHA256).Hash -eq ((Get-Content .\SHA256SUMS) -split '\s+')[0]   # True
```

In Git Bash or WSL, `sha256sum -c SHA256SUMS`. The `.exe` is reproducible: the same tag with the same Go gives the same bytes, so anyone can build it and compare ([how](https://github.com/agustinyarrus/killport/blob/v1.0.0/docs/RELEASE.md#5-el-exe-es-reproducible)).

## How it was verified

- More than 600 Go tests: the algorithm against a discrete-event simulator with a virtual clock, properties against naive implementations, and end-to-end tests that build the real `killport.exe` and run it against the machine (Docker and WSL with faithful doubles; http.sys, impostors and excluded ranges, for real).
- Each layer checked against an independent source: sockets 42 of 42 and 109 of 109 against netstat, processes 417 of 417 against `Win32_Process`, services 147 of 147, Docker 49 of 49 against its CLI, WSL 31 of 31 against `ss` and `pgrep` inside the distro.
- 290 of 290 mutations caught and 270 of 270 end-to-end checks against the real machine.
- CI configured for `windows-latest` with the minimum Go from the `go.mod`: formatting, `go vet` (for Linux too), all the tests, and the `.exe` with its version and its SHA256. This version was verified with the same sequence on a clean clone, before publishing it.

The details, in [docs/VERIFICACION.md](https://github.com/agustinyarrus/killport/blob/v1.0.0/docs/VERIFICACION.md); the architecture, in [docs/ARQUITECTURA.md](https://github.com/agustinyarrus/killport/blob/v1.0.0/docs/ARQUITECTURA.md).

## What's missing

- **The manual run as administrator.** Whatever needs administrator —deleting a `portproxy` rule, stopping a service and leaving it on manual, `--reserve`, `--unreserve`, `--fix-dynamic`, the path through UAC— is tested with doubles, with the plan it would print and with the elevated process run unelevated against our own processes, but never end to end as administrator. The script is ready: [ADMIN.md](https://github.com/agustinyarrus/killport/blob/v1.0.0/internal/killport/_oracle/ADMIN.md).
- Not tested against the real thing: Podman and Windows containers (only with recorded responses), and WSL with `networkingMode=mirrored`, which has no `wslrelay`.
- A host name (`api.local:3000`) isn't resolved: the whole port is taken and it warns about it.

The full list, in [docs/PENDIENTE.md](https://github.com/agustinyarrus/killport/blob/v1.0.0/docs/PENDIENTE.md).

<!-- i18n source=release-v1.0.0.md sha256=efce009747ac -->
