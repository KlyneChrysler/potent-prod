# OpenAI Python + potent (HTTP mode)

Drop potent in front of any HTTP tool server. Your OpenAI tool-calling agent points its tool URL at potent instead of the upstream server, and duplicate side effects vanish.

## Layout

```
agent.py        # OpenAI tool-calling loop that executes tools via HTTP
tool_server.py  # The actual tool (a tiny Flask app that "sends emails")
policy.yaml     # potent's idempotency policy
```

## Run

```bash
# Terminal 1: the tool
python tool_server.py
# → :8000

# Terminal 2: potent in front of the tool
potent -mode http -addr :8080 -upstream http://localhost:8000 -policy policy.yaml

# Terminal 3: the agent (point it at potent, not the upstream)
export OPENAI_API_KEY=sk-...
export TOOL_URL=http://localhost:8080
python agent.py "Send Q3 report to alice@example.com"

# Run it again with slightly different wording
python agent.py "send q3 report to ALICE@example.com"
# → second run replays, tool_server.py only receives ONE request
```

## What you should see

`tool_server.py` prints exactly one "sending email" line per unique intent, even when the LLM phrases the request differently each turn.

`X-Potent-Status: replayed` and `X-Potent-Match: semantic` (or `exact`) appear on the deduped responses.
