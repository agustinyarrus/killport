// Package ports es el vocabulario de puertos de killport, puro y sin llamadas
// al sistema (compila y se prueba en cualquier plataforma):
//
//   - Set: conjunto de puertos como intervalos canónicos (ordenados, disjuntos,
//     no adyacentes). Contains en O(log k) por bisección; unión, intersección y
//     diferencia en O(k+m) por mezcla. La forma canónica es única, así que la
//     igualdad es comparar intervalos y el texto ("3000, 5000–5003") es
//     determinista y vuelve a entrar por el parser.
//   - ParseSpec: lo que escribe el usuario, en todas sus formas: 3000,
//     8080,8081, 5000-5003 (o dado vuelta), :3000, localhost:3000,
//     127.0.0.1:3000, [::1]:8080, http://localhost:5173/x (sin puerto, el del
//     esquema). Errores en español que citan el fragmento culpable.
//     ParseRequest además recuerda a qué dirección apunta cada puerto (Dest,
//     Request): localhost, una IP, o un comodín.
//   - Answer: quién contesta a un cliente que se conecta a una dirección,
//     con las reglas de Windows medidas (la dirección exacta le gana al
//     comodín; un 0.0.0.0 nativo, a la mitad IPv4 de un socket de doble
//     pila; localhost prueba ::1 y cae a 127.0.0.1), y ReachOf: esta PC,
//     toda la red o una interfaz.
//   - Socket, State y Filter: una fila de las tablas TCP/UDP, los estados con
//     nombre MIB y etiqueta en español, y la regla de qué cuenta como "ocupar
//     el puerto": TCP en LISTEN o cualquier UDP; con --all-states, además, las
//     conexiones cuyo puerto LOCAL es el pedido, salvo los restos del PID 0
//     (TIME_WAIT y compañía), que se informan pero no tienen dueño.
//   - ParseExcluded: la tabla de rangos excluidos de netsh, leída sin
//     entender el idioma: se ancla en la línea de guiones y toma las filas
//     "entero entero [*]".
//   - ParseSS y ParseProcNet: lo que escucha dentro de una distro de WSL,
//     desde `ss -Hltnup` (con los procesos dueños) o desde las tablas crudas
//     /proc/net/tcp{,6} y udp{,6} (con el inodo, para unirlo a /proc/*/fd).
//   - ParseRequestQueues: las colas de http.sys (`netsh http show
//     servicestate view=requestq`), independiente del idioma: qué proceso
//     registró cada URL (el que está detrás de System en el 80).
//   - ProxyRule y ParseProxyRule: las reglas de `netsh interface portproxy`
//     leídas del registro, con el comando exacto que borra cada una.
//   - DynamicRange y ParseDynamic: el rango de puertos que Windows reparte
//     solo (`netsh int ipvX show dynamicport`), y si está corrido de fábrica.
//   - BindResult y FindFree: el puerto libre de verdad (--free): fuera de lo
//     tomado, de los bloques de WinNAT, de las exclusiones administradas y
//     del rango dinámico, confirmado con un sondeo de bind exclusivo, con el
//     porqué de cada tramo salteado.
package ports
