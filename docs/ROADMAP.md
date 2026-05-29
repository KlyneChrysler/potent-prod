# Roadmap

Current version: v0.1.6. See [architecture.md](./architecture.md) for an overview of what shipped and how the pipeline fits together.

## Shipped (v0.1.0 to v0.1.6)

- Three proxy modes: `http` (generic reverse proxy keyed on `X-Potent-Tool`), `mcp-http` (Streamable HTTP MCP), `mcp-stdio` (child-process MCP server).
- Canonical JSON normalizer and BLAKE3 fingerprint for the exact-match path.
- Char n-gram embedder plus cosine similarity for the semantic-match path.
- BoltDB persistent store with a background compactor that sweeps expired entries.
- JSONL audit log with a buffered writer and shutdown drain.
- Prometheus metrics (`potent_proxy_decisions_total`, `potent_proxy_upstream_latency_seconds`, `potent_store_errors_total`).
- Response headers: `X-Potent-Status`, `X-Potent-Hash`, `X-Potent-Match`, `X-Potent-Similarity`.
- Authenticated admin API behind `POTENT_ADMIN_TOKEN`, constant-time comparison, loopback warning.
- Pipeline coalescing for `http` and `mcp-http`, plus in-flight `mcp-stdio` coalescing so concurrent retries share one upstream call.
- Panic recovery middleware on every proxy handler.
- Leader timeout sweeper that releases stuck coalesced waiters with a synthetic error.
- Inbound body size cap (`-max-body-bytes`) for DoS protection.
- Structured `log/slog` output on stderr with correlation IDs (stdout reserved for mcp-stdio framing).

## Next

- Swap the char n-gram embedder for an ONNX model behind the existing `Embedder` interface.
- OpenTelemetry traces alongside the Prometheus counters.
- S3 audit sink in addition to the local JSONL file.
- Helm chart for k8s, published alongside the existing `deploy/k8s` manifest.
- mkdocs site built from `docs/` on tag.
