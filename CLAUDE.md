# CLAUDE.md — potent-prod

Project-specific instructions. Overrides global rules where conflict.

## Project

**potent** — a transparent Go gateway that enforces semantic exactly-once semantics for AI agent tool calls. Sits between an agent and a tool server (OpenAI tool calls / MCP / generic HTTP) and dedupes side effects when the LLM phrases a retry differently.

- Remote: `git@github-personal:KlyneChrysler/potent-prod.git`
- Always push via the `github-personal` SSH alias. Never plain `git@github.com:`.
- Default branch: `main`
- License: Apache 2.0
- Go: 1.23

## Go-specific rules

- Accept interfaces, return structs
- Small interfaces (1-3 methods). Define where consumed, not where implemented
- Constructor injection — `NewX(deps...)`. No globals.
- Functional options for server config (`WithAddr`, `WithUpstream`)
- Always wrap errors with context: `fmt.Errorf("normalize %q: %w", field, err)`
- `context.Context` as first arg on every I/O-touching function
- Timeouts on every external call
- `gofmt` + `goimports` + `go vet` + `staticcheck` + `gosec` clean before commit
- `go test -race -cover` mandatory
- Table-driven tests for pure functions
- 80%+ coverage before "done"

## Architecture rules

- **Hexagonal / ports-and-adapters.**
  - Core domain (zero infra deps): `internal/fingerprint`, `internal/policy`, `internal/normalizer`
  - Adapters: `internal/store`, `internal/proxy`, `internal/audit`
  - Adapters depend on core. Core never imports adapters.
- **Immutability by default** — return new values, no in-place mutation
- **Single responsibility per package** — no `utils`/`common` dumping grounds
- Files <400 lines, functions <50 lines, nesting ≤4
- No premature abstraction — concrete first, extract interface when a 2nd caller appears
- Config validated at startup, fail fast
- Structured logging via `log/slog` only — every line carries correlation IDs

## TDD workflow

1. Red — write failing test
2. Green — minimal implementation
3. Refactor — clean up
4. Verify 80%+ coverage

Pure-function packages (`normalizer`, `fingerprint`, `policy`) are written test-first. Adapters are built behind the smallest possible interface, then a fake/in-memory implementation drives the test suite before the real BoltDB/HTTP adapter exists.

## Git workflow

- Conventional commits: `feat:`, `fix:`, `refactor:`, `test:`, `docs:`, `chore:`
- **All commit messages and PR titles/bodies must be lowercase.** No capital letters anywhere in commit subjects, commit bodies, PR titles, or PR descriptions. Code identifiers, file paths, and proper nouns inside backticks stay as written.
- **No em dashes (`—`) or en dashes (`–`) in any commit message or PR text.** Use a period, comma, parentheses, or rewrite the sentence. Regular hyphens (`-`) are fine.
- Feature branches: `klyne/<short-desc>`
- PR template: goal / summary / design decisions / edge cases / test plan
- Commits attributed to Klyne only. No Co-Authored-By.
- **Always open PRs as the `KlyneChrysler` account.** Before `gh pr create`, run `gh auth status` and if the active account is not `KlyneChrysler`, run `gh auth switch --user KlyneChrysler` first. Do not open PRs from `chalory` or any other account.

## Operational

- Prometheus metrics + OTel traces from the first request handler
- Graceful shutdown — drain in-flight, close DB
- No silent failures — every error logged with context
- Boundary validation only (inbound HTTP/JSON). Trust internal types.

## Layout

```
cmd/potent/         # entrypoint
internal/
  policy/           # core: tool policy types + merging
  normalizer/       # core: string transforms
  fingerprint/      # core: canonical JSON + hash + (later) embedding
  store/            # adapter: persistence (memory, BoltDB)
  proxy/            # adapter: HTTP / MCP reverse proxy
  audit/            # adapter: JSONL audit log
configs/policy.yaml # example per-tool policies
docs/ROADMAP.md     # 6-week build plan
```

## What "done" means

A task is done when:
1. Tests written first, passing with race detector
2. ≥80% line coverage on new code
3. `go vet`, `staticcheck`, `gosec` clean
4. Conventional-commit message
5. Self-review against this file's rules
6. Pushed to a feature branch with a PR opened (not merged direct to main)
