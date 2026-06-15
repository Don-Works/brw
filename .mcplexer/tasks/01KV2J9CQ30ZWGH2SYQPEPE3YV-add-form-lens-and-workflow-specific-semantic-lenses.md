---
id: 01KV2J9CQ30ZWGH2SYQPEPE3YV
schema: task/v1
workspace: agent-browser
title: Add form lens and workflow-specific semantic lenses
status: done
priority: high
tags:
  - form-lens
  - token-efficiency
  - quality
  - semantic-frontier
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:59:37.187015Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:35:00.313475Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:36:49.384086Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:59:37Z
updated_at: 2026-06-14T08:36:49Z
---

When focus/context is inside a form/table/list/cart/checkout/modal, return a compact lens: fields, labels, current values/redaction, required/invalid state, errors/live regions, submit/next controls, and relevant list/table rows. This should usually replace full snapshots for data-entry and checkout workflows.
