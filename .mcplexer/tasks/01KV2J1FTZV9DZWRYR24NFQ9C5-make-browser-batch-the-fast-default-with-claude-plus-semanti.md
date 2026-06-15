---
id: 01KV2J1FTZV9DZWRYR24NFQ9C5
schema: task/v1
workspace: agent-browser
title: Make browser_batch the fast default with Claude-plus semantics
status: done
priority: critical
tags:
  - browser-speed
  - batching
  - mcp
  - claude-parity
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:55:18.239936Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:22:49.378995Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:24:37.343312Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:55:18Z
updated_at: 2026-06-14T08:24:37Z
---

Existing task 01KV0BDPAPFV2H1MVZT8SQEWAS covers first-class browser_batch. Extend its acceptance criteria using Claude's model: sequential actions, no nested batch, per-step permission/safety check, stop on first error, interleaved text/image outputs, post-action compact observation, tabId/session scoping, and explicit guidance nudging agents to batch predictable sequences.
