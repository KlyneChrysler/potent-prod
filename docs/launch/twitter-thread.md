# X/Twitter launch thread

**1/**
I let an AI agent autonomously refund customers last month.

It tried to refund the same person FOUR times.

The LLM phrased the retry slightly differently each time, so Stripe's idempotency keys didn't catch it.

So I built Potent →

**2/**
Potent is a transparent Go gateway that sits between your agent and your tools.

For every tool call:
→ normalizes the args (trim, lowercase, sort keys)
→ fingerprints the canonical form (SHA-256)
→ falls back to embedding-based semantic match
→ replays cached response on a hit

[demo gif]

**3/**
The wins:

✅ No client SDK. No code changes.
✅ Works with any tool server (HTTP, MCP, stdio).
✅ Catches "Hi Alice" vs "Hi alice" (exact)
✅ Catches "send Q3 report" vs "send the Q3 report" (semantic)
✅ Per-tool policy: strict / cache / log_only / off

**4/**
Three transport modes today:

→ generic HTTP reverse proxy (X-Potent-Tool header)
→ MCP Streamable HTTP (intercepts tools/call frames)
→ MCP stdio (spawns your MCP server as a child)

Same idempotency pipeline behind all three.

**5/**
Production-ready:

→ BoltDB or in-memory store
→ Prometheus metrics endpoint
→ JSONL audit log for compliance
→ Authenticated admin API (bearer token, constant-time compare)
→ Distroless non-root container
→ k8s manifest with dropped caps + readOnlyRootFilesystem

**6/**
Honest about what's pre-1.0:

→ Embedder is hashing-TFIDF (char n-gram). Works great for normalized dupes; ONNX/transformer swap is a one-file change.
→ Brute-force cosine scan over per-tool entries. ANN index lands when scale demands.

**7/**
Apache 2.0, open from day 1.

⭐ github.com/KlyneChrysler/potent-prod

Built in 6 weeks in public. Three examples included (Claude Code MCP, LangGraph, raw OpenAI).

Would love your worst war stories with double-fired agent tool calls 👇
