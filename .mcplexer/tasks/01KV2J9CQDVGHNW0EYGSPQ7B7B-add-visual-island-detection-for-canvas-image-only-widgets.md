---
id: 01KV2J9CQDVGHNW0EYGSPQ7B7B
schema: task/v1
workspace: agent-browser
title: Add visual island detection for canvas/image-only widgets
status: done
priority: normal
tags:
  - visual-islands
  - screenshots
  - ocr
  - quality
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:59:37.197372Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:39:47.246177Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:41:20.530455Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:59:37Z
updated_at: 2026-06-14T08:41:20Z
---

Detect regions where DOM/accessibility semantics are insufficient: canvas, maps, charts, image buttons, custom drawing surfaces. Return cropped screenshot/OCR/box only for the relevant island, not whole-page screenshots. Keep normal web pages semantic-first.
