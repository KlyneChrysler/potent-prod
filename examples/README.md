# Examples

Three integrations, each about 5 minutes to run.

## Prerequisites

- Go 1.23+ with `potent` built and on `PATH`. From the repo root: `go install ./cmd/potent`.
- Python 3.10+.
- An `OPENAI_API_KEY` (only required for `openai-python/`).


| Directory | Stack | Mode |
|-----------|-------|------|
| [`claude-code-mcp/`](./claude-code-mcp) | Claude Code → MCP server | `mcp-stdio` |
| [`langgraph-agent/`](./langgraph-agent) | LangGraph Python agent | `http` |
| [`openai-python/`](./openai-python) | Raw OpenAI tool-calling | `http` |

Each example includes:
- A working policy file
- The exact commands to run potent and the agent
- A demo that triggers a duplicate and shows it being replayed
