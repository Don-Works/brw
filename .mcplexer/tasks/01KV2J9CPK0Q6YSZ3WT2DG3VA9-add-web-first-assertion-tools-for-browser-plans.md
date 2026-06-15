---
id: 01KV2J9CPK0Q6YSZ3WT2DG3VA9
schema: task/v1
workspace: agent-browser
title: Add web-first assertion tools for browser plans
status: done
priority: critical
tags:
  - assertions
  - mcp
  - quality
  - speed
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:59:37.171602Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:18:45.277579Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:22:49.36427Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:59:37Z
updated_at: 2026-06-14T08:22:49Z
---

Expose retrying assertions as MCP/browser_plan steps: expect_text, expect_not_text, expect_ref, expect_role, expect_value, expect_checked, expect_url, expect_console_clear, expect_network_idle/match, expect_download, expect_count. Assertions should re-fetch/re-resolve until pass or timeout, then return compact evidence. This replaces wait/snapshot loops and makes plans deterministic.
