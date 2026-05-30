# Benchmarks

Real numbers so operators can do capacity planning. All measurements on an Apple M1, Go 1.23, single-process potent, no profiling overhead. Reproduce with the commands shown.

## Microbenchmarks (`go test -bench`)

```
$ go test -bench=. -benchmem -run=^$ -benchtime=3s ./internal/pipeline/
goos: darwin
goarch: arm64
pkg: github.com/potent/potent/internal/pipeline
cpu: Apple M1
BenchmarkApply_ExactReplay-8    442017      8765 ns/op    1864 B/op    44 allocs/op
BenchmarkApply_ForwardMiss-8    368437      9118 ns/op    2041 B/op    38 allocs/op
BenchmarkAnalyse-8              349761      9134 ns/op    1824 B/op    46 allocs/op
```

What this means at a glance:

| Path | ns/op | Single-thread ceiling |
|------|-------|------------------------|
| Cache hit (replay) | 8.8 µs | ~113k req/s |
| Forward + cache write | 9.1 µs | ~110k req/s |
| Fingerprint compute (pure) | 9.1 µs | ~110k req/s |

Allocation count is dominated by JSON marshal/unmarshal during normalize and canonical-JSON. Reducing it requires zero-alloc JSON which is a v0.3+ optimization, not a v0.1.x correctness concern.

## End-to-end HTTP load test (`hey`)

10,000 POSTs at 100 concurrent against potent in front of a Python upstream with simulated 5 ms latency.

```bash
# upstream (5ms latency, threading server)
python3 docs/eval/load-upstream.py &

# potent
potent -mode http -upstream http://127.0.0.1:18081 -policy http-policy.yaml -store memory &

# hot path: every request hits the same fingerprint
hey -n 10000 -c 100 -m POST \
    -H 'Content-Type: application/json' -H 'X-Potent-Tool: send_email' \
    -d '{"to":"alice@example.com","subject":"hi","body":"y"}' \
    http://127.0.0.1:18080/send

# cold path: every request is a unique fingerprint
hey -n 10000 -c 100 -m POST \
    -H 'Content-Type: application/json' -H 'X-Potent-Tool: send_email' \
    -d '{"to":"u@x","subject":"s","body":"unique-$RANDOM"}' \
    "http://127.0.0.1:18080/send?n={number}"
```

| Scenario | Throughput | p50 | p95 | p99 | Upstream calls |
|----------|------------|-----|-----|-----|----------------|
| Hot cache (all replays) | 6720 req/s | 8 ms | 41 ms | 106 ms | 1 |
| Cold path (all forwards) | 6570 req/s | 8 ms | 38 ms | 69 ms | 10,000 |

Note the hot cache p99 is *worse* than the cold path p99. With 100 concurrent identical fingerprints, the leader-follower coalesce path has thundering-herd characteristics: 99 followers wait on one leader, and the longest waiter pays the leader's full upstream latency plus scheduler jitter. The cold path has no waiters because every fingerprint is unique.

In production this only matters when many concurrent requests pile up on a slow tool call. Mitigations: keep upstream latency low (the leader's wait is the floor), and watch the `coalesced` rate in audit to know when this regime is active.

## Capacity planning

Single instance on a small VM (M1-equivalent, 2-4 vCPU):

- Sustained 5-7k req/s for typical workloads
- Hard ceiling ~110k req/s on pure fingerprint compute (no I/O)
- Each replay saves one full upstream round-trip (typically 50-500 ms in production agent tooling)

Multi-instance with Postgres backend:

- Linear scale up to Postgres connection limit
- pgxpool defaults to a sane pool size; raise via DSN params if needed
- Compactor runs hourly by default; bound the table size with `ttl` per tool

If you need >100k req/s sustained, the bottleneck is JSON allocation and the audit channel, not the pipeline. v0.3+ will address both.

## Reproducing

Microbenchmarks: `go test -bench=. -benchmem -run=^$ -benchtime=3s ./internal/pipeline/`

Load test: see commands above. The upstream script lives at `docs/eval/load-upstream.py`.

CI runs the microbenchmarks on every push but does not gate on them (yet). Track regressions by diffing `go test -bench` output between releases.
