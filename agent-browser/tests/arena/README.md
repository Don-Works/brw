# Arena — agent-browser vs Claude-in-Chrome, head to head

`arena` races our browser-automation tool surface against the real **Claude in
Chrome** extension on the same ladder of increasingly difficult tasks, and
reports **token consumption, speed, round-trips, and success** for each.

## The idea: hold everything constant except the browser tool surface

Both competitors are driven by an **identical headless `claude -p` agent loop**
(same model, same harness, same banned "cheat" tools). The only thing that
differs is which browser tools the agent is handed:

| Competitor      | Browser surface                                   | How it's mounted |
|-----------------|---------------------------------------------------|------------------|
| `ours`          | `agent_browser` MCP (semantic refs, batch, find)  | `claude -p --no-chrome --strict-mcp-config --mcp-config ours-mcp.json` |
| `claude-chrome` | the real Claude-in-Chrome extension               | `claude -p --chrome` |

Because the model, the prompts, and the agent loop are the same on both sides,
the measured deltas in tokens / turns / wall-clock / success are attributable to
the **browser tool surface itself** — which is exactly the variable we control
and want to refine.

Cheat tools (`Bash`, `WebFetch`, `WebSearch`, `Read`, …) are disabled on both
sides via `--disallowedTools`, so the only way to satisfy a task is to actually
drive a browser. Token/cost/turn figures come from Claude Code's own per-run
`usage` accounting (`--output-format stream-json`); the raw transcript of every
run is saved for evidence.

## Prerequisites

1. **Claude Code CLI** on `PATH`, logged in, with the Claude-in-Chrome extension
   installed in your Chrome (so `claude --chrome` can drive it).
2. **A persistent `agent-browserd`** for the `ours` side, running its own
   isolated Chrome via direct CDP (so the two competitors never fight over one
   browser):

   ```sh
   agent-browserd --http 127.0.0.1:17320 \
     --remote-debugging-port 0 \
     --user-data-dir /tmp/arena-ours-chrome
   ```

   `ours-mcp.json` points a thin `--upstream-http` MCP proxy at this daemon, so
   Chrome stays warm across runs (mirroring Claude reusing an already-open
   browser).

## Run

```sh
go build -o bin/arena ./cmd/arena

# full ladder, sonnet on both sides
./bin/arena

# just the easy rungs, or a single task, or 3 trials each
./bin/arena -tier-max 2
./bin/arena -only ddg-search
./bin/arena -trials 3 -model opus
```

Flags: `-ladder`, `-model`, `-trials`, `-ours-mcp`, `-ours-health`,
`-results-dir`, `-only`, `-tier-max`, `-timeout`.

## Output

Each invocation writes a timestamped folder under `tests/arena/results/<stamp>/`:

- `runs.jsonl` — one JSON line of metrics per (competitor, task, trial).
- `<task>.<competitor>.t<N>.jsonl` — the full Claude Code stream transcript.
- `scorecard.md` — per-task table, totals, head-to-head deltas, and findings.

Metrics per run: `pass`, `num_turns`, `browser_calls`, `toolsearch_calls`,
`output_tokens`, `observation_bytes`, `cache_read/creation_tokens`, `cost_usd`,
`wall_ms`, and the agent's final answer.

## The task ladder

`ladder.json` defines public, no-auth sites (so the two different Chrome
profiles stay comparable) in increasing difficulty:

1. **read-static** — read example.com's H1.
2. **wiki-extract** — pull a fact from a content-heavy Wikipedia page.
3. **ddg-search** — type a query, submit, read dynamic results.
4. **form-submit** — fill + submit a multi-field form, read the echoed response.
5. **todomvc-spa** — add/complete items in a rerendering React SPA, read the count.

Each task has a cheap success oracle (`expect_any` / `expect_all` substring
match on the agent's final answer).

## Fairness caveats (read the scorecard's Notes section)

- `ours` drives a **fresh, isolated** Chrome (temp profile, no cookies/logins);
  `claude-chrome` drives the **installed** profile (logged-in). Cookie-wall and
  auth-gated steps favor `claude-chrome`.
- Today, our 48-tool MCP surface trips Claude Code **tool-deferral**, so the
  `ours` agent spends extra `ToolSearch` round-trips discovering tools before it
  can act. This is a real, actionable ergonomics finding (lean the surface), but
  it is partly a harness artifact — `search_calls` is reported separately so you
  can see it.
- `cache_read` / `cache_creation` token counts fluctuate with prompt-cache
  warmth between runs; prefer `turns`, `output_tokens`, and `observation_bytes`
  for stable surface comparison.
