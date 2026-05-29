"""Toy MCP server: speaks line-delimited JSON-RPC 2.0 on stdio, exposes a
send_email tool, and prints a counter to stderr so you can see when calls
actually reach it (vs being deduped by potent).
"""

import json
import sys

CALLS = 0


def respond(rid, result=None, error=None):
    msg = {"jsonrpc": "2.0", "id": rid}
    if error is not None:
        msg["error"] = error
    else:
        msg["result"] = result
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()


def main():
    global CALLS
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except Exception:
            continue

        method = req.get("method", "")
        rid = req.get("id")
        params = req.get("params", {})

        if method == "initialize":
            respond(rid, {
                "protocolVersion": "2025-06-18",
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "fake-email", "version": "0.1.0"},
            })
        elif method == "tools/list":
            respond(rid, {
                "tools": [
                    {
                        "name": "send_email",
                        "description": "Send an email",
                        "inputSchema": {
                            "type": "object",
                            "properties": {
                                "to": {"type": "string"},
                                "subject": {"type": "string"},
                                "body": {"type": "string"},
                            },
                            "required": ["to", "subject", "body"],
                        },
                    }
                ]
            })
        elif method == "tools/call":
            CALLS += 1
            args = params.get("arguments", {})
            print(f"[fake_mcp_server] CALL #{CALLS}: to={args.get('to')!r}",
                  file=sys.stderr, flush=True)
            respond(rid, {
                "content": [{"type": "text", "text": f"sent (call #{CALLS}) to {args.get('to')}"}]
            })
        elif rid is not None:
            respond(rid, error={"code": -32601, "message": f"unknown method {method}"})


if __name__ == "__main__":
    main()
