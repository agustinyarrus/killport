# killport: prueba manual con administrador

Las pruebas de Go (`go test ./...`) corren sin administrador y no cambian nada de la
máquina: no detienen servicios, no tocan la configuración de red y no pasan por el UAC (el clic
del diálogo no se puede automatizar). Todo lo que pide administrador está probado con dobles y
con el plan que imprimiría; **la mitad que falta es esta, y necesita una persona**.

Son dos partes. La A corre killport *como administrador* (hace las cosas él mismo). La B corre
killport *sin administrador* y pasa por el UAC (el trabajo viaja al proceso elevado). Las dos
usan los mismos fixtures, todos propios y con nombres de prueba:

| fixture | qué es | puerto |
| --- | --- | --- |
| regla de portproxy `127.0.0.1:47999 → 127.0.0.1:9` | lo que deja un `netsh interface portproxy add` olvidado | 47999 |
| servicio `KillportPrueba` | un servicio de terceros de inicio automático (el doble `testdata/e2e/serviciofalso`) | 47997 |
| reserva del `47998` | una exclusión administrada (lo que deja `--reserve`) | 47998 |

Nada de esto toca un servicio, una regla ni una reserva que no sea de la prueba. Si algo queda
puesto (se cortó la luz a mitad de camino): `pwsh _oracle\admin.ps1 -Limpiar`, en una consola de
administrador.

> **Estado:** pendiente manual, enlazado desde `docs\VERIFICACION.md` (sección "Pendiente manual"). Este
> procedimiento y el guion se escribieron en una sesión SIN administrador. Lo que
> se pudo comprobar ahí: el guion no tiene errores de sintaxis, sin administrador se saltea solo,
> sus lectores de `netsh` y `sc` leen bien esta máquina, el servicio de prueba anda lanzado a
> mano, cada texto que el guion espera sale del código de killport (y la mayoría lo fija además
> un test con dobles), y sus comparaciones leen bien la salida de killport en una consola recién
> abierta. **Bajo el administrador de servicios y con el UAC no corrió nunca**: la primera corrida
> es también la prueba del guion. Si un paso falla, mirar primero si falló killport o el guion.
>
> Una trampa ya resuelta en el guion, por si alguien lo toca: pwsh decodifica lo que captura de un
> programa con la página de códigos de la consola, que en una consola nueva es la OEM (850 en un
> Windows en castellano). killport escribe UTF-8 en un pipe, así que sin fijar
> `[Console]::OutputEncoding` en UTF-8 toda comparación con acentos ("inicio automático",
> "quedó borrada") daba FALLA con killport andando bien. El guion la fija mientras corre y deja
> la de antes al salir. Antes de tocar nada, también comprueba que los puertos de prueba no caigan
> en una exclusión (los bloques de WinNAT cambian en cada arranque).

## Parte A — como administrador (guiada)

En una consola **de administrador** (Windows Terminal › clic derecho › Ejecutar como
administrador), desde la raíz del repo:

```
pwsh internal\killport\_oracle\admin.ps1
```

El guion compila killport, arma cada fixture, corre killport, compara con un oráculo
independiente (`netsh`, `sc.exe`, el registro) y limpia al final. Escribe `ok` o `FALLA` por
cada comprobación y termina con `ADMIN: TODO OK`.

Las acciones de red preguntan siempre y exigen una consola, así que en el paso 3 killport corre
pegado a la consola y **contestás vos** lo que el guion indica (`s` o `n`).

| paso | qué se prueba | qué tiene que pasar |
| --- | --- | --- |
| 1 | regla de portproxy | el objetivo es **la regla** (no el svchost); `-y` la borra con `netsh … delete`; el IP Helper sigue corriendo y es el mismo proceso; las demás reglas de la máquina quedan igual |
| 2 | servicio de terceros | el bloque dice `inicio automático: vuelve al reiniciar Windows`; `-y` lo detiene y avisa que va a volver; `-y --manual` lo detiene **y** lo deja en `DEMAND_START`, y dice `se deshace con sc config KillportPrueba start= auto`; la segunda vez dice `ya estaba en inicio manual` |
| 3 | `--reserve` / `--unreserve` | sin consola no hace nada ni con `--yes` (código 3); con tu `s`, `netsh` muestra `47998 47998 *` en IPv4 y en IPv6; WinNAT no se tocó; `killport 47998` dice **libre** (exclusión administrada); un servidor lo puede tomar; `--unreserve` la quita; con `n` no cambia nada (código 130) |
| 5 | `--fix-dynamic` | con el rango de fábrica: `No hay nada que hacer` |

Dos pasos opcionales, que tocan algo de toda la máquina (el guion avisa y deja todo como estaba):

- `-ConWinnat` (paso 4): reserva un puerto **tapado por un bloque de WinNAT**. El plan tiene que
  ser `detener WinNAT → reservar (IPv4) → reservar (IPv6) → volver a arrancar WinNAT`. Corta un
  instante la red de WSL, Docker y Hyper-V. Comprobar a ojo que la red de WSL/Docker vuelve. Entre
  las comprobaciones del final tiene que estar `WinNAT corriendo` (si no hubiera vuelto: `No quedó:
  WinNAT corriendo` con el `net start winnat` para arrancarlo, y un código distinto de 0).
- `-ConDinamico`: corre el rango dinámico de TCP IPv4 a 1025–5000 y lo arregla con
  `killport --fix-dynamic --tcp`. Solo si el rango de la máquina es el de fábrica.

## Parte B — sin administrador, por el UAC

1. En la consola **de administrador**: `pwsh internal\killport\_oracle\admin.ps1 -Preparar`.
   Deja puestos la regla y el servicio, y dice dónde quedó el `killport.exe` compilado. Se puede
   volver a correr entre una prueba y otra: repone lo que la prueba anterior haya sacado.
2. En una consola **común** (sin administrador), con ese `killport.exe`:

### B1. Borrar la regla de portproxy

```
killport 47999
```

- El bloque: `regla de portproxy 127.0.0.1:47999 → 127.0.0.1:9`, y `Windows lo ve como svchost.exe
  (PID …): es el reenvío de puertos de Windows (portproxy), no se toca`.
- La pregunta avisa **antes**: `¿Borrar la regla de portproxy … y liberar el 47999? (va a pedir
  permiso de administrador) [s/N]`. Contestar `s`.
- Sale `Borrar la regla de portproxy … pide administrador` (no un "No pude…") y el diálogo del UAC.

**Aceptar el UAC.** Tiene que salir `Haciéndolo como administrador…`, y al final
`Puertos libres 1 de 1` y `Reglas borradas 1`; código 0. Comprobar:
`netsh interface portproxy show all` ya no muestra la regla.

Volver a poner la regla (`admin.ps1 -Preparar`, en la consola de administrador: repone lo que
falte) y repetir **rechazando el UAC**: `Cancelaste el pedido de administrador`, el puerto
`sigue ocupado`, código 3, y la regla sigue en `netsh interface portproxy show all`.

### B2. Detener un servicio y que no vuelva

```
killport 47997 --manual
```

- El bloque dice `servicio KillportPrueba · inicio automático: vuelve al reiniciar Windows` y
  `--manual: después de detenerlo queda en inicio manual (se deshace con sc config KillportPrueba start= auto)`.
- La pregunta: `¿Detener el servicio KillportPrueba, dejarlo en inicio manual y liberar el 47997?
  (va a pedir permiso de administrador)`. Contestar `s` y aceptar el UAC.
- Al final: `El servicio KillportPrueba quedó en inicio manual` con el comando para deshacerlo,
  `Servicios detenidos 1`, `En inicio manual 1`, código 0.
- Oráculo: `sc query KillportPrueba` → `STOPPED`; `sc qc KillportPrueba` → `DEMAND_START`.

### B3. Reservar un puerto

```
killport 47998 --reserve
```

- Muestra el plan (dos pasos de `netsh … add excludedportrange`, sin WinNAT) y pregunta
  `¿Hacerlo? (va a pedir permiso de administrador)`. Contestar `s` y aceptar el UAC.
- Al final, una línea por paso, las comprobaciones (`47998 TCP IPv4 reservado`, `… se deja atar`)
  y la tarjeta `Pasos hechos 2 de 2 · Verificado 4 de 4`; código 0.
- Oráculo: `netsh interface ipv4 show excludedportrange protocol=tcp` muestra `47998 47998 *`.
- Deshacer: `killport 47998 --unreserve` (otra vez pregunta y pasa por el UAC).

### B4. Un proceso de otro usuario (lo heredado)

Con un servidor cualquiera escuchando desde una consola de administrador (por ejemplo
`python -m http.server 47996`), en la consola común: `killport 47996`. Cerrarlo da `acceso
denegado`; killport pregunta `¿Reintentar como administrador?`; con `s` y el UAC aceptado, el
proceso termina y el puerto queda libre.

### B5. Eleva OTRO usuario (opcional)

Desde la sesión de un usuario **sin** permisos de administrador, el UAC pide las credenciales de
un administrador y el proceso elevado corre como ese otro usuario. Ahí el proceso elevado **no
deja su informe** (solo escribe en sus propias carpetas de trabajo): tiene que salir
`El proceso elevado no dejó su informe` y, debajo, lo que killport verifica por su cuenta (la
regla ya no está, el servicio quedó en manual, el puerto quedó libre). El trabajo se hace igual.

3. Al terminar, en la consola de administrador: `pwsh internal\killport\_oracle\admin.ps1 -Limpiar`.

## Lo que NO se prueba a mano, y por qué

- **La marca de proceso crítico** (la última red de la regla de impostores). Probarla de verdad
  es marcar crítico un proceso de prueba y pedirle a killport que lo cierre: si la red fallara, la
  máquina se cuelga con una pantalla azul. Está cubierta con la tabla de `abierto.negativa()` y
  con la lectura real de la marca sobre procesos comunes (`TestHandleCritical`).
- **Detener o deshabilitar el controlador HTTP, o detener el IP Helper.** killport no lo hace ni
  lo aconseja nunca; los tests comprueban que esos comandos no aparecen en ninguna salida.

## Arreglos de la etapa de revisión que conviene mirar acá

Estos cambios se probaron con dobles y sin administrador; como administrador cambian cosas que
vale la pena ver una vez (no los mira el guion):

- **Sin leer memoria de procesos de Windows.** Como administrador, `killport --system --json`
  lista lsass, wininit y services. Para esos (y todo lo que es una imagen de Windows, o crítico)
  killport ya no pide `PROCESS_VM_READ` ni lee su memoria: la carpeta de trabajo solo se lee de lo
  que puede ser del usuario. Con Sysmon (evento 10) o Process Monitor: ningún acceso `0x1010` a
  lsass; como mucho `0x1000` (QUERY_LIMITED).
- **Una reserva que quedó a medias** dice cómo deshacer lo que sí se hizo (`lo que sí quedó hecho
  se deshace con: netsh interface ipv4 delete excludedportrange …`). Hace falta que un `netsh`
  falle entre un paso y otro, lo que a mano es difícil de provocar: lo fija `TestConfigHacer` con
  dobles. Si pasa de verdad, anotar la salida.
- **Ctrl+C mientras se detiene un servicio** (paso 2 o B2, si el servicio tarda): tiene que decir
  que cancelaste mientras se detenía (el pedido ya se mandó), no `context canceled`.

## Qué anotar

La versión de Windows (`winver`), si el UAC era del mismo usuario o de otro, la salida completa
del guion de la parte A, y de la parte B la salida de cada comando con su código (`echo
%ERRORLEVEL%` o `$LASTEXITCODE`).
