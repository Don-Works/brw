---
id: 01KV2J9CQQDBFQ9MATEYJYENDZ
schema: task/v1
workspace: agent-browser
title: Add browser automation performance trace and scorecard
status: done
priority: high
tags:
  - performance
  - trace
  - scorecard
  - conformance
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:59:37.207729Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:37:07.220368Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:39:40.176468Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:59:37Z
updated_at: 2026-06-14T08:39:40Z
---

Record per-tool timings, CDP round trips, output bytes/token estimates, snapshot size, cache hit/miss, actionability wait time, ref recovery events, and resulting changed state. Feed this into the conformance suite so we can compare ours vs Claude by median task time and failure rate.
