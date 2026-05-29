# Operating potent

For an overview of the pipeline and package layout, see [architecture.md](./architecture.md).

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
| `-max-body-bytes` | `1048576` (1 MiB)     | Cap on inbound tool-call request bodies. `0` disables. |
| `-stdio-request-timeout` | `60s`          | How long an in-flight mcp-stdio `tools/call` may wait before being treated as failed. |
| `-bolt-compact-interval` | `1h`           | How often the bolt store sweeps expired entries from disk. `0` disables. |

## Coalescing

When two requests with the same fingerprint arrive while the first is still in flight, potent coalesces them. The leader executes upstream once; followers receive the leader response. This works for the generic pipeline (`http`, `mcp-http`) and for `mcp-stdio`. Followers waiting longer than `-stdio-request-timeout` are released with an error rather than blocking indefinitely.

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

## OIDC bearer tokens

Production deployments often need IdP-signed JWTs instead of static bearer tokens (Okta, Auth0, Azure AD, Keycloak, Google). Configure the four OIDC flags:

```
-oidc-jwks-url https://idp.example.com/.well-known/jwks.json
-oidc-issuer https://idp.example.com
-oidc-audience potent
-oidc-caller-claim sub          # default; can be email, preferred_username, etc.
-oidc-refresh-interval 1h       # how often to re-fetch the JWKS
```

When `-oidc-jwks-url` is set, inbound bearers are parsed as JWTs, verified against the IdP's published public keys, and gated on `iss`/`aud`/`exp`/`nbf` claims. The configured caller claim is extracted and attached to the request context exactly like the static-token path so per-tool `allowed_callers` ACLs work without change.

Algorithms accepted: RS256, RS384, RS512, ES256, ES384, ES512. The `none` algorithm is explicitly rejected. A 30-second clock-skew tolerance is built in.

The JWKS is fetched at startup, cached, and refreshed in the background at `-oidc-refresh-interval`. Network failures during refresh are logged but the cached keys keep working; if no keys are cached yet and the first fetch fails, every request will 401 until the next refresh succeeds.

## PII redaction

Two per-tool policy fields keep sensitive bytes off disk while preserving the dedup behavior:

```yaml
tools:
  charge_card:
    mode: strict
    fingerprint_fields: [customer_id, amount_cents, currency]
    replay_strategy: synthesized_ack
    synthesized_response: '{"already_charged":true,"hash":"$hash"}'
    redact_request_body: true
```

- `replay_strategy: synthesized_ack` tells potent never to store the upstream response body. On a duplicate, the configured template is returned instead. `$hash` and `$tool` are expanded. Default is `cached_response` (original byte-for-byte replay).
- `redact_request_body: true` stores `nil` for the raw inbound bytes. The fingerprint (one-way SHA-256) still records the canonical intent so dedupe keeps working; only the admin debug view and any post-mortem disk dump lose signal.

Together these eliminate the two paths through which PII can reach the on-disk cache. Verified by scanning the bolt file in tests; sensitive strings from the upstream response and the request never appear.


potent speaks the [W3C Trace Context](https://www.w3.org/TR/trace-context/) `traceparent` header on every HTTP and MCP HTTP request:

- If the inbound request carries a valid `traceparent`, potent preserves the trace id and generates a fresh span id for its hop.
- If the header is missing or malformed, potent generates a brand new context, marked sampled.
- The chosen context is echoed back on the response so the caller can correlate.
- Outbound requests to upstream propagate the context unchanged so the operator's distributed-tracing backend (Datadog, Honeycomb, Jaeger, Tempo, etc.) sees one trace across agent + potent + upstream.

The structured log handler injects `trace_id` and `span_id` into every record whose context carries a span, so log lines correlate with the spans without any backend-specific code. Audit log records carry the `trace_id` for compliance correlation.

No OpenTelemetry SDK is bundled today; the operator's collector can scrape the proxy headers and the slog output independently. The W3C header shape and the audit field name will stay stable when (and if) a full OTLP exporter is added.

## Shadow mode

Start potent with `-shadow-mode` and every tool call is forwarded to upstream regardless of policy: no replays, no blocks, no rate-limit rejections, no ACL forbiddens. The audit log records what the policy *would* have done.

Use shadow mode to calibrate `semantic_threshold`, ACLs, and rate limits against real traffic before flipping the policy on. Run for a representative window (a day, a week), then summarize:

```
potent-eval -from-audit /var/log/potent/audit.log
```

The summary reports the would-be decision distribution per tool, the unique fingerprint count, and the top repeated hash. The `would_replay_rate` is the headline: it's the fraction of requests that would have been deduped. If it's near zero on a tool, dedup is not earning its keep there. If it's high, you have evidence that flipping the policy on will save real upstream calls.

Shadow mode still writes to the cache, so the would-be replay decisions reflect what production would have done with a warm cache.

## Resilience

- Panic recovery. Every proxy handler is wrapped in recovery middleware. A panic returns 500 and increments an error metric instead of killing the process.
- Leader timeout sweeper. Coalesced waiters whose leader exceeds `-stdio-request-timeout` are released with a synthetic error so callers do not hang.
- Bolt background compactor. Runs every `-bolt-compact-interval`, deletes expired entries in batches, keeps the on-disk file from growing unbounded.
- Structured logging. All `log/slog` output goes to stderr with correlation IDs. stdout is reserved for `mcp-stdio` JSON-RPC framing.

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
