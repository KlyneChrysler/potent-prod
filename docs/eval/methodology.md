# Semantic-dedup evaluation methodology

The product thesis is that potent's semantic tier catches real LLM retries reliably and safely. This document describes how we test that.

## What the eval measures

For each tool, every pair of records `(a, b)` in the dataset is run through the same normalize + fingerprint + embedder pipeline the proxy uses at request time. The pair is labelled:

- `dedup should fire` when `a.intent_id == b.intent_id` (same intent, different bytes)
- `dedup should not fire` when `a.intent_id != b.intent_id` (different intent, same tool)

At each candidate threshold in `[0.50, 0.99]` in 0.01 steps, the eval counts TP / FP / TN / FN and reports precision, recall, FPR, and F1.

## Dataset composition

The current dataset has 51 records across 5 tools chosen to span the realistic shape space of agent tool calls:

| Tool | Shape | Why it's in the set |
|---|---|---|
| `send_email` | text-heavy, long body | the README's marquee example; most representative of "LLM-rephrased prose" |
| `charge_card` | mixed numeric + short memo | the tool where false positives cost most (a wrongly blocked refund) |
| `create_task` | text-heavy + structured fields (priority) | tests whether the embedder discriminates load-bearing structured fields |
| `delete_user` | single ID field | sanity check that exact match dominates |
| `search_web` | short query | hardest case for any embedder (low signal per token) |

Each tool has 5-8 distinct intents. Each intent has a "baseline" record and 2-4 variations spanning:

- `whitespace-casing` — the trivial case the exact tier already handles
- `reorder` — same words, different sentence order
- `synonym` — single-word swaps ("find" -> "see", "review" -> "look at")
- `paraphrase` — same intent, mostly different words

Adversarial negatives are constructed deliberately to share template words with positives but differ on a load-bearing field. Examples:

- `charge_card`: same customer, same memo template, different `amount_cents`
- `create_task`: same description, different `priority`
- `send_email`: same template body, different recipient or opposite intent ("Welcome" vs "Offboarding")
- `search_web`: same topic words ("idiomatic Go") different intent ("test" vs "write" vs "optimize")

## Honest limitations

1. **Hand-crafted, not real LLM traces.** The variations are written to match how LLMs are known to rephrase, but they are not captured from a real agent. Real LLM retries may distribute differently. Operators with real data should run the harness on their own dataset.

2. **Small sample.** 51 records, 280 same-tool pairs. Statistical confidence is limited. A larger dataset will tighten the numbers but the qualitative picture is unlikely to flip.

3. **No threshold blending across categories.** F1 is computed pooling all pair types. In practice an operator may want to weight precision higher (avoid false blocks on payment tools) or recall higher (catch every dedup on idempotent reads).

4. **Embedder under test is the v0.1 hashing-TFIDF.** Numbers are a lower bound on what the architecture supports; an ONNX/transformer swap behind the same `Embedder` interface should improve them. The eval will be re-run when that ships.

## How to use the harness

```bash
go build -o bin/potent-eval ./cmd/potent-eval
./bin/potent-eval \
  -dataset docs/eval/dataset.jsonl \
  -policy docs/eval/policy.yaml
```

Add `-json` for machine-readable output (suitable for CI regression tracking). Add `-v` to dump the full per-threshold sweep.

To evaluate against your own dataset, write your records in the same jsonl format:

```jsonl
{"id":"my-1","tool":"send_email","intent_id":"intent-1","args":{...}}
{"id":"my-2","tool":"send_email","intent_id":"intent-1","args":{...}}
```

Two records with the same `intent_id` are positives (dedup should fire). Two records with different `intent_id` are negatives (dedup should not fire).
