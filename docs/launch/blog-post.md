# I let an AI agent refund customers and it tried to refund the same one four times. Here's the open-source fix.

If you're shipping autonomous agents that take real actions — sending emails, charging cards, writing to databases — you have a duplicate-execution problem. The LLM doesn't know.

## The bug class nobody talks about

Stripe's idempotency keys solved the duplicate-payment problem for humans writing HTTP clients. Pass `Idempotency-Key: 0c2e91...` once, retry the same call all you want, only one charge gets through.

LLMs break this assumption two ways:

1. **They generate the key.** If the agent retries a tool call, the LLM produces a new key. Now you have two "fresh" requests with two different keys and your idempotency layer treats them as two distinct intents.

2. **They generate the request body.** Even if you tried to hash the body to derive a key, LLMs don't produce byte-identical output across retries. "Hi Alice, please find the Q3 report attached" becomes "Hi Alice, please find Q3 report attached" — same intent, different bytes, different hash.

This is the bug class behind the "agent sent the same email three times" stories you see on r/LocalLLaMA every other week. It's a category of failure, not a bug in any particular framework.

## What didn't work

I tried the obvious fixes first.

**Tell the LLM to use a deterministic key.** Works ~70% of the time. The 30% it doesn't, you don't know about it until a customer complains.

**Hash the body.** Same problem the LLM has — minor word swaps blow the hash. Whitespace and casing alone are common offenders.

**Catch retries in the agent framework.** Works inside one framework. Doesn't help when the agent talks to N tool servers and you change frameworks every six months.

The right place to put this logic is between the agent and the tool, not inside either. It needs to be a sidecar that the agent can't see and the tool doesn't have to know about.

## Enter Potent

[Potent](https://github.com/KlyneChrysler/potent-prod) is a Go gateway that does exactly that. For every tool call it sees, it:

1. **Normalizes** the arguments per a per-tool policy you write — trim whitespace, lowercase emails, collapse runs of spaces.
2. **Fingerprints** the canonical form with SHA-256. If you've seen the same intent before, the hash matches and you replay the cached response.
3. **Falls back to semantic similarity** when the exact hash misses. An embedder turns the intent into a 384-dim vector; cosine similarity above your policy's threshold counts as a duplicate.

Three transport modes today: generic HTTP reverse proxy, MCP Streamable HTTP, and MCP stdio. Same pipeline behind all three, so you write the policy file once and pick the protocol per deployment.

## A policy looks like this

```yaml
tools:
  send_email:
    mode: strict
    ttl: 24h
    normalize:
      to: [lowercase, trim]
      subject: [trim, collapse_whitespace]
    fingerprint_fields: [to, subject, body]
    semantic_threshold: 0.9

  charge_card:
    mode: strict
    require_human_confirm_on_replay: true   # never silently replay money
    fingerprint_fields: [customer_id, amount_cents, currency]

  search_web:
    mode: cache                              # read-only; replay freely
    ttl: 5m

  log_event:
    mode: off                                # bypass entirely
```

Four modes per tool: `strict` (block/replay), `cache` (replay), `log_only` (detect but always forward), `off`. Strict is the right default for anything that has a side effect.

## The semantic tier

The exact-match fingerprint handles whitespace and casing once you've configured `normalize`. It doesn't handle "Please find the Q3 report attached" vs "Please find Q3 report attached" — same intent, different words.

For that, Potent ships a zero-dependency embedder: char n-grams hashed into a 384-dim float vector, L2-normalized. It's a hashing-trick TF-IDF embedder — production NLP shipped this kind of thing for a decade before transformers. Near-duplicates score cosine >0.95; unrelated strings stay below 0.3.

When you want transformer-quality matching, the `Embedder` interface has two methods. An ONNX-backed bge-small drops in without touching the proxy.

## What you get for free

- **BoltDB** persistence (or in-memory for dev). Cache survives restarts.
- **Prometheus** metrics on a separate port. Decisions, upstream latency, store errors.
- **JSONL audit log** of every decision. Backpressure-aware, drains on shutdown.
- **Authenticated admin API** (bearer token, constant-time compare) for `/stats`, `/fingerprints`, and `DELETE`.
- **Distroless non-root container.** k8s manifest with `readOnlyRootFilesystem`, dropped caps, RuntimeDefault seccomp.

## What's left

Pre-1.0. Building in public. The hashing-TFIDF embedder is honest about its limits — for the 5% of cases where transformer semantics matter, the ONNX swap is the next planned change. Brute-force cosine over per-tool entries is fine until your per-tool cache exceeds ~10k; an ANN index lands when scale demands it.

## Try it

```bash
brew install KlyneChrysler/tap/potent   # or grab a binary from releases
potent -mode http -upstream http://your-tool-server -policy policy.yaml
```

Three working examples in the repo: [Claude Code MCP](https://github.com/KlyneChrysler/potent-prod/tree/main/examples/claude-code-mcp), [LangGraph](https://github.com/KlyneChrysler/potent-prod/tree/main/examples/langgraph-agent), [raw OpenAI tool-calling](https://github.com/KlyneChrysler/potent-prod/tree/main/examples/openai-python).

Apache 2.0. Issues and PRs welcome.

> [github.com/KlyneChrysler/potent-prod](https://github.com/KlyneChrysler/potent-prod)
