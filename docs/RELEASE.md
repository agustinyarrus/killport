# Cómo se arma una release

Una release de killport es una etiqueta `vX.Y.Z` y, en GitHub, tres cosas: `killport.exe`, `SHA256SUMS` y las notas (`docs/release-vX.Y.Z.md`). Nada más: no hay instalador ni paquetes, el `.exe` es el programa entero.

El `.exe` se describe solo, y eso es lo que se revisa antes de publicarlo:

```powershell
.\dist\killport.exe --version        # killport 1.0.0+abc1234: la versión y el commit, sin "-mod"
go version -m .\dist\killport.exe    # el Go que lo compiló, vcs.revision (el commit entero), vcs.modified=false
```

## 1. Antes de etiquetar

- `internal/version/version.go` dice la versión que se publica (semver).
- `docs/release-vX.Y.Z.md` está escrito: qué trae, qué cambió, qué falta. Los enlaces van absolutos y fijados a la etiqueta (`https://github.com/agustinyarrus/killport/blob/vX.Y.Z/docs/…`): en la página de una release, un enlace relativo no lleva a ningún lado.
- README y `docs/` al día (los números de tests y de oráculos, la demo).
- La CI está en verde para el commit que se va a etiquetar ([`.github/workflows/ci.yml`](../.github/workflows/ci.yml)).
- Go: el más nuevo que haya. El `go.mod` pide 1.26.0 como mínimo (y la CI compila con esa, para probar que alcanza), pero la release se compila con la última versión estable, que trae los arreglos de seguridad de la biblioteca estándar. Cuál fue queda escrito en las notas y en el propio `.exe`.

## 2. Compilar

En un clon limpio de `main`, al día:

```powershell
git switch main
git pull --ff-only
git status --porcelain     # tiene que salir vacío: con cambios, el commit estampado lleva "-mod"
.\build.ps1 -Test          # go vet y todas las pruebas; después, dist\killport.exe
```

`build.ps1` compila Go puro (`CGO_ENABLED=0`) para `windows/amd64`, sin símbolos de depuración (`-s -w`) y sin rutas de la máquina (`-trimpath`), y estampa con `-ldflags -X` la versión de `internal/version` y el commit (siempre 7 caracteres: el largo automático de git crece con el repo y cambiaría los bytes). Al final muestra el tamaño y el SHA256.

## 3. SHA256SUMS

```powershell
$h = (Get-FileHash -Algorithm SHA256 .\dist\killport.exe).Hash.ToLowerInvariant()
[IO.File]::WriteAllText("$PWD\dist\SHA256SUMS", "$h  killport.exe`n")
Get-Content .\dist\SHA256SUMS
```

Es el formato de `sha256sum` (el hash en minúsculas, dos espacios, el nombre, un salto de línea LF), así que quien lo baja lo puede comprobar con cualquiera de las dos herramientas:

```powershell
(Get-FileHash .\killport.exe -Algorithm SHA256).Hash -eq ((Get-Content .\SHA256SUMS) -split '\s+')[0]   # True
```

```bash
sha256sum -c SHA256SUMS    # Git Bash, WSL o Linux: "killport.exe: OK"
```

## 4. El .exe es reproducible

El mismo commit compilado con la misma versión de Go da el mismo `.exe`, byte a byte, en cualquier máquina y en cualquier carpeta: `-trimpath` saca las rutas, `build.ps1` fija sistema, arquitectura, flags y el largo del commit, y killport no tiene dependencias externas. Se comprobó el 28/09/2026 compilando el mismo commit desde dos clones en carpetas distintas: SHA256 idéntico.

Eso permite comprobar que el `.exe` publicado sale de la etiqueta, sin confiar en quien lo compiló:

```powershell
git clone https://github.com/agustinyarrus/killport
cd killport
git checkout v1.0.0
$env:GOTOOLCHAIN = 'go1.27.1'    # la versión que dicen las notas; Go la baja sola
.\build.ps1
Get-FileHash .\dist\killport.exe  # el mismo hash que SHA256SUMS
```

## 5. Etiquetar y publicar

```powershell
git tag -a v1.0.0 -m "killport 1.0.0"
git push origin v1.0.0
```

La CI corre también sobre la etiqueta y, además de todo lo de siempre, comprueba que la etiqueta y `internal/version` digan la misma versión.

Las notas de la página son las de `docs/release-v1.0.0.md` (sin su título: la release ya tiene uno) más un pie con el Go que compiló, el commit y el SHA256. Se arman en `dist\`, que no entra al repo:

```powershell
$notas = (Get-Content docs\release-v1.0.0.md -Encoding utf8 | Select-Object -Skip 2) -join "`n"
$notas += "`n`n---`n`nCompilado con ``$(go env GOVERSION)`` desde ``$(git rev-parse HEAD)``.`n`n``````text`n$(Get-Content dist\SHA256SUMS -Raw)```````n"
[IO.File]::WriteAllText("$PWD\dist\NOTAS.md", $notas)

gh release create v1.0.0 dist\killport.exe dist\SHA256SUMS --verify-tag --draft --title "killport 1.0.0" --notes-file dist\NOTAS.md
```

`--verify-tag` no deja crear la release si la etiqueta no está en GitHub; `--draft` la deja como borrador para mirarla antes de que la vea nadie. Revisada la página (las notas, los enlaces, los dos archivos):

```powershell
gh release edit v1.0.0 --draft=false
```

## 6. Después

Bajar lo publicado, como lo bajaría cualquiera, y comprobarlo:

```powershell
gh release download v1.0.0 --dir dist\publicada
cd dist\publicada
(Get-FileHash .\killport.exe -Algorithm SHA256).Hash -eq ((Get-Content .\SHA256SUMS) -split '\s+')[0]
.\killport.exe --version
```

Y que la corrida de la CI sobre la etiqueta haya terminado en verde.

Una etiqueta publicada no se mueve. Si algo salió mal, la corrección es otra versión (`v1.0.1`); si la release todavía es un borrador, `gh release delete v1.0.0 --cleanup-tag` la borra junto con su etiqueta y se empieza de nuevo.

## Los .exe de la CI

Cada corrida de la CI guarda como artefacto (14 días) el `killport.exe` de ese commit con su `SHA256SUMS`. Sirven para probar un commit sin compilarlo, no para publicar: la CI compila con el Go mínimo del `go.mod`, y la release con el más nuevo.
