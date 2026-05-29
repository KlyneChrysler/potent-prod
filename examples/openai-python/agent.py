"""Minimal OpenAI tool-calling agent. Executes send_email by POSTing to TOOL_URL.

Point TOOL_URL at potent (default :8080), not the upstream tool server.
"""

import json
import os
import sys
import urllib.request

from openai import OpenAI

TOOL_URL = os.environ.get("TOOL_URL", "http://localhost:8080")

TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "send_email",
            "description": "Send an email to a recipient.",
            "parameters": {
                "type": "object",
                "properties": {
                    "to": {"type": "string"},
                    "subject": {"type": "string"},
                    "body": {"type": "string"},
                },
                "required": ["to", "subject", "body"],
            },
        },
    }
]


def call_tool(name: str, args: dict) -> dict:
    req = urllib.request.Request(
        TOOL_URL + "/",
        data=json.dumps(args).encode(),
        headers={
            "Content-Type": "application/json",
            "X-Potent-Tool": name,  # potent reads this to look up the policy
        },
        method="POST",
    )
    with urllib.request.urlopen(req) as resp:
        status = resp.getheader("X-Potent-Status", "n/a")
        match = resp.getheader("X-Potent-Match", "")
        sim = resp.getheader("X-Potent-Similarity", "")
        print(f"[potent] status={status} match={match} sim={sim}", file=sys.stderr)
        return json.loads(resp.read())


def main() -> None:
    if len(sys.argv) < 2:
        print("usage: agent.py <user request>")
        sys.exit(1)

    user_msg = sys.argv[1]
    client = OpenAI()

    messages = [
        {"role": "system", "content": "You are an assistant that sends emails when asked."},
        {"role": "user", "content": user_msg},
    ]

    resp = client.chat.completions.create(model="gpt-4o-mini", messages=messages, tools=TOOLS)
    msg = resp.choices[0].message
    if not msg.tool_calls:
        print(msg.content)
        return

    for tc in msg.tool_calls:
        args = json.loads(tc.function.arguments)
        result = call_tool(tc.function.name, args)
        print(f"tool result: {result}")


if __name__ == "__main__":
    main()
