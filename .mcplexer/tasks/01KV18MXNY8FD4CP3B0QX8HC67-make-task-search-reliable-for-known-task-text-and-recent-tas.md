---
id: 01KV18MXNY8FD4CP3B0QX8HC67
schema: task/v1
workspace: agent-browser
title: Make task search reliable for known task text and recent task IDs
status: open
priority: normal
tags:
  - mcplexer
  - tasks
  - search
  - coordination
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by: 01KV18NXKHVJYSB50GYPGFGDJ5
source:
  kind: agent
  session_id: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
status_history:
  - at: 2026-06-13T19:51:54.814768Z
    evt: created
    to: open
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
created_at: 2026-06-13T19:51:54Z
updated_at: 2026-06-13T19:52:27.552419Z
---

task.get worked for known regression task IDs, but task.list({q:'agent-browser mcplexer epic browser integration...'}) returned count 0 despite relevant existing tasks. This made it hard to compose follow-ups into the right epic and avoid duplicates. Improve FTS/semantic search diagnostics: return workspace searched, known matching IDs/tags when q has no result, and support id-prefix/title/tag matching consistently across open/closed tasks.
