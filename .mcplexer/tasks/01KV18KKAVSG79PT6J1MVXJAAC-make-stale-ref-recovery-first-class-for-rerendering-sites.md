---
id: 01KV18KKAVSG79PT6J1MVXJAAC
schema: task/v1
workspace: agent-browser
title: Make stale-ref recovery first-class for rerendering sites
status: done
priority: high
tags:
  - agent-browser
  - refs
  - rerender
  - semantic-actions
  - reliability
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
status_history:
  - at: 2026-06-13T19:51:11.451321Z
    evt: created
    to: open
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-14T08:30:41.306981Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:32:09.434868Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-13T19:51:11Z
updated_at: 2026-06-14T08:32:09Z
---

During Decathlon and Intervals runs, refs became stale after drawers/modals/rerenders. The successful workaround was to re-find by semantic query/role and verify post-action value, but agents had to do this manually. Generic fix: when an action fails because ref is missing/stale, retry by the original element signature (role/name/query/tag/value) if available, or return a structured stale_ref result with suggested candidates. Browsercheck should prefer query+role for rerender-prone fields and record semantic identity with saved refs.
