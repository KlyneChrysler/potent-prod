# Roadmap

## Week 1 — Spike + measurement
- [x] Repo scaffold
- [ ] HTTP reverse proxy skeleton
- [ ] bge-small ONNX integration spike
- [ ] Benchmark: <5ms p99 on cache hit, <20ms on embed miss
- [ ] Decision gate: hash-only fallback if embed too slow

## Week 2 — Core engine
- [ ] Canonical JSON normalizer
- [ ] BLAKE3 exact-match path
- [ ] BoltDB store
- [ ] Policy YAML loader + validation
- [ ] Unit tests (Unicode, nested, arrays)

## Week 3 — Semantic tier
- [ ] Embedding cache layer
- [ ] Cosine similarity search (brute force <10k)
- [ ] Replay decision pipeline
- [ ] Response headers: X-Potent-Status, X-Potent-Similarity

## Week 4 — MCP integration
- [ ] Streamable HTTP MCP proxy
- [ ] stdio MCP proxy
- [ ] MCP test suite conformance
- [ ] Auto-suggest policy from tools/list schemas

## Week 5 — Ops + audit
- [ ] Prometheus metrics
- [ ] JSONL audit log (file + S3)
- [ ] Admin HTTP API
- [ ] Docker image + k8s manifest

## Week 6 — Polish + launch
- [ ] mkdocs site
- [ ] 3 example deployments
- [ ] Demo video
- [ ] HN / Lobsters / r/LocalLLaMA launch
