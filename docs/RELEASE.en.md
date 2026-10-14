<p align="right"><sub><b>English</b> · <a href="RELEASE.md">Español</a></sub></p>

# How a release is put together

A killport release is a `vX.Y.Z` tag and, on GitHub, three things: `killport.exe`, `SHA256SUMS` and the notes (`docs/release-vX.Y.Z.md`). Nothing else: there's no installer and no packages, the `.exe` is the whole program.

The `.exe` describes itself, and that's what gets reviewed before publishing it:

```powershell
.\dist\killport.exe --version        # killport 1.0.0+abc1234: the version and the commit, without "-mod"
go version -m .\dist\killport.exe    # the Go that built it, "mod … v1.0.0", vcs.revision and vcs.modified=false
```

## 1. Before tagging

- `internal/version/version.go` says the version being published (semver).
- `docs/release-vX.Y.Z.md` is written: what it brings, what changed, what's missing. The links are absolute and pinned to the tag (`https://github.com/agustinyarrus/killport/blob/vX.Y.Z/docs/…`): on a release page, a relative link leads nowhere.
- README and `docs/` up to date (the numbers of tests and oracles, the demo).
- The CI is green for the commit about to be tagged ([`.github/workflows/ci.yml`](../.github/workflows/ci.yml)).
- Go: the newest there is. The `go.mod` asks for 1.26.0 at minimum (and the CI builds with that one, to prove it's enough), but the release is built with the latest stable version, which brings the standard library's security fixes. Which one it was is written in the notes and in the `.exe` itself.

## 2. Tag, before building

In a clean clone of `main`, up to date:

```powershell
git switch main
git pull --ff-only
git status --porcelain                    # it has to come out empty
git tag -a v1.0.0 -m "killport 1.0.0"     # not pushed yet
```

The tag goes before building because Go stamps into the `.exe` the module version it deduces from the tags: with `v1.0.0` on the commit, `go version -m` says `mod … v1.0.0`; without it, a pseudo-version (`v0.0.0-<date>-<commit>`). They're different bytes, and whoever downloads the tag to reproduce the `.exe` will have it. If something goes wrong before pushing it, `git tag -d v1.0.0` and start over.

## 3. Build

```powershell
.\build.ps1 -Test          # go vet and all the tests; then, dist\killport.exe
.\dist\killport.exe --version
go version -m .\dist\killport.exe
```

`build.ps1` builds pure Go (`CGO_ENABLED=0`) for `windows/amd64`, without debug symbols (`-s -w`) and without the machine's paths (`-trimpath`), and stamps with `-ldflags -X` the version from `internal/version` and the commit (always 7 characters: git's automatic length grows with the repo and would change the bytes). At the end it shows the size and the SHA256. With uncommitted changes, the stamped commit carries `-mod` and `go version -m` says `vcs.modified=true`: that doesn't get published.

## 4. SHA256SUMS

```powershell
$h = (Get-FileHash -Algorithm SHA256 .\dist\killport.exe).Hash.ToLowerInvariant()
[IO.File]::WriteAllText("$PWD\dist\SHA256SUMS", "$h  killport.exe`n")
Get-Content .\dist\SHA256SUMS
```

It's `sha256sum`'s format (the lowercase hash, two spaces, the name, an LF line break), so whoever downloads it can check it with either tool:

```powershell
(Get-FileHash .\killport.exe -Algorithm SHA256).Hash -eq ((Get-Content .\SHA256SUMS) -split '\s+')[0]   # True
```

```bash
sha256sum -c SHA256SUMS    # Git Bash, WSL or Linux: "killport.exe: OK"
```

## 5. The .exe is reproducible

The same tag built with the same Go version gives the same `.exe`, byte for byte, on any machine and in any folder: `-trimpath` takes the paths out, `build.ps1` pins the OS, architecture, flags and the commit length, and killport has no external dependencies. It was checked on Sep 28, 2026: the same commit from two clones in different folders gave the same SHA256, and a third clone with a different build cache gave, with Go 1.26.0, the same hash as the `.exe` from a CI run simulated on the PC. The only thing that changes the bytes, besides the Go and the commit, is what Go deduces from the tags (step 2): the same commit, freshly tagged, gave another hash.

That makes it possible to check that the published `.exe` comes from the tag, without trusting whoever built it:

```powershell
git clone https://github.com/agustinyarrus/killport
cd killport
git checkout v1.0.0
$env:GOTOOLCHAIN = 'go1.27.1'    # the version the notes mention; Go downloads it on its own
.\build.ps1
Get-FileHash .\dist\killport.exe  # the same hash as SHA256SUMS
```

## 6. Publish

```powershell
git push origin v1.0.0
```

The CI also runs on the tag and, on top of everything usual, checks that the tag and `internal/version` say the same version.

The page's notes are the ones in `docs/release-v1.0.0.md` (without its title: the release already has one) plus a footer with the Go that built it, the commit and the SHA256. They're assembled in `dist\`, which doesn't go into the repo:

```powershell
$notas = (Get-Content docs\release-v1.0.0.md -Encoding utf8 | Select-Object -Skip 2) -join "`n"
$notas += "`n`n---`n`nCompilado con ``$(go env GOVERSION)`` desde ``$(git rev-parse HEAD)``.`n`n``````text`n$(Get-Content dist\SHA256SUMS -Raw)```````n"
[IO.File]::WriteAllText("$PWD\dist\NOTAS.md", $notas)

gh release create v1.0.0 dist\killport.exe dist\SHA256SUMS --verify-tag --draft --title "killport 1.0.0" --notes-file dist\NOTAS.md
```

`--verify-tag` doesn't let the release be created if the tag isn't on GitHub; `--draft` leaves it as a draft to look at before anyone sees it. Once the page has been reviewed (the notes, the links, the two files):

```powershell
gh release edit v1.0.0 --draft=false
```

## 7. Afterwards

Download what was published, the way anyone would, and check it:

```powershell
gh release download v1.0.0 --dir dist\publicada
cd dist\publicada
(Get-FileHash .\killport.exe -Algorithm SHA256).Hash -eq ((Get-Content .\SHA256SUMS) -split '\s+')[0]
.\killport.exe --version
```

And that the CI run on the tag finished green.

A published tag doesn't move. If something went wrong, the fix is another version (`v1.0.1`); if the release is still a draft, `gh release delete v1.0.0 --cleanup-tag` deletes it along with its tag and you start over.

## The CI's .exe files

Every CI run keeps as an artifact (14 days) the `killport.exe` of that commit with its `SHA256SUMS`. They're for testing a commit without building it, not for publishing: the CI builds with the minimum Go from the `go.mod`, and the release with the newest one. They also reproduce, with two precautions: Go 1.26.0 (`$env:GOTOOLCHAIN = 'go1.26.0'`) and a clone without tags, like the CI's (`git clone --no-tags`), so Go deduces the same pseudo-version.

<!-- i18n source=RELEASE.md sha256=d14009ae4bfb -->
