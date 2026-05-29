"""LangGraph agent that retries the send_email node once. Without potent in
front of TOOL_URL, the retry duplicates the side effect. With potent, it
replays from cache and the upstream is hit exactly once.
"""

import json
import os
import sys
import urllib.request
from typing import TypedDict

from langgraph.graph import END, StateGraph

TOOL_URL = os.environ.get("TOOL_URL", "http://localhost:8080")


class State(TypedDict):
    to: str
    subject: str
    body: str
    result: dict
    attempts: int


def send_email(state: State) -> State:
    state["attempts"] = state.get("attempts", 0) + 1

    args = {"to": state["to"], "subject": state["subject"], "body": state["body"]}
    req = urllib.request.Request(
        TOOL_URL + "/",
        data=json.dumps(args).encode(),
        headers={"Content-Type": "application/json", "X-Potent-Tool": "send_email"},
        method="POST",
    )
    with urllib.request.urlopen(req) as resp:
        status = resp.getheader("X-Potent-Status", "n/a")
        match = resp.getheader("X-Potent-Match", "")
        print(f"[attempt {state['attempts']}] potent status={status} match={match}", file=sys.stderr)
        state["result"] = json.loads(resp.read())

    # Force a "transient failure" on the first attempt to exercise retry.
    if state["attempts"] == 1:
        raise RuntimeError("simulated transient failure to trigger retry")

    return state


def retry_send_email(state: State) -> State:
    try:
        return send_email(state)
    except RuntimeError as e:
        print(f"retrying after: {e}", file=sys.stderr)
        return send_email(state)


def main() -> None:
    g = StateGraph(State)
    g.add_node("send", retry_send_email)
    g.set_entry_point("send")
    g.add_edge("send", END)
    app = g.compile()

    out = app.invoke({
        "to": "alice@example.com",
        "subject": "Q3 report",
        "body": "Q3 financial summary attached.",
        "attempts": 0,
        "result": {},
    })
    print("final:", out["result"])


if __name__ == "__main__":
    main()
