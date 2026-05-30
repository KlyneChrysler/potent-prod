# Changelog

All notable changes to potent are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0] - 2026-05-30

### Added
- HTTP embedder backend (`-embed-backend=http -embed-url ...`). Operators plug in any external embedding service that follows the minimal contract `POST {"input": "<text>"} -> {"embedding": [...]}` (also accepts the OpenAI-shape `{"data": [{"embedding": [...]}]}`). Compatible out of the box with sentence-transformers, OpenAI, Ollama, Cohere, Voyage, or any FastAPI shim. Auth via `POTENT_EMBED_API_KEY` env var (sent as `Authorization: Bearer ...`). Dim auto-discovered on first call; vectors L2-normalised client-side so backends returning raw transformer hidden states still produce cosine in `[-1, 1]`. Falls back to writing the cache entry without an embedding on timeout or non-2xx; exact-match dedup keeps working. New `internal/embed/http.go` with 93.2% coverage; verified end-to-end against a local Python embedding server through potent's HTTP mode.
- Supply-chain hardening: every release now ships a CycloneDX SBOM per archive (via syft) and is keyless-signed via cosign + Sigstore. Container images signed too. New `docs/benchmarks.md` documents reproducible numbers.

### Changed
- Embedder selection is now a flag (`-embed-backend`) instead of an implicit default. `hashing` (the prior behavior) remains the default; explicit opt-in to `http` for transformer-quality semantic dedup.

### Performance baseline
- BenchmarkApply_ExactReplay: 8.8 µs/op (~113k req/s ceiling single-thread)
- BenchmarkApply_ForwardMiss: 9.1 µs/op
- End-to-end HTTP: 6.7k req/s sustained at 100 concurrent on M1, p95 < 41 ms
- See `docs/benchmarks.md` for reproduction commands and the thundering-herd note on hot-cache p99.

## [0.1.16] - 2026-05-30

### Added
- Postgres store backend (`-store=postgres`, DSN via `POTENT_PG_DSN`). Multiple potent replicas can sit behind a load balancer and share one cache; a node going away no longer loses the cache. Last-write-wins upsert on `(tool, hash)`; `replay_count` aggregates across replicas. Background TTL eviction reuses `-bolt-compact-interval` (flag name kept for backwards compatibility). Schema is created on first connect; no separate migration tool. Uses `github.com/jackc/pgx/v5/pgxpool`; embeddings packed as little-endian IEEE-754 bytes so no `pgvector` dependency.
- HA verified end-to-end: two potent instances sharing one Postgres dedupe a single fingerprint to one upstream call total (not one per instance).
- New `postgres-integration` CI job runs the conformance suite against a real Postgres 16 service container on every push and PR.

### Fixed
- Audit log now records the actual wire decision instead of the lookup-time decision. Concurrent followers that coalesced as waiters used to be logged as `decision=forward` even though they never reached upstream; they now log as `decision=replay match=coalesced`. Caught during a 50-concurrent stress test (only 1 upstream hit, audit was reporting 19). Operators computing cost savings or compliance dedup rates from the audit log now get correct numbers. New regression test pins the semantics. New match kind `coalesced` distinguishes in-flight dedup from cache-hit on an existing entry (which keeps `match=exact`).

## [0.1.15] - 2026-05-30

### Fixed
- Policy YAML loader stripped surrounding double or single quotes from inline list elements. Before the fix, a policy that used YAML's quoted-string syntax (e.g. `fingerprint_fields: ["path"]`) silently produced the empty-object fingerprint for every request on that tool, disabling exact-match dedup. Every shipped example used bare identifiers so this surfaced only when an external operator wrote a YAML-correct quoted list. Caught by an end-to-end smoke test through `mcp-stdio` against `npx @modelcontextprotocol/server-filesystem`.

## [0.1.14] - 2026-05-30

### Added
- S3 audit sink (`-audit-sink-s3 s3://bucket/prefix`). Records are buffered in memory and uploaded either every `-audit-s3-flush-interval` (default 5 minutes) or once buffered bytes cross `-audit-s3-flush-bytes` (default 5 MiB), whichever comes first. Keys are Hive-partitioned (`year=YYYY/month=MM/day=DD/hour=HH/audit-<ts>.jsonl`) so Athena, BigQuery External, Trino, and Spark auto-discover the partitions. Default AWS credential chain is used (env vars, shared profile, EC2/EKS/ECS instance roles). The audit pipeline now supports multiple sinks at once, so `-audit-log` and `-audit-sink-s3` can be active in the same process. Internal refactor introduces an `audit.Sink` interface and `audit.NewWriter(sinks ...Sink)` constructor; existing `audit.Open` and `audit.NewDiscard` remain for backwards compatibility.

### Changed
- `audit.Writer` is now a fan-out dispatcher rather than a single-channel writer; each sink owns its own buffering and shutdown. Behavior is unchanged for callers using `audit.Open(path, cap)`.

## [0.1.13] - earlier

### Added
- OIDC bearer-token validation. New `internal/oidc` package fetches JWKS from the IdP, caches keys, refreshes in the background, and verifies inbound JWTs (RS256/RS384/RS512/ES256/ES384/ES512). Claim gates: `iss`, `aud`, `exp`, `nbf` (30-second clock-skew tolerance). The configured caller claim (default `sub`) is extracted and attached to the request context so per-tool `allowed_callers` ACLs work unchanged. CLI flags: `-oidc-jwks-url`, `-oidc-issuer`, `-oidc-audience`, `-oidc-caller-claim`, `-oidc-refresh-interval`. The `none` algorithm is explicitly rejected.
- Per-tool PII redaction. New policy fields `replay_strategy` and `redact_request_body`. `replay_strategy: synthesized_ack` skips storing the upstream response body and returns a configurable `synthesized_response` template (with `$hash` and `$tool` expansion) on replay. `redact_request_body: true` stores `nil` for the raw inbound bytes; the fingerprint hash keeps the cache functional. Together, sensitive bytes from the upstream and from the request stay off disk. Verified by scanning the bolt file in tests.
- W3C Trace Context propagation. New `internal/tracing` package parses the inbound `Traceparent` header (or generates one when missing), stores a `SpanContext` in the request context, echoes potent's chosen context on the response, propagates it to upstream on every HTTP and MCP HTTP forward, and injects `trace_id` + `span_id` into structured logs via a wrapping `slog.Handler`. Audit log records gain a `trace_id` field. No OpenTelemetry SDK is bundled; this is the minimum that lets Datadog/Honeycomb/Jaeger correlate the agent + potent + upstream hops out of the box.
- Shadow mode (`-shadow-mode`). Every tool call is forwarded to upstream regardless of policy while the audit log records what the policy would have done. Operators run for a window, then summarize with `potent-eval -from-audit`. Closes the loop from the offline eval: lets operators build a calibration corpus from their own production traffic before flipping the policy on.
- `potent-eval -from-audit` subcommand. Reads a potent audit-log jsonl file (typically captured under shadow mode) and reports per-tool would-decision distribution, unique fingerprint count, top repeated hash, and `would_replay_rate`.
- Audit log records gain `shadow` and `would_decision` fields when the proxy is running in shadow mode. The on-disk schema is backward-compatible (old parsers ignore the new fields).
- Semantic-dedup eval harness `cmd/potent-eval`. Reads a labelled jsonl dataset (`docs/eval/dataset.jsonl`) and reports precision/recall/FPR at every candidate threshold so `semantic_threshold` is calibrated against data instead of guessed. CI runs the harness on every push and uploads the json artifact for regression tracking. The methodology, results, and recommended per-tool defaults are in `docs/eval/`.
- Multi-tenant tokens (`-api-tokens-file`) plus per-tool `allowed_callers` ACLs.
- mTLS to upstream via `-upstream-ca`, `-upstream-cert`, `-upstream-key`.
- Proxy-port bearer auth. The `http` and `mcp-http` modes accept an optional `POTENT_API_TOKEN`; when set, every inbound request must carry `Authorization: Bearer <token>`. Constant-time comparison. The process refuses to start with an unauthenticated proxy bound to a non-loopback address.
- Per-tool rate limiting. `RateLimit { RPS, Burst }` in the policy YAML. Token-bucket via `golang.org/x/time/rate`. Excess requests return `429` (HTTP) or JSON-RPC `-32005` (MCP HTTP). `RPS=0` disables.
- `/readyz` health endpoint separate from `/healthz`. Liveness vs readiness so Kubernetes can distinguish "restart" from "stop sending traffic".
- Shared `internal/auth` package. Single bearer-token implementation used by admin, http, and mcp-http; constant-time compare and `WWW-Authenticate` challenge consolidated.

## [v0.1.6] - 2026-05-29

### Added
- Background bolt compactor reclaims expired entries from disk. Lazy delete in `Get` no longer leaves dead data forever. Configurable via `-bolt-compact-interval` (default `1h`, `0` disables).

### Fixed
- Leader panic in `pipeline.Apply` no longer deadlocks coalesced waiters. A panic in `forward()` is now captured on the inflight handle so waiters surface it as an error instead of silently receiving a zero `Result`. The panic re-raises so the leader's own caller still sees it.

## [v0.1.5] - 2026-05-29

### Fixed
- Concurrent duplicate POSTs in `http` and `mcp-http` modes now coalesce instead of all reaching upstream. Same in-flight machinery that v0.1.2 added to `mcp-stdio` was lifted into `pipeline.Apply`.

## [v0.1.4] - 2026-05-29

### Added
- `-max-body-bytes` and `-stdio-request-timeout` CLI flags so the body cap and request timeout are tunable without recompiling.

### Changed
- Lowered the stdio sweep interval floor from 5 s to 500 ms so short-timeout configurations expire promptly.

## [v0.1.3] - 2026-05-29

### Added
- Pending and inflight maps in `mcp-stdio` now expire (default `60s`). Prevents OOM from orphaned entries when a child server hangs or crashes. Clients waiting on a coalesced duplicate receive a JSON-RPC `-32000 'upstream timeout'` error.
- HTTP request bodies are capped at 1 MiB by default. Oversized requests return `413` without touching cache or upstream.

## [v0.1.2] - 2026-05-29

### Fixed
- `mcp-stdio` coalesces pipelined duplicate `tools/call` requests. Three identical pipelined calls used to all hit the upstream because the cache write for the leader had not yet completed. The first request becomes a leader; subsequent duplicates wait on the leader and receive replays.
- Byte interleaving in `writeLine` when two concurrent goroutines wrote to the same `clientOut`. `writeLine` now emits one `Write` per frame and `writeClient` serializes via `writeMu`.

## [v0.1.1] - 2026-05-29

### Fixed
- Structured logs no longer corrupt the `mcp-stdio` JSON-RPC stream. `slog` now writes to stderr, not stdout. Every real MCP client (Claude Code, Cursor, Claude Desktop) would have failed to parse the startup log line otherwise.

## [v0.1.0] - 2026-05-29

Initial public release.

### Added
- Three modes: `http`, `mcp-http`, `mcp-stdio`.
- Canonical JSON + BLAKE3 fingerprint for exact-match deduplication.
- Char n-gram embedder + cosine search for semantic deduplication.
- BoltDB persistent store.
- Authenticated admin API behind `POTENT_ADMIN_TOKEN`.
- JSONL audit log.
- Prometheus metrics.
- Response headers: `X-Potent-Status`, `X-Potent-Hash`, `X-Potent-Match`, `X-Potent-Similarity`.

[Unreleased]: https://github.com/KlyneChrysler/potent-prod/compare/v0.1.6...HEAD
[v0.1.6]: https://github.com/KlyneChrysler/potent-prod/releases/tag/v0.1.6
[v0.1.5]: https://github.com/KlyneChrysler/potent-prod/releases/tag/v0.1.5
[v0.1.4]: https://github.com/KlyneChrysler/potent-prod/releases/tag/v0.1.4
[v0.1.3]: https://github.com/KlyneChrysler/potent-prod/releases/tag/v0.1.3
[v0.1.2]: https://github.com/KlyneChrysler/potent-prod/releases/tag/v0.1.2
[v0.1.1]: https://github.com/KlyneChrysler/potent-prod/releases/tag/v0.1.1
[v0.1.0]: https://github.com/KlyneChrysler/potent-prod/releases/tag/v0.1.0
