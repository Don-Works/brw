#!/usr/bin/env python3
"""Measure the token cost of the brw MCP tool catalogue, per profile.

The catalogue is re-sent to the model on every request, so its size is a fixed
tax on every turn an agent takes rather than a one-off. The README and
docs/agent-guide.md quote these numbers; re-run this after changing a tool
description or a profile so the docs do not drift.

    go build -o bin/brwd ./cmd/brwd
    python3 scripts/measure-tool-catalogue.py [--detail]

--detail also ranks the individual tools by size, which is where to look first
when the total moves.
"""
import json
import os
import subprocess
import sys

BRWD = os.environ.get("BRWD", "bin/brwd")
PROFILES = ("all", "core", "minimal")
INIT = '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}\n'
LIST = '{"jsonrpc":"2.0","id":2,"method":"tools/list"}\n'

# Rough characters-per-token for English prose plus JSON punctuation. Good
# enough to compare profiles and track a change; not a tokenizer.
CHARS_PER_TOKEN = 4


def catalogue(profile):
    result = subprocess.run(
        [BRWD, "--mcp", "--mcp-tools", profile, "--http", "off"],
        input=INIT + LIST, capture_output=True, text=True, timeout=120,
    )
    lines = [l for l in result.stdout.splitlines() if '"tools":[' in l]
    if not lines:
        sys.exit(f"{BRWD} returned no tools/list for profile {profile}:\n{result.stderr[-2000:]}")
    return json.loads(lines[-1])["result"]["tools"]


def size_of(tool):
    return len(tool["description"]) + len(json.dumps(tool["inputSchema"]))


def main():
    detail = "--detail" in sys.argv
    if not os.path.exists(BRWD):
        sys.exit(f"{BRWD} not found — run: go build -o {BRWD} ./cmd/brwd")

    print(f"{'profile':9} {'tools':>5} {'desc':>7} {'schema':>7} {'~tokens':>8}")
    for profile in PROFILES:
        tools = catalogue(profile)
        desc = sum(len(t["description"]) for t in tools)
        schema = sum(len(json.dumps(t["inputSchema"])) for t in tools)
        tokens = (desc + schema) // CHARS_PER_TOKEN
        print(f"{profile:9} {len(tools):5} {desc:7} {schema:7} {tokens:8}")

        if detail and profile == "all":
            for tool in sorted(tools, key=size_of, reverse=True)[:12]:
                print(f"    {tool['name']:26} {size_of(tool):6}")


if __name__ == "__main__":
    main()
