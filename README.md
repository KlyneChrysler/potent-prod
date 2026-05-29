# potent

**Stops your AI agent from doing the same thing twice.**

Like sending the same email. Charging a card twice. Deleting a record you already deleted. The kind of bug that's hard to spot until a customer is angry.

## Why this happens

AI agents retry. That's what they do. When something feels stuck, they try again. And the LLM doesn't write the retry exactly the same way it wrote the first attempt.

So your tool server sees this:

```json
// first try
{ "to": "alice@example.com", "subject": "Q3 report", "body": "Hi Alice..." }

// second try (one minute later)
{ "to": " Alice@Example.com ", "subject": "Q3 Report", "body": "Hi Alice..." }
```

Same intent. Different bytes. Your normal idempotency keys don't catch it. Two emails go out.

## What potent does

It sits between your agent and your tool server. For every call, it:

1. Cleans up the arguments (lowercase emails, trim whitespace, the boring stuff)
2. Checks if it's seen this same intent before
3. If yes, replays the cached response and never bothers your tool
4. If no, forwards the call and remembers the answer for next time

That's the whole idea.

## Use it in 3 commands

```bash
# 1. Pull the image
docker pull ghcr.io/klynechrysler/potent:v0.1.0

# 2. Write a tiny policy file
cat > policy.yaml <<EOF
tools:
  send_email:
    mode: strict
    ttl: 24h
    fingerprint_fields: [to, subject, body]
EOF

# 3. Run it in front of your tool server
docker run -p 8080:8080 -v $(pwd)/policy.yaml:/configs/policy.yaml \
  ghcr.io/klynechrysler/potent:v0.1.0 \
  -upstream http://your-tool-server:8000
```

Now your agent talks to `http://localhost:8080` instead of your tool server. It adds one header so potent knows which tool is being called:

```
X-Potent-Tool: send_email
```

That's it. Your agent doesn't need an SDK. Your tool server doesn't need to change. You wrote 5 lines of YAML.

## What you'll see

```bash
# first call
curl -X POST http://localhost:8080/ \
  -H "X-Potent-Tool: send_email" \
  -d '{"to":"alice@example.com","subject":"Q3","body":"hi"}'

# response header: X-Potent-Status: fresh
# the email gets sent
```

```bash
# same call again, different casing and spacing
curl -X POST http://localhost:8080/ \
  -H "X-Potent-Tool: send_email" \
  -d '{"to":" Alice@Example.com ","subject":"Q3","body":"hi"}'

# response header: X-Potent-Status: replayed
# the email does NOT get sent again. potent returns the first response.
```

## Other ways to install

If you don't want Docker:

```bash
# Mac (Apple Silicon)
curl -L https://github.com/KlyneChrysler/potent-prod/releases/download/v0.1.0/potent_0.1.0_darwin_arm64.tar.gz | tar xz

# Mac (Intel)
curl -L https://github.com/KlyneChrysler/potent-prod/releases/download/v0.1.0/potent_0.1.0_darwin_amd64.tar.gz | tar xz

# Linux (amd64)
curl -L https://github.com/KlyneChrysler/potent-prod/releases/download/v0.1.0/potent_0.1.0_linux_amd64.tar.gz | tar xz

# Linux (arm64)
curl -L https://github.com/KlyneChrysler/potent-prod/releases/download/v0.1.0/potent_0.1.0_linux_arm64.tar.gz | tar xz

# Or build from source
go install github.com/KlyneChrysler/potent-prod/cmd/potent@latest
```

## Works with what you already use

Potent has three modes for three common setups:

| Mode | Use this if |
|------|-------------|
| `http` | Your tool server speaks HTTP |
| `mcp-http` | You're using MCP over HTTP (Claude clients, Cursor) |
| `mcp-stdio` | You're using MCP over stdio (Claude Code, most local agents) |

Pick the mode with `-mode http` or `-mode mcp-http` or `-mode mcp-stdio`. The dedup logic is the same. Only the protocol changes.

Working examples for OpenAI tool calling, Claude Code MCP, and LangGraph are in [`examples/`](./examples).

## The policy file in 30 seconds

```yaml
tools:
  send_email:
    mode: strict              # block duplicates, replay cached response
    ttl: 24h                  # forget after a day
    normalize:                # clean up before comparing
      to: [lowercase, trim]
      subject: [trim, collapse_whitespace]
    fingerprint_fields: [to, subject, body]
    semantic_threshold: 0.9   # also catch near duplicates (different wording, same intent)

  charge_card:
    mode: strict
    require_human_confirm_on_replay: true   # never silently replay money

  search_web:
    mode: cache               # read only tools can replay freely
    ttl: 5m

  log_event:
    mode: off                 # skip potent for this one
```

Four modes per tool:

- `strict`: block duplicates, replay the cached response. Use this for anything with a side effect.
- `cache`: cache and replay freely. Use this for read only tools (search, lookups).
- `log_only`: detect duplicates and log them, but always forward. Use this when you want to learn before you enforce.
- `off`: skip potent entirely.

## When you want more

These are off by default. Turn them on when you need them.

**Persist across restarts:**
```bash
potent ... -store bolt -db /var/lib/potent/cache.db
```

**Audit log (every decision, JSONL):**
```bash
potent ... -audit-log /var/log/potent/audit.jsonl
```

**Prometheus metrics:**
Already on. Scrape `http://localhost:9090/metrics`.

**Inspect or evict cache entries at runtime:**
```bash
export POTENT_ADMIN_TOKEN=$(openssl rand -hex 32)
potent ... -admin-addr 127.0.0.1:9095

# then
curl -H "Authorization: Bearer $POTENT_ADMIN_TOKEN" \
  http://127.0.0.1:9095/stats?tool=send_email
```

Full reference in [docs/operating.md](./docs/operating.md).

## Deploy it

A hardened Kubernetes manifest is in [`deploy/k8s/potent.yaml`](./deploy/k8s/potent.yaml). It runs as a non root user, drops every Linux capability, uses a read only root filesystem, and pins to a versioned image.

```bash
kubectl apply -f https://github.com/KlyneChrysler/potent-prod/raw/main/deploy/k8s/potent.yaml
```

## Why Go

You don't want a sidecar that adds 50ms to every tool call. Potent is built to add under 5ms on a cache hit. Single static binary, no runtime to install, no Python environment to fight, no NPM tree to audit. The whole thing is under 10MB.

## Honest about what's pre 1.0

This is v0.1.0. It works and the tests prove it, but real users will find bugs the tests didn't.

- The semantic matcher is a hashing TF-IDF embedder. It catches normalized duplicates great. When you need true transformer semantics, the ONNX swap is a one file change behind the `Embedder` interface.
- The cosine search is brute force over the per tool cache. Fine until your per tool cache hits 10k entries, then we'll add an ANN index.
- No automatic policy suggestion from MCP `tools/list` schemas yet. Coming.

If you hit something that doesn't work, open an issue. That's the entire deal of "pre 1.0".

## License

Apache 2.0. Use it, fork it, ship it.
