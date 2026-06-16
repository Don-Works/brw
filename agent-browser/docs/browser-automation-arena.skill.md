---
name: browser-automation-arena
description: Use when benchmarking a browser-automation tool surface (e.g. agent-browser's MCP) head-to-head against the real Claude-in-Chrome extension, or against each other, on hard real-world interactions — and when diagnosing/fixing why a semantic browser automation thrashes on a specific interaction. Covers the fair-race harness (identical headless `claude -p` agent loops, only the browser surface differs), the the-internet.herokuapp.com + todomvc + saucedemo test-site catalog, token/speed/cost/success metrics, the free zero-token HTTP repro loop for root-causing primitive bugs, and a catalog of real browser-fidelity failure modes (CDP key/mouse delivery, CSS :hover, HTML5 drag, opacity:0 actionability, visual-element surfacing, screenshot bloat).
---

# Browser-automation arena: race two automations, then win

Race a semantic browser-automation surface (e.g. **agent-browser** MCP) against the
**real Claude-in-Chrome** extension on a ladder of hard real-world tasks, measure
**tokens / speed / cost / success**, then use the losses as a fitness function to
find and fix the underlying browser-fidelity bugs.

## The fair-race idea (this is the whole trick)

Drive BOTH competitors with an **identical headless `claude -p` agent loop** — same
model, same prompts, same harness, same banned "cheat" tools. The ONLY variable is
which browser tools the agent holds. Then every delta is attributable to the tool
surface itself.

| Competitor | Surface | Driver |
|---|---|---|
| ours | the MCP under test (semantic refs) | `claude -p --no-chrome --strict-mcp-config --mcp-config ours.json` |
| claude-chrome | real Claude-in-Chrome extension | `claude -p --chrome` |

Proven invocation (both sides), capturing usage/cost/turns + full transcript:

```sh
claude -p "<task prompt>" --output-format stream-json --verbose \
  --model sonnet --dangerously-skip-permissions \
  --disallowedTools "WebFetch,WebSearch,Bash,Read,Write,Edit,Glob,Grep,NotebookEdit,TodoWrite,Task" \
  <surface flags>
```
- ours surface flags: `--no-chrome --strict-mcp-config --mcp-config ours.json`
- claude surface flags: `--chrome --strict-mcp-config --mcp-config '{"mcpServers":{}}'`

Cheat tools are banned so the ONLY way to touch the web is the browser surface.
Parse the `stream-json`: `type=="result"` carries `usage` (input/output/cache
tokens), `total_cost_usd`, `num_turns`, `duration_ms`, `result`; `type=="assistant"`
`tool_use` blocks give the tool-call histogram; `type=="user"` `tool_result` content
bytes are the observation tokens (split image/base64 vs text — screenshots otherwise
swamp the signal). A reference Go implementation lives at `agent-browser/cmd/arena`.

## Setup

1. **Claude CLI** logged in, with the Claude-in-Chrome extension installed.
2. A **persistent daemon** for the ours side, on its OWN isolated Chrome so the two
   competitors never fight over one browser:
   ```sh
   agent-browserd --http 127.0.0.1:17320 --remote-debugging-port 0 \
     --user-data-dir /tmp/arena-ours-chrome
   ```
   `ours.json` points a thin `--upstream-http` MCP proxy at it so Chrome stays warm
   across runs (mirrors Claude reusing an open browser).
3. Run sequentially (the two drive different Chromes; sequential keeps rate limits +
   measurement clean).

## Test-site catalog (public, automation-tolerant, each isolates ONE skill)

`the-internet.herokuapp.com` is the canonical hard-interaction playground:

| Path | Exercises | Oracle |
|---|---|---|
| `/tables` | read a data table | `jsmith@gmail.com` for Smith |
| `/dropdown` | native `<select>` | "Option 2" selected |
| `/checkboxes` | toggle checkboxes | both checked |
| `/key_presses` | keyboard → result (Enter SUBMITS+reloads — adversarial!) | `You entered: ENTER` |
| `/login` | form fill+submit (`tomsmith`/`SuperSecretPassword!`) | "secure area" |
| `/hovers` | CSS `:hover`-gated caption reveal on an avatar `<img>` | `user1` |
| `/dynamic_loading/1` | click + wait for async element | "Hello World!" |
| `/add_remove_elements/` | add/remove DOM + count | 2 Delete buttons |
| `/windows` | open + switch to a new tab/window | "New Window" |
| `/drag_and_drop` | native HTML5 drag-and-drop swap | columns swap A↔B |
| `/horizontal_slider` | set an `<input type=range>` | value 3 |
| `/iframe` | type into a TinyMCE editor inside an iframe | echoed text |

Plus: `todomvc.com/examples/react/dist/` (rerendering SPA: add/complete/count),
`saucedemo.com` (`standard_user`/`secret_sauce` login + add-to-cart SPA),
`en.wikipedia.org` (content read/extract), `duckduckgo.com` (type+submit+dynamic).

Tasks are data-driven: `{id, tier, prompt, expect_any/expect_all}` with a cheap
substring oracle over the agent's final answer. Increase tier = increase difficulty.

## Metrics + reading the result

Lower is better: `turns`, `browser_calls`, `output_tokens`, `semantic_obs_KB`,
`screenshot_KB`, `wall_seconds`, `cost`. Report per-task + aggregate + head-to-head
ratios. **Wall-clock ≈ turns × model-latency**, so fewer turns ⇒ less time — the
headline a semantic surface wins by avoiding screenshot+vision loops.

## Diagnose-and-fix loop (the high-value part)

When ours LOSES or is HIGH-VARIANCE on a task, do NOT iterate via the (expensive,
nondeterministic) agent. **Reproduce the primitive directly against the daemon over
HTTP — zero LLM tokens, deterministic, fast:**

```sh
curl -s localhost:17320/api/browser/open -d '{"url":"<site>"}'
curl -s "localhost:17320/api/page/find?role=image"          # is the target even surfaced?
curl -s localhost:17320/api/page/hover -d '{"ref":"e3"}'    # does the action fire the effect?
curl -s localhost:17320/api/page/evaluate -d '{"expression":"<assert page state>"}'
```
Install an in-page event listener via `evaluate`, fire the action, read what the page
actually received. Fix the Go/CDP/JS primitive, rebuild, re-curl until the primitive
works, THEN re-run the agent bench (multi-trial) to confirm the win is reliable.

## Browser-fidelity failure-mode catalog (what actually breaks)

These are the real bugs a semantic-first automation hits — synthetic JS events and
"it's in the DOM" are NOT enough; the browser only honours real input + real layout:

1. **CDP key presses dropped when the window isn't frontmost.** `Input.dispatchKeyEvent`
   routes through the focused RenderWidgetHost and is silently dropped for a
   background automation Chrome — `insertText` bypasses it, so TYPING works but
   `Enter`/`Tab`/arrows do nothing (React forms never submit). Fix:
   `Emulation.setFocusEmulationEnabled(true)` per target.
2. **Synthetic `mouseover` can't trigger CSS `:hover`.** Only the real cursor position
   does. Dispatch `Input.dispatchMouseEvent(mouseMoved)` to the element centre to
   reveal `:hover`-gated menus/captions/tooltips.
3. **CDP mouse events don't drive HTML5 drag-and-drop.** `draggable=true` widgets need
   the real drag protocol — dispatch in-page `dragstart→drag→dragenter→dragover→drop→
   dragend` carrying ONE shared `DataTransfer`. Fall back to a coordinate drag for
   pointer-based libs (jQuery UI sortable, which is NOT `draggable=true`).
4. **`opacity:0` controls are clickable.** They still receive pointer events (the
   CSS-styled checkbox/radio pattern, e.g. TodoMVC `.toggle`). Only
   `display:none`/`visibility:hidden`/`pointer-events:none` are un-actionable; let a
   hit-test handle occlusion.
5. **Non-interactive visual elements must be surfaced as refs.** `<img>` avatars,
   product tiles, draggable tiles, map pins — a real a11y tree exposes them; a
   semantic snapshot that only emits interactive controls leaves the agent blind, so
   it screenshot-thrashes with high variance. Surface salient visible images
   (`role:image`, size-gated to skip icons/pixels) and `draggable=true` elements.
6. **Range inputs:** set via the native value setter + `input`/`change` events in ONE
   call (`fill`), not N arrow-key presses.
7. **Screenshots balloon to MBs on HiDPI.** Cap the longest side + JPEG for plain
   captures; cap resolution on annotated Set-of-Marks captures (the legend is
   ref-based, so image resolution is purely visual). Otherwise base64 swamps tokens.
8. **MCP `structuredContent` must be a JSON object.** Tools returning a string/array
   (e.g. `evaluate` of `el.textContent`) make strict clients reject the whole result
   ("expected record") → wasteful retries. Only attach it for objects.

## The mcplexer production caveat (fairness)

If ours is mounted as a RAW MCP server to a vanilla `claude -p`, it pays a
**tool-deferral tax**: Claude Code special-cases its own claude-in-chrome integration
and DEFERS all third-party MCP tools (even a 13-tool surface), so the agent spends
~2-3 `ToolSearch` discovery turns/task. This is NOT a count threshold you can beat by
trimming. BUT in real use the surface is reached **through mcplexer's slim 4-tool
surface** (`mcpx__execute_code` + `mcpx__search_tools`) — Claude Code never sees the
N tools, the deferral never fires, and calls batch in one round-trip. Measured: via
mcplexer 3 turns vs raw-mount 5-6 vs claude-chrome 6. So report the raw-mount lane as
the conservative comparison and note the mcplexer path is strictly faster/cheaper.

## Procedure summary

1. Stand up the persistent ours daemon + configs.
2. Define/extend the task ladder (one site per interaction class).
3. Run the head-to-head (`--output-format stream-json`), parse metrics, emit a scorecard.
4. For every loss/high-variance task: free HTTP repro → root-cause against the catalog
   above → fix the primitive → re-curl → multi-trial agent re-run to confirm a RELIABLE
   win (single runs lie; the hard tasks are high-variance).
5. Write up per-task + aggregate + head-to-head; keep raw transcripts as evidence.
