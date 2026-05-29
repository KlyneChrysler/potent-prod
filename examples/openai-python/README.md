# OpenAI Python + potent (HTTP mode)

Drop potent in front of any HTTP tool server. Your OpenAI tool-calling agent points its tool URL at potent instead of the upstream server, and duplicate side effects vanish.

## Layout

```
agent.py         # OpenAI tool-calling loop that executes tools via HTTP
tool_server.py   # The actual tool (a tiny stdlib HTTP server that "sends emails")
policy.yaml      # potent's idempotency policy
requirements.txt # pinned Python deps
```

## Run

```bash
pip install -r requirements.txt

# Terminal 1: the tool
python tool_server.py
# listens on :8000

# Terminal 2: potent in front of the tool
potent -mode http -addr :8080 -upstream http://localhost:8000 -policy policy.yaml

# Terminal 3: the agent (point it at potent, not the upstream)
export OPENAI_API_KEY=sk-...
export TOOL_URL=http://localhost:8080
python agent.py "Send Q3 report to alice@example.com"

# Run it again with slightly different wording
python agent.py "send q3 report to ALICE@example.com"
# second run replays. tool_server.py only receives ONE request.
```

## Expected output

After the first run you should see one line on `tool_server.py` stderr:

```
[tool_server] call #1: send_email to='alice@example.com'
```

After the second run, no new `tool_server` line appears. `agent.py` stderr instead shows:

```
[potent] status=replayed match=semantic sim=0.97
```

`X-Potent-Status: replayed` and `X-Potent-Match: semantic` (or `exact`) appear on the deduped responses.

## Cleanup

Stop each terminal with `Ctrl+C`. If you ran potent with `-store bolt`, remove `potent.db`.
