"""Tiny tool server. Pretends to send emails; counts how many times it's called."""

import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

CALL_COUNT = 0


class Handler(BaseHTTPRequestHandler):
    def do_POST(self) -> None:
        global CALL_COUNT
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        args = json.loads(body) if body else {}

        CALL_COUNT += 1
        print(f"[tool_server] call #{CALL_COUNT}: send_email to={args.get('to')!r}", file=sys.stderr, flush=True)

        resp = {"message_id": f"msg-{CALL_COUNT}", "ok": True}
        payload = json.dumps(resp).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_args: object) -> None:
        pass


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", 8000), Handler).serve_forever()
