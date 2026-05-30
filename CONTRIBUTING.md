# contributing to potent

thanks for your interest. potent is open source but the `main` branch is curated. this doc tells you what merges and what does not.

## ground rules

1. **open an issue before a pr** for anything beyond a typo or a one-line bug fix. unsolicited large prs will be closed, not because the work isn't good, but because direction needs to be agreed first.
2. **no force pushes, no rewriting history on `main`.** branch protection enforces this; do not ask for it to be turned off.
3. **scope discipline.** one logical change per pr. drive-by refactors get split out or asked to be removed.
4. **lowercase commit messages and pr titles.** no em dashes. see `claude.md` for the exact rules the maintainer follows.

## what gets accepted quickly

- bug fixes with a failing test that the fix turns green
- documentation corrections (typos, broken links, clarifications)
- new adapter implementations behind existing interfaces (e.g. a new audit sink, a new store backend) that follow the existing `internal/audit/s3.go` shape
- security fixes (please file privately first: see `security.md`)

## what gets reviewed slowly or declined

- new public api surface (new flags, new exported types, new policy fields). these change the operator contract and need design buy-in first.
- changes to the fingerprint algorithm, normalizer rules, or coalesce semantics. these affect correctness across every existing deployment.
- new dependencies. the bar is high. justify what you cannot do with the standard library.
- features without a corresponding test. coverage is enforced.
- silent behavior changes. anything that changes a default goes in `changelog.md` under `### Changed`.

## the pr bar

before opening a pr, your branch must:

```
go build ./...
go test ./... -race -count=1
go vet ./...
staticcheck ./...           # if installed
gosec -quiet ./...           # if installed
```

all of these must be clean. ci runs them again and will block merge if anything fails.

new code needs:
- tests in the same package (`*_test.go`), table-driven where possible
- at least 80 percent line coverage on the new file
- a `changelog.md` entry under `## [Unreleased]`
- public types and functions documented with a godoc comment

the pr template asks for goal, summary, design decisions, edge cases, files changed, and test plan. fill all of it. prs missing any section get sent back.

## branch and commit format

- branches: `<your-handle>/<short-desc>` (e.g. `alice/fix-bolt-leak`)
- commit subject: `<type>: <description>`, lowercase, imperative, under 72 chars
- types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`, `perf`, `ci`
- no `co-authored-by:` lines unless you actually pair-programmed with someone

## what happens to your pr

1. ci runs build + test + lint. red ci means the pr will not be reviewed.
2. `codeowners` triggers a review request. the maintainer is the sole code owner.
3. you may get inline review comments. resolve them or push back with reasoning.
4. once approved with green ci, the maintainer squash-merges. you do not need merge permission and will not receive it.

## what will not happen

- you will not be granted write access to the repo
- your fork will not be auto-merged
- branch protection on `main` will not be relaxed for your pr
- `main` will never be force-pushed

## security issues

do not open a public issue or pr for a security vulnerability. see `security.md` for the private reporting path.

## license

by contributing you agree your contribution is licensed under apache 2.0, the same license as the rest of the project. there is no separate cla.
