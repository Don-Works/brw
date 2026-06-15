---
id: 01KV2J1FW1BCEY39FF53Z44XCH
schema: task/v1
workspace: agent-browser
title: Expose console, network, download, and artifact observation primitives
status: done
priority: high
tags:
  - diagnostics
  - network
  - downloads
  - mcp
  - claude-parity
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:55:18.273741Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:32:09.447875Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:35:00.282629Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:55:18Z
updated_at: 2026-06-14T08:35:00Z
---

Claude appears to expose READ_CONSOLE_MESSAGES, READ_NETWORK_REQUESTS, downloads, screenshots, and GIF/export flows. Add first-class MCP diagnostics for console logs, network request summaries, download tracking/results, and artifact capture without making screenshots the primary state model.
