# Operating potent

## Modes

```
-mode http        Generic HTTP reverse proxy keyed on X-Potent-Tool header
-mode mcp-http    MCP Streamable HTTP; intercepts JSON-RPC tools/call frames
-mode mcp-stdio   Spawn the upstream MCP server as a child and proxy stdio
```

## Flags

| Flag             | Default                | Notes                                   |
|------------------|------------------------|-----------------------------------------|
| `-mode`          | `http`                 | `http` \| `mcp-http` \| `mcp-stdio`     |
| `-addr`          | `:8080`                | HTTP listen address                     |
| `-metrics-addr`  | `:9090`                | Prometheus scrape endpoint              |
| `-admin-addr`    | (disabled)             | Admin API; bind to localhost in prod    |
| `-upstream`      | (required)             | Upstream URL or stdio command           |
| `-policy`        | `configs/policy.yaml`  | Per-tool idempotency policy             |
| `-store`         | `memory`               | `memory` \| `bolt`                      |
| `-db`            | `potent.db`            | BoltDB file path (when `-store=bolt`)   |
| `-embed-dim`     | `384`                  | Embedding dimension                     |
| `-embed-ngram`   | `4`                    | Char n-gram size for the embedder       |
| `-audit-log`     | (disabled)             | JSONL audit records appended here       |

## Policy file

```yaml
defaults:
  mode: log_only      # strict | cache | log_only | off
  ttl: 1h
  semantic_threshold: 0.9

tools:
  send_email:
    mode: strict
    ttl: 24h
    normalize:
      to: [lowercase, trim]
      subject: [trim, collapse_whitespace]
    fingerprint_fields: [to, subject, body]
    semantic_threshold: 0.94
```

### Modes

- **strict** — block/replay duplicates; the safe default for side-effecting tools
- **cache** — replay cached responses for read-only tools (search, lookups)
- **log_only** — detect duplicates and log them; always forward
- **off** — bypass the pipeline entirely

## Admin API (`-admin-addr`)

```
GET    /stats?tool=NAME                per-tool aggregates
GET    /fingerprints?tool=NAME         list cached entries (redacted)
DELETE /fingerprints/{tool}/{hash}     evict one fingerprint
```

**Authentication is required.** Set `POTENT_ADMIN_TOKEN` in the environment
before starting potent with `-admin-addr`; the process refuses to start
without it. Every admin request must carry:

```
Authorization: Bearer <POTENT_ADMIN_TOKEN>
```

Comparisons are constant-time. Generate the token with
`openssl rand -hex 32` and store it in your secret manager.

Bind to `127.0.0.1:9095` (the recommended value). Potent logs a warning when
`-admin-addr` is not bound to loopback — even with a token, network exposure
should be gated by mTLS or a NetworkPolicy.

## Audit log (`-audit-log`)

Each decision appends one JSON line:

```json
{"ts":"2026-05-29T01:23:45Z","tool":"send_email","mode":"strict","decision":"replay","match":"exact","similarity":1,"hash":"a118a..."}
```

Sends are buffered through a 1024-deep channel; the writer goroutine drains
the buffer on shutdown. Disk full or slow downstream applies backpressure to
the request path.

## Metrics

| Metric                                        | Type      | Labels                       |
|-----------------------------------------------|-----------|------------------------------|
| `potent_proxy_decisions_total`                | counter   | `tool`, `mode`, `decision`   |
| `potent_proxy_upstream_latency_seconds`       | histogram | `tool`                       |
| `potent_store_errors_total`                   | counter   | `op`                         |

## Response headers

| Header                 | Values                              |
|------------------------|-------------------------------------|
| `X-Potent-Status`      | `fresh` \| `replayed`               |
| `X-Potent-Hash`        | SHA-256 of canonicalized intent     |
| `X-Potent-Match`       | `exact` \| `semantic` (on replay)   |
| `X-Potent-Similarity`  | `0.0000`–`1.0000` (on semantic)     |

## Deployment

- `Dockerfile` — distroless static, runs as `nonroot` (uid 65532)
- `deploy/k8s/potent.yaml` — Deployment + Service + ConfigMap with readOnlyRootFilesystem and dropped caps
