---
id: 01KV0BDPAPFV2H1MVZT8SQEWAS
schema: task/v1
workspace: agent-browser
title: Add first-class browser_batch transaction primitive
status: done
priority: high
tags:
  - mcp
  - browser-speed
  - batching
  - token-efficiency
pinned: false
assignee:
  origin_kind: local
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
source:
  kind: agent
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
status_history:
  - at: 2026-06-13T11:21:09.206293Z
    evt: created
    to: open
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:21:09.206293Z
    evt: assigned
    to: f8ed800b-c98d-4403-a5ba-ea03a0447722
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-14T08:35:00.319871Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:36:49.388082Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-13T11:21:09Z
updated_at: 2026-06-14T08:36:49Z
---

Build a compact MCP primitive that executes ordered browser ops in one daemon call: find/fill/select/click/press/wait/read, returning a compact per-step result and final observation. This complements mcplexer JS batching by eliminating browser round trips for common forms and checkout flows.
