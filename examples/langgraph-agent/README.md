# LangGraph + potent (HTTP mode)

Same shape as the OpenAI example, but the agent is a LangGraph state machine. LangGraph retries failed nodes; without potent, the retry sends the email twice. With potent in front of the tool URL, the retry is deduped.

## Layout

```
graph.py         # LangGraph agent with a send_email node
policy.yaml      # potent's idempotency policy
requirements.txt # pinned Python deps (langgraph only; no LLM call in this example)
```

The tool server is shared with `../openai-python/tool_server.py`. Copy it in before running:

```bash
cp ../openai-python/tool_server.py .
```

## Run

```bash
pip install -r requirements.txt
python tool_server.py &
TOOL_PID=$!

potent -mode http -addr :8080 -upstream http://localhost:8000 -policy policy.yaml &
POTENT_PID=$!

export TOOL_URL=http://localhost:8080
python graph.py
```

`graph.py` makes no LLM call, so no `OPENAI_API_KEY` is needed.

The `graph.py` script intentionally triggers a retry by raising the first time through the tool node. Without potent, you'd see two upstream calls. With potent, the second is replayed.

## Cleanup

```bash
kill $TOOL_PID $POTENT_PID
```

## Why this matters for LangGraph

LangGraph (and most agent frameworks) make retries trivial. That's the point. But retries on side-effecting tool calls are dangerous unless the upstream is idempotent. Potent makes the upstream effectively idempotent without your tool's cooperation.
