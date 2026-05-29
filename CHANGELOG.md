# Changelog

All notable changes to potent are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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
