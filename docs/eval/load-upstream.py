"""Tiny threaded HTTP server used by docs/benchmarks.md as a stand-in tool
backend. Simulates 5 ms of upstream latency per request. Bind to port
18081 to match the benchmarks doc.

Usage:
    python3 docs/eval/load-upstream.py
"""
import time
from http.server import ThreadingHTTPServer, BaseHTTPRequestHandler


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args, **kwargs):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        self.rfile.read(length)
        time.sleep(0.005)
        body = b'{"ok":true}'
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", 18081), Handler).serve_forever()
