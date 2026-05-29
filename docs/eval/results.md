# Semantic-dedup evaluation results

## TL;DR

The current hashing-TFIDF embedder catches normalized whitespace/casing and word-reorder LLM variants reliably. Paraphrase recall is partial. False-positive rates are non-trivial for tools whose adversarial cases share template words with real duplicates.

**Specifically, the claim "semantic_threshold: 0.9 catches LLM retries" overstates what this embedder delivers on text-heavy tools.** Calibration is required per tool.

## Dataset under test

51 records, 280 same-tool pairs across 5 tools. Methodology and limitations: [methodology.md](./methodology.md). Raw data: [dataset.jsonl](./dataset.jsonl).

## Best-F1 threshold per tool

| Tool | Best threshold | Precision | Recall | FPR | F1 | Read |
|---|---|---|---|---|---|---|
| `send_email` | 0.73 | 0.559 | 1.000 | 0.128 | 0.717 | Catches every real retry, blocks ~13% of distinct emails |
| `charge_card` | 0.91 | 0.375 | 0.750 | 0.156 | 0.500 | 5 false positives. Do not use semantic on payments |
| `create_task` | 0.85 | 0.462 | 0.857 | 0.241 | 0.600 | Marginal. Adversarial cases score close to real ones |
| `delete_user` | 0.99 | 1.000 | 1.000 | 0.000 | 1.000 | Exact match already handles this; semantic moot |
| `search_web` | 0.81 | 0.625 | 0.455 | 0.055 | 0.526 | Short queries are hard. Misses 55% of real paraphrases |

## What the numbers mean for each tool

### `send_email` (the marquee tool)

- At threshold **0.9** (the README's prior default): recall drops to **0.263**. The embedder misses 74% of paraphrases an LLM would realistically produce.
- At threshold **0.73** (best F1): recall is **1.000** but **12.8% of legitimately distinct emails** would be wrongly blocked.
- The trade is real and tool-dependent. No global threshold is right.

### `charge_card`

The combination of short structured fields (`amount_cents`, `currency`, `customer_id`) and an adversarial set that varies one numeric field per pair means the embedder confuses "$100 to Acme" with "$110 to Acme". Five distinct refunds would be wrongly blocked at threshold 0.91.

**Recommendation: set `semantic_threshold: 0` (exact match only) for payment tools.** Exact normalization handles the legitimate retry variations (whitespace, casing); the semantic tier introduces more risk than it removes.

### `create_task`

Adversarial cases (same description, different `priority`; same template, different quarter) score 0.85+. Best F1 of 0.600 is marginal. Same recommendation as `charge_card`: prefer exact match unless the body is the primary signal.

### `delete_user`

A single `user_id` field cannot benefit from semantic. The exact tier handles it perfectly; semantic is moot. Setting `semantic_threshold: 0` is appropriate.

### `search_web`

Short queries pack too little signal for the embedder to distinguish topic from intent. "how to write idiomatic Go code" and "how to test idiomatic Go code" score similarly to two real paraphrases of the same query. Recall caps at 0.455 at any threshold that keeps FPR below 10%.

**Recommendation: for `cache` mode (read-only tools), accept the lower recall.** Even if a duplicate slips through, it's not a side effect; the upstream just does a search twice.

## Honest assessment of the thesis

The product thesis is "semantic intent dedup catches real LLM retries reliably and safely". After this eval:

- **The mechanism works as designed.** Exact matching + semantic fallback executes, headers fire, audit log records decisions.
- **For text-heavy tools, semantic adds real value** but with a precision/recall tradeoff that operators must calibrate against their own pain (more duplicates leaked through vs more legitimate calls blocked).
- **For structured/numeric tools the semantic tier is at best neutral and at worst harmful.** Recommend `semantic_threshold: 0` (exact-only).
- **For very short text (search queries), the embedder is weak.** A transformer swap should help; the hashing-TFIDF approach is at a fundamental disadvantage on short inputs.

The README's pre-1.0 admission ("hashing-TFIDF is honest about its limits") is validated by these numbers. The ONNX swap behind the `Embedder` interface is the next step; this eval will be re-run when it ships and the comparison will be in this file.

## Updated default recommendations

Use these as starting thresholds, then re-run the harness against your own dataset:

```yaml
tools:
  send_email:
    semantic_threshold: 0.73   # was 0.9; raised precision will require ONNX
  charge_card:
    semantic_threshold: 0      # exact only; semantic risks too many false blocks
  create_task:
    semantic_threshold: 0      # exact only on side-effecting writes
  delete_user:
    semantic_threshold: 0      # exact only on single-field IDs
  search_web:
    semantic_threshold: 0.81   # cache mode; lower recall is acceptable
```

## Reproducing this report

```bash
go build -o bin/potent-eval ./cmd/potent-eval
./bin/potent-eval \
  -dataset docs/eval/dataset.jsonl \
  -policy docs/eval/policy.yaml
```

The numbers in this file were generated against potent at the time of writing. CI runs the same harness on every push and a regression in best-F1 of more than 0.05 per tool fails the build.
