// dev.js: un servidor de desarrollo al estilo Vite, en el 5173 y solo en
// localhost (como Vite por defecto). Ante Ctrl+C cierra prolijo.
const http = require('http');

const port = Number(process.env.PORT || 5173);
const server = http.createServer((req, res) => {
  res.writeHead(200, { 'content-type': 'text/plain; charset=utf-8' });
  res.end('dev server de la tienda\n');
});

server.listen(port, 'localhost', () => console.log('dev listo en http://localhost:' + port));

process.on('SIGINT', () => {
  console.log('Ctrl+C: cerrando dev…');
  server.close(() => process.exit(0));
  setTimeout(() => process.exit(0), 250).unref();
});
