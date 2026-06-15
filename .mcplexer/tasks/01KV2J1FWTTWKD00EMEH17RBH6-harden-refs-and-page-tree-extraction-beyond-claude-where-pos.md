---
id: 01KV2J1FWTTWKD00EMEH17RBH6
schema: task/v1
workspace: agent-browser
title: Harden refs and page-tree extraction beyond Claude where possible
status: done
priority: high
tags:
  - stable-refs
  - shadow-dom
  - iframes
  - snapshots
  - claude-parity
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:55:18.298035Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:37:07.226369Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:39:40.182956Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:55:18Z
updated_at: 2026-06-14T08:39:40Z
---

Claude's tree supports depth/ref focusing, truncation limits, all-frame content scripts, WeakRef element maps, and some shadow DOM traversal. Make ours better: stable refs across rerenders, stale-ref recovery, iframe-aware snapshots, shadow DOM coverage, focus-scoped snapshots, truncation/max-bytes controls, and deterministic source attribution. Coordinate with active stable-ref work.
