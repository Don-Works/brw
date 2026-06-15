---
id: 01KV2J1FV7AAZ5J9W3XGA7W9CD
schema: task/v1
workspace: agent-browser
title: Add Claude-style coordinate primitives without weakening semantic refs
status: done
priority: high
tags:
  - actions
  - hover
  - drag
  - mcp
  - claude-parity
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:55:18.247377Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:37:07.215155Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:39:40.122781Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:55:18Z
updated_at: 2026-06-14T08:39:40Z
---

Add or finish parity for hover, right_click, double_click, triple_click, left_click_drag, coordinate scroll, zoom/region screenshot, and scroll_to ref. Keep semantic refs as the default; use coordinates for canvas, drag, hover menus, and visual fallback. Coordinate work with the active hover/stable-ref agent.
