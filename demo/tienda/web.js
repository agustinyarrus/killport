// web.js: el servidor "web" de la tienda de la demo, en el 3000.
// Ante Ctrl+C cierra prolijo: deja de aceptar conexiones y sale.
const http = require('http');

const port = Number(process.env.PORT || 3000);
const server = http.createServer((req, res) => {
  res.writeHead(200, { 'content-type': 'text/html; charset=utf-8' });
  res.end('<h1>tienda</h1><p>web en el ' + port + '</p>');
});

server.listen(port, '0.0.0.0', () => console.log('web escuchando en http://localhost:' + port));

process.on('SIGINT', () => {
  console.log('Ctrl+C: cerrando web…');
  server.close(() => process.exit(0));
  setTimeout(() => process.exit(0), 250).unref();
});
