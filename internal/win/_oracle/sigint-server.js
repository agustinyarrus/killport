// Fixture: servidor que se queda escuchando y, al recibir SIGINT (el Ctrl+C
// que le manda el ayudante), escribe un archivo marcador y cierra prolijo.
// Así el end-to-end comprueba que el Ctrl+C llegó y provocó un cierre limpio,
// no una muerte a la fuerza.
const fs = require('fs');
const net = require('net');
const marker = process.argv[2];
const port = parseInt(process.argv[3] || '0', 10);

const server = net.createServer(() => {});
server.listen(port, '127.0.0.1', () => {
  fs.writeFileSync(marker + '.ready', String(process.pid));
});

process.on('SIGINT', () => {
  fs.writeFileSync(marker, 'cerrado-por-SIGINT pid=' + process.pid + ' t=' + Date.now());
  server.close(() => process.exit(0));
  // por si acaso, salir aunque el close tarde
  setTimeout(() => process.exit(0), 200);
});

// mantener vivo
setInterval(() => {}, 1 << 30);
