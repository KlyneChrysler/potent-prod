# Hacker News launch post

**Title (≤80 chars):**
> Show HN: Potent – Stop AI agents from doing the same action twice

**URL:** https://github.com/KlyneChrysler/potent-prod

**Text:**

I let an agent autonomously refund customers last month and it tried to refund the same person four times. The LLM phrased the retry slightly differently each time, so Stripe's idempotency keys didn't catch it.

Every system I looked at solves this either by (a) requiring the client to pass an idempotency key the LLM probably won't generate consistently, or (b) baking dedup into a specific framework (LangGraph, Temporal). I wanted a transparent sidecar — no SDK, no code changes, drop it in front of any tool server and never double-fire again.

That's Potent. It's a Go gateway that:

1. Normalizes the tool arguments per a per-tool policy (trim, lowercase, sort keys)
2. Fingerprints the canonical form with SHA-256 (exact match)
3. Falls back to embedding-based cosine search when the exact hash misses — catches retries where the LLM rephrased the body
4. Replays the cached response on a hit, forwards on a miss, optionally blocks for human review

Three transport modes: generic HTTP reverse proxy, MCP Streamable HTTP, MCP stdio. The same idempotency pipeline runs across all three so the policy file is the only thing you write.

Tech notes:
- Zero-dep embedder (char n-gram + signed hashing trick + L2-norm). ONNX swap is a one-file change behind the Embedder interface — wanted to ship the architecture first.
- BoltDB or in-memory store, same Store interface
- Prometheus metrics, JSONL audit log, authenticated admin API
- Distroless non-root container, k8s manifest with readOnlyRootFilesystem + dropped caps
- ~2,500 LOC, every package ≥80% coverage, vet/staticcheck/gosec clean

I'd love feedback on: (a) the policy DSL — is "normalize + fingerprint_fields + semantic_threshold" the right abstraction or am I missing a category? (b) the semantic tier — when does hashing-TFIDF fall over vs. a transformer embedder? (c) anything obvious I'm missing for production deployment.

Examples (Claude Code MCP, LangGraph, raw OpenAI) and docs in the repo.
