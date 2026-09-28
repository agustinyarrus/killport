"""api.py: la API de la tienda de la demo, en el 8080 (solo biblioteca estándar).

Ante Ctrl+C cierra prolijo: http.server corta serve_forever y sale.
"""
import http.server
import json
import os

PORT = int(os.environ.get("PORT", "8080"))


class Api(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"productos": 3, "estado": "ok"}).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    with http.server.ThreadingHTTPServer(("0.0.0.0", PORT), Api) as srv:
        print(f"api escuchando en http://localhost:{PORT}", flush=True)
        try:
            srv.serve_forever()
        except KeyboardInterrupt:
            print("Ctrl+C: cerrando api…", flush=True)
