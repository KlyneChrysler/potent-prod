# Architecture

## What potent is

potent is a transparent gateway that sits between an AI agent and a tool server (HTTP, MCP Streamable HTTP, or MCP stdio) and deduplicates semantically equivalent tool calls. It normalizes intent, fingerprints it, and either replays a cached response or forwards the call and remembers the answer.

## Request lifecycle

```
 agent
   |
   v
 +-----------------+
 | proxy handler   |  recover panics, enforce body cap, attach correlation id
 +-----------------+
   |
   v
 +-----------------+
 | normalize       |  per-tool string transforms from policy.yaml
 +-----------------+
   |
   v
 +-----------------+
 | fingerprint     |  canonical JSON + BLAKE3 + embedding
 +-----------------+
   |
   v
 +-----------------+
 | store lookup    |  exact hash hit, then semantic cosine search
 +-----------------+
   |
   +---- hit ------> replay cached response, headers: X-Potent-Status=replayed
   |
   +---- in flight -> coalesce (follower waits on leader, bounded by -stdio-request-timeout)
   |
   +---- miss -----> forward to upstream, persist response, headers: X-Potent-Status=fresh
                       |
                       v
                  +-----------------+
                  | audit           |  append JSONL decision record
                  +-----------------+
                       |
                       v
                   response to agent
```

## Packages

Hexagonal layout. Core has zero infra deps; adapters depend on core.

- Core: `internal/policy`, `internal/normalizer`, `internal/fingerprint`.
- Adapters: `internal/store` (memory, bolt), `internal/proxy` (http, mcp-http, mcp-stdio), `internal/audit`.

## Modes

- `http`. Generic reverse proxy. Tool name is read from the `X-Potent-Tool` request header. Use this in front of any HTTP tool server.
- `mcp-http`. MCP Streamable HTTP. potent intercepts JSON-RPC `tools/call` frames and passes everything else through unchanged.
- `mcp-stdio`. potent spawns the upstream MCP server as a child process and proxies JSON-RPC frames over stdio. stdout is the protocol; all logs go to stderr.

## Coalescing

If two requests with the same fingerprint arrive while the first is still in flight, the second becomes a follower. The leader executes upstream once; the follower receives the leader's response. This works for the generic pipeline (`http`, `mcp-http`) and for `mcp-stdio`. Followers waiting longer than `-stdio-request-timeout` are released with an error rather than blocking indefinitely.

## Durability

With `-store bolt`, fingerprints and cached responses are persisted to a BoltDB file. A background compactor (interval `-bolt-compact-interval`) sweeps expired entries from disk. The leader timeout sweeper runs alongside it and releases waiters whose leader exceeded the timeout, so a hung upstream cannot cause unbounded goroutine growth.
