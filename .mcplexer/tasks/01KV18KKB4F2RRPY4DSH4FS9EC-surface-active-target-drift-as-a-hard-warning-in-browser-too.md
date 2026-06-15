---
id: 01KV18KKB4F2RRPY4DSH4FS9EC
schema: task/v1
workspace: agent-browser
title: Surface active-target drift as a hard warning in browser tool results
status: done
priority: high
tags:
  - agent-browser
  - targeting
  - observability
  - tool-results
  - reliability
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
status_history:
  - at: 2026-06-13T19:51:11.460494Z
    evt: created
    to: open
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-14T08:30:41.332649Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:32:09.44002Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-13T19:51:11Z
updated_at: 2026-06-14T08:32:09Z
---

The live run hit cases where active target drifted to a Google search or chrome://newtab while the agent believed it was still on Decathlon. Generic fix: include target_id, url, title in every action/read/snapshot result and warn/error when actual URL/title no longer matches the last selected/opened target or an expected target constraint. Add optional expected_url_contains/expected_title_contains guards to page actions and browsercheck steps. This is separate from tab-scoping and helps catch wrong-tab actions early.
