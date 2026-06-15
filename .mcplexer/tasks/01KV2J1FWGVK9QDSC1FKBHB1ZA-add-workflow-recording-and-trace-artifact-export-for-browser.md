---
id: 01KV2J1FWGVK9QDSC1FKBHB1ZA
schema: task/v1
workspace: agent-browser
title: Add workflow recording and trace artifact export for browser automation
status: done
priority: normal
tags:
  - trace
  - recording
  - gif
  - debugging
  - claude-parity
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:55:18.288522Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:39:47.233256Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:41:20.501463Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:55:18Z
updated_at: 2026-06-14T08:41:20Z
---

Claude has gif_creator/workflow recording: start/stop/clear/export scoped to tab group, captures screenshots around actions, can download/export with overlays. Build a safer agent-browser trace format first, then optional GIF/web replay export for debugging and demos.
