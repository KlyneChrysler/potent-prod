# potent

**Stop AI agents from doing the same thing twice.**

A transparent Go gateway that catches duplicate tool calls — sending the same email, charging the same card, deleting the same record — even when the LLM phrases the retry differently. No client SDK. No code changes. Drop it in front of your tool server.

```
agent ──HTTP/MCP──► potent ──HTTP/MCP──► your tool server
                       │
                       └── decides: forward · replay · block
```

---

## The problem

LLMs are nondeterministic. When an agent retries a tool call, it might produce:

```json
// retry 1
{ "to": "alice@example.com", "subject": "Q3 report", "body": "Hi Alice..." }

// retry 2
{ "to": " alice@example.com ", "subject": "Q3 Report", "body": "Hi Alice..." }
```

These mean the same thing. Every system today treats them as two different requests and performs the side effect twice. Stripe-style idempotency keys fail because the LLM generated different keys.

## What potent does

For every tool call:

1. **Normalizes** the arguments per a per-tool policy (trim, lowercase, sort keys)
2. **Fingerprints** the intent — exact-match SHA-256 over canonicalized JSON
3. **Falls back to semantic similarity** — embedding-based search catches retries with edited wording
4. **Replays** the cached response on a hit, **forwards** on a miss, optionally **blocks** for human review

The same response goes back to the client. No upstream call. No duplicate side effect.

## Install

```bash
# Homebrew (coming soon)
brew install KlyneChrysler/tap/potent

# Direct download
curl -L https://github.com/KlyneChrysler/potent-prod/releases/latest/download/potent_$(uname -s)_$(uname -m).tar.gz | tar xz

# Docker
docker pull ghcr.io/klynechrysler/potent:latest
```

## Quick start

```bash
# 1. Write a policy
cat > policy.yaml <<EOF
tools:
  send_email:
    mode: strict
    ttl: 24h
    normalize:
      to: [lowercase, trim]
      subject: [trim, collapse_whitespace]
    fingerprint_fields: [to, subject, body]
    semantic_threshold: 0.9
EOF

# 2. Run potent in front of your tool server
potent -mode http -upstream http://localhost:8000 -policy policy.yaml

# 3. Send a request — the second time it dedupes
curl -X POST http://localhost:8080/ \
  -H "X-Potent-Tool: send_email" \
  -d '{"to":"alice@example.com","subject":"Q3","body":"hi"}'
# → 200, X-Potent-Status: fresh

curl -X POST http://localhost:8080/ \
  -H "X-Potent-Tool: send_email" \
  -d '{"to":" Alice@Example.com ","subject":"Q3","body":"hi"}'
# → 200, X-Potent-Status: replayed, X-Potent-Match: exact
```

## Protocols

| Mode         | What it does                                                    |
|--------------|------------------------------------------------------------------|
| `http`       | Generic HTTP reverse proxy keyed on `X-Potent-Tool` header       |
| `mcp-http`   | MCP Streamable HTTP — intercepts JSON-RPC `tools/call` frames    |
| `mcp-stdio`  | Spawns your MCP server as a child and proxies stdio              |

See [examples/](./examples) for Claude Code MCP, LangGraph, and raw OpenAI integrations.

## Policy modes

```yaml
tools:
  send_email:    { mode: strict }       # block/replay duplicates (default for side effects)
  charge_card:   { mode: strict, require_human_confirm_on_replay: true }
  search_web:    { mode: cache }        # replay cached responses (for read-only tools)
  log_event:     { mode: off }          # bypass entirely
```

## Operating

Full reference in [docs/operating.md](./docs/operating.md). Highlights:

- **Backends**: in-memory or BoltDB (persists across restarts)
- **Metrics**: Prometheus scrape endpoint on `:9090`
- **Audit**: JSONL log of every decision (compliance-ready)
- **Admin**: authenticated HTTP API (`POTENT_ADMIN_TOKEN`) for stats and cache eviction
- **Deploy**: distroless container running as non-root, k8s manifest in [`deploy/k8s/`](./deploy/k8s)

## Why Go

The pipeline must add **<5ms p99** on a cache hit. One goroutine per request, zero-alloc fingerprint hashing, embedded BoltDB and HNSW (when bge-small lands), single static binary. Python gateways pay a 50ms cold start per request before they even read your policy.

## Status

Pre-1.0. Building in public. Issues and PRs welcome.

## License

Apache 2.0
