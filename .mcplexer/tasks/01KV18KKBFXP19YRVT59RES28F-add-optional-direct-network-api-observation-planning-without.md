---
id: 01KV18KKBFXP19YRVT59RES28F
schema: task/v1
workspace: agent-browser
title: Add optional direct network/API observation planning without site-specific automation
status: done
priority: normal
tags:
  - agent-browser
  - network-observation
  - api-planning
  - future
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
status_history:
  - at: 2026-06-13T19:51:11.471524Z
    evt: created
    to: open
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-14T08:39:47.241648Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:41:20.519265Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-13T19:51:11Z
updated_at: 2026-06-14T08:41:20Z
---

User noted that if the browser can observe network it can sometimes make direct API calls later. Decathlon exposed the likely value but we should keep this generic and not hardcode sites. Create a capability that summarizes recent XHR/fetch endpoints, request/response shapes, auth/cookie scope, and safe repeatability, but does not automatically bypass UI or security gates. Future actions can propose direct API calls only when idempotent/safe and explicitly tied to observed user-visible UI state.
