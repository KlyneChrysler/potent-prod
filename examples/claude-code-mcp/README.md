# Claude Code + potent (MCP stdio mode)

Wrap any MCP server with potent so Claude Code can't accidentally trigger the same side-effect twice.

## Layout

```
fake_mcp_server.py   # A toy MCP server that "sends emails" over stdio
policy.yaml          # potent's idempotency policy
claude-mcp.json      # Sample mcpServers snippet to merge into your Claude Code config
```

## Wire it up

Claude Code reads MCP servers from `~/.claude.json` (user scope) or a project-local `.mcp.json`. The Claude Desktop paths (`~/Library/Application Support/Claude/claude_desktop_config.json` on macOS) are a different file; if you use both apps, configure each separately.

Merge the `mcpServers.email` block from `claude-mcp.json` into your chosen config, replacing the `/absolute/path/to/...` placeholders:

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

What this says: when Claude Code wants to talk to the `email` MCP server, it spawns `potent` instead. Potent in turn spawns `fake_mcp_server.py` as a child and proxies JSON-RPC frames bidirectionally, except for `tools/call`, which gets the idempotency check first.

## Demonstrate

Restart Claude Code, then in a chat:

> Send Q3 report to alice@example.com via the email tool.

Followed by:

> Actually send that Q3 report to Alice at alice@example.com again, just to make sure.

The second message should result in a `tools/call` that potent recognizes as a duplicate and replays from cache. The upstream Python server never receives it.

You can verify by tailing `potent.db` size and checking the audit log if you wired `-audit-log`.
