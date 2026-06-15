---
id: 01KV2J9CPAG924SQ36WQR82AS3
schema: task/v1
workspace: agent-browser
title: Add Playwright-grade actionability and auto-wait to semantic actions
status: done
priority: critical
tags:
  - reliability
  - speed
  - actionability
  - semantic-actions
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:59:37.162538Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:17:05.836945Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:18:45.272594Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:59:37Z
updated_at: 2026-06-14T08:18:45Z
---

Before click/fill/drag/select, enforce an actionability contract inspired by Playwright: target resolves uniquely, is visible, has stable bounding box, receives pointer events/not obscured, and is enabled/editable where relevant. Auto-wait within timeout; return structured failure reasons and candidate refs instead of misclicking or requiring sleeps. Apply to direct CDP and extension bridge. Sources: Playwright actionability docs.
