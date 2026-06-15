---
id: 01KV18MXNNG4Z46YRRRH06HE4R
schema: task/v1
workspace: agent-browser
title: Expose downstream route health and restart diagnostics in mcplexer
status: open
priority: high
tags:
  - mcplexer
  - downstream
  - diagnostics
  - agent-browser
  - reliability
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by: 01KV18NXKHVJYSB50GYPGFGDJ5
source:
  kind: agent
  session_id: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
status_history:
  - at: 2026-06-13T19:51:54.805729Z
    evt: created
    to: open
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
created_at: 2026-06-13T19:51:54Z
updated_at: 2026-06-13T19:52:27Z
---

During max-air checkout work, agent_browser MCP calls returned 'no response from downstream' while the remote agent-browser HTTP daemon and extension bridge were healthy. The agent had no exposed mcplexer admin route to inspect/restart/rebind just that downstream route, so it had to use an SSH tunnel to HTTP. Generic mcplexer improvement: surface downstream instance health, last stderr/log excerpt, command/config, and a safe restart/reconnect action where permitted. Also make tool errors distinguish daemon down, route down, timeout, and tool schema errors.

## Notes
- 2026-06-15 (agent): REPRO (2026-06-15): killing a downstream stdio MCP daemon mid-session (agent-browser, to deploy a rebuilt binary) wedges mcplexer's per-session EXECUTE route with 'failed to write msg: failed to acquire lock: context canceled'. mcpx.reload_server re-introspects the catalog and even respawns the daemon process (confirmed new PID running), but the execute/call connection never re-dials — stays wedged for the whole session. Gap: downstream death should drop+recreate the execute connection, not just the introspection one. Recovery today = new session or gateway restart. Strong motivation for downstream route health + restart diagnostics + auto re-dial.
