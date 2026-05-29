# Claude Code + potent (MCP stdio mode)

Wrap any MCP server with potent so Claude Code can't accidentally trigger the same side-effect twice.

## Layout

```
fake_mcp_server.py    # A toy MCP server that "sends emails" over stdio
policy.yaml           # potent's idempotency policy
claude-mcp.json       # Drop into ~/.config/claude/mcp.json
```

## Wire it up

Edit your Claude Code MCP config (typically `~/.config/claude/mcp.json` or `~/Library/Application Support/claude/mcp.json`):

```json
{
  "mcpServers": {
    "email": {
      "command": "potent",
      "args": [
        "-mode", "mcp-stdio",
        "-policy", "/absolute/path/to/policy.yaml",
        "-store", "bolt",
        "-db", "/absolute/path/to/potent.db",
        "-upstream", "python /absolute/path/to/fake_mcp_server.py"
      ]
    }
  }
}
```

What this says: when Claude Code wants to talk to the `email` MCP server, it spawns `potent` instead. Potent in turn spawns `fake_mcp_server.py` as a child and proxies JSON-RPC frames bidirectionally — except for `tools/call`, which gets the idempotency check first.

## Demonstrate

Restart Claude Code, then in a chat:

> Send Q3 report to alice@example.com via the email tool.

Followed by:

> Actually send that Q3 report to Alice at alice@example.com again, just to make sure.

The second message should result in a `tools/call` that potent recognizes as a duplicate and replays from cache — the upstream Python server never receives it.

You can verify by tailing `potent.db` size and checking the audit log if you wired `-audit-log`.
