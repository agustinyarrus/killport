// Package reaper es el corazón de killport: decide y ejecuta el cierre de los
// procesos que ocupan los puertos pedidos, y verifica que hayan quedado libres.
// Es PURO: no llama a Windows ni importa syscall. Todo lo que toca el sistema
// pasa por la interfaz OS, que el integrador implementa con internal/win y los
// tests con un simulador de eventos discretos de reloj virtual. Así el
// algoritmo se prueba entero, con tiempos exactos y sin carreras.
//
// # Identidad
//
// Un PID no identifica un proceso: Windows los recicla. La unidad es
// ID{PID, Created}. Todo lo que el motor decide y hace se refiere a
// identidades; entre leer la tabla y abrir el handle, el PID pudo reciclarse, y
// actuar sobre el número sin confirmar la identidad sería matar a un inocente.
// Open compara la hora de creación y devuelve ErrRecycled si no coincide;
// mientras el handle vive, el PID no se puede reciclar.
//
// # Bosque de procesos
//
// A partir de la instantánea se arma un Forest. La arista hijo→padre vale solo
// si el padre existe con Created <= hijo.Created; si el "padre" nació después,
// su PID se recicló y es un impostor, así que el hijo es raíz. Sobre el bosque:
// ancestros, subárbol (BFS), y el ancestro común más cercano (LCA), incluido el
// LCA CRUZADO entre el bosque de ANTES y el de DESPUÉS, que descubre quién
// relanzó un proceso (el supervisor de un respawn). Construcción O(n);
// consultas O(profundidad).
//
// # Planificación
//
// Los sockets que ocupan los puertos pedidos se agrupan por dueño y se
// clasifican: dueño fantasma (el PID murió y un hijo heredó el socket — la
// trampa clásica de netstat mostrando un PID muerto), negativas con motivo y
// consejo (kernel/http.sys, procesos críticos, killport y su terminal, svchost
// y servicios del sistema), servicios de terceros (se detiene el servicio, no
// se mata el proceso: si no, el administrador de servicios lo reinicia) y
// procesos comunes. Con --tree las víctimas son la raíz y su subárbol, con un
// ANTICHAIN que absorbe una raíz en otra si es su descendiente, y sin los
// intocables (ni lo que cuelga de ellos): lo negado como dueño no vuelve a
// entrar arrastrado por un subárbol.
//
// # Intermediarios: dueño enmascarado → objetivo real
//
// A veces la tabla nombra a un INTERMEDIARIO (Relay) que sostiene el puerto
// en nombre de otro: com.docker.backend por un contenedor, wslrelay por un
// proceso Linux, VS Code por un puerto reenviado. Un intermediario nunca es
// víctima (negativa con Refusal.Relay). La herramienta puede desenmascararlo:
// pide Plan.Masked, mira detrás con el dominio que corresponda y contesta un
// Unmasking por puerto; Unmask reescribe el plan, puro, con Target
// StopExternal cuyo External (contenedor, proceso Linux; mañana una regla de
// portproxy o una URL de http.sys) se detiene con OS.StopExternal. El relay
// queda como Via, intocable; solo pasa a víctima si el dominio lo declaró
// colgado y seguro de cerrar (KillVia). Qué clases participan lo decide un
// solo lugar, Relay.Unmaskable: Unmask no reescribe ninguna otra negativa,
// aunque le llegue una respuesta con su identidad. Y cómo se nombra un
// externo en una oración ("el contenedor db-1") lo dice External.Label.
//
// # Lo pedido: una dirección, un selector
//
// PlanKill planifica sobre todo lo que ocupa los puertos pedidos; Focus se
// queda con lo que el usuario pidió de verdad: el que CONTESTA una dirección
// ("localhost:5173": dos procesos pueden escuchar el mismo puerto en [::1] y
// en 0.0.0.0 sin que ninguno dé error, y cada cliente llega a uno; ver
// ports.Answer), o los dueños que eligieron los selectores (huérfanos, por
// nombre, por carpeta). Lo demás no es una negativa: queda en
// Plan.Bystanders, se nombra, no se toca, y la verificación no lo confunde con
// alguien que retomó el puerto. Orphans (y OrphanOf) dice quién quedó suelto:
// raíz del bosque (su padre murió o su PID lo reusa otro), en la sesión de
// killport, y que no sea de Windows ni de la maquinaria.
//
// # Escalada
//
// Todas las víctimas en paralelo, un deadline por peldaño, sin polling:
//
//  0. abrir handles con identidad (ya no está / reciclado / acceso denegado);
//  1. detener los servicios de terceros, y (1b) los objetivos externos, todos
//     a la vez con un solo plazo: su dominio hace su propia escalera;
//  2. cierre prolijo (salvo --force): UN Ctrl+C por consola —con el conjunto
//     permitido de miembros y nunca la consola propia ni una compartida— y
//     WM_CLOSE a las ventanas;
//  3. esperar a todos juntos hasta --timeout, avisando cada salida en vivo;
//  4. a los que siguen vivos, terminación en seco (raíz antes que el subárbol);
//  5. un Ctrl+C del usuario corta y deja todo como está.
//
// El conjunto permitido del Ctrl+C sale de una sola función (ctrlCAllow), que
// usa también Plan.CtrlCOutlook: lo que la herramienta avisa ANTES de
// preguntar (a quién más le llega, o que no se va a poder mandar) es lo que
// el motor hace después.
//
// El motor informa lo que pasa como Event tipados (para la UI en vivo) y
// devuelve un Report con el destino de cada víctima y de cada puerto. No arma
// texto de interfaz salvo los motivos y consejos en español, que la UI muestra
// tal cual.
//
// # Verificación
//
// Se releen las tablas con backoff exponencial corto (10 ms → 200 ms, tope
// ~2 s) hasta que ningún puerto pedido esté ocupado, y se dictamina puerto por
// puerto: libre; fantasma (hijos que heredaron el socket, se ofrecen como nueva
// ronda); lo retomó otro proceso (respawn, con el supervisor por LCA cruzado);
// o, si nadie lo tiene, se diagnostica con los rangos excluidos y un sondeo de
// bind si lo reserva Windows (WSAEACCES), diciendo de quién es la exclusión:
// solo un bloque de WinNAT recibe las salidas de WinNAT (reservarlo,
// reiniciarlo); lo que aparta http.sys o un programa se dice por lo que es. El intermediario de un externo que
// se detuvo cuenta como muerto reciente SOLO en los puertos de ese externo: se
// espera a que los suelte, sin esperar los que nunca va a soltar. En un plan
// enfocado, los sockets de lo que no se pidió no cuentan (el puerto queda
// libre PARA LO PEDIDO, y PortResult.Bystander dice quién sigue ahí), y en un
// puerto pedido con dirección se pregunta quién la contesta AHORA: si es otro
// que ya escuchaba, la dirección no quedó libre (PortAnswered).
//
// # Esperar (--wait, --wait-free)
//
// Watch es la máquina de estados pura de la espera (Observe con la hora y la
// tabla; sin reloj propio): por puerto, el testigo que cumple y desde cuándo,
// con --stable (el mismo testigo sostenido), --new (solo sockets creados
// después de arrancar) y --any. WaitPorts es el bucle: relee la tabla liviana
// con backoff y, esperando que se libere, duerme sobre los handles de los
// dueños (una salida despierta al instante). Diagnoser.Blocked corta rápido lo
// que tapa una exclusión de Windows.
//
// # Quién es quién
//
// StandingOf dice si un proceso es de verdad el de Windows cuyo nombre lleva
// (la ruta en System32 y el padre esperado) o un posible impostor, que no se
// protege ni se oculta. IsWindowsProc decide qué esconde el listado general.
// PlanProcess planifica cerrar un proceso por identidad (el supervisor que
// relanza a un servidor, que no escucha nada).
package reaper
