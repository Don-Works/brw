---
id: 01KV5E9BZBFZ81F9229EARDYJV
schema: memory/v1
kind: note
name: HANDOFF agent-browser speed work (resume next session)
workspace: agent-browser
tags:
  - agent-browser
  - handoff
  - resume
  - browser-automation
  - project-fact
pinned: true
source:
  kind: agent
  session_id: 59050316-1ea7-4a5b-9634-46aa6a29fb4f
created_at: 2026-06-15T10:47:25Z
updated_at: 2026-06-15T10:47:25Z
---

RESUME PROMPT for next Claude Code session on agent-browser browser-MCP speed/robustness. Repo /Users/max/github/revitt/brw, branch fix/browser-click-latency-actuation (4 commits ahead of main): 88d5159 fast in-page click path (clickRef was 3x CDP Input.dispatchMouseEvent ~5s -> in-page ClickXYScript 1 round-trip ~tens-ms; ClickXYScript hardened w/ hover+focus; evaluate tolerates void) + void-eval; 791151e browsercheck --bench scorecard (per-action ms+bytes); fc1cd5d+7938a14 browser_read_data generic structured fast-read (__NEXT_DATA__/JSON-LD/microdata/OG). build+test green; merged binary installed to ~/Library/Application Support/agent-browser/bin (daemon restart needed for browser_read_data; click fix already live in running daemon). Tasks: epic 01KV5A3X61WX1RDN0HYDZT2GYT, delivery 01KV5AGZYNKGEQPPDJ149K1X9Y, speed 01KV5CKY1ZVK9TX3NXZPRP5VGE, 7 friction children (3 fixed/review, 4 open: find-visibility, transport-unify, cache-version-bump, post-action-effect-confirm). NEXT: (1) live-verify 5s->ms click on Decathlon Dry+ shorts page (prior session's mcplexer route wedged after mid-session daemon kill; fresh session re-dials clean); (2) apply in-page fast-click to internal/browser Manager.Click (bench shows ~0.8-1.1s/click direct-CDP too vs 1-2ms reads); (3) restart daemon + live-smoke browser_read_data on a real product page; (4) network XHR observe/replay = next 10-100x lever. REAL Chrome via bridge: visible, never purchase, never fabricate user PII.
