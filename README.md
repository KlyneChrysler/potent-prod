# potent



**Exactly-once for AI agents.**

A transparent Go gateway that stops AI agents from accidentally doing the same action twice — sending duplicate emails, double-charging cards, deleting the same record twice — even when the LLM phrases the retry slightly differently.

## The problem

LLMs are nondeterministic. When an agent retries a tool call, it might produce:

```json
{ "to": "alice@example.com", "subject": "Q3 report", "body": "Hi Alice..." }
```

then later:

```json
{ "to": " alice@example.com ", "subject": "Q3 Report", "body": "Hi Alice..." }
```

These mean the same thing. Every system today treats them as two different requests and performs the side effect twice. Stripe-style idempotency keys fail because the LLM generated different keys.

## How potent solves it

potent sits between your agent and your tools as a transparent reverse proxy. For every tool call, it:

1. **Normalizes** arguments per a per-tool policy (trim, lowercase, sort keys)
2. **Fingerprints** the intent with BLAKE3 (exact match) + bge-small embedding (semantic match)
3. **Replays** the cached response if a duplicate is detected
4. **Logs** every decision for audit

No client cooperation needed. Drop potent in front of your MCP server or OpenAI-compatible tool endpoint and you get exactly-once semantics.

## Status

Pre-alpha. Building in public.

## Architecture

```
Agent ──HTTP/MCP──► potent ──HTTP/MCP──► tool server
                      │
                      ├── normalizer (per-tool policy)
                      ├── fingerprinter (BLAKE3 + ONNX embedding)
                      ├── BoltDB (fingerprint → cached response)
                      └── audit log (JSONL)
```

## Supported protocols

- [ ] OpenAI-compatible tool calls (Week 1-3)
- [ ] MCP Streamable HTTP (Week 4)
- [ ] MCP stdio (Week 4)
- [ ] Generic HTTP webhook with `X-Potent-Tool` header (Week 5)

## License

Apache 2.0
