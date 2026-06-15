---
id: 01KV18MXNVRHN6TPX29CX3FAFB
schema: task/v1
workspace: agent-browser
title: Fix stale pending mesh indicator when receive returns no new messages
status: open
priority: normal
tags:
  - mcplexer
  - mesh
  - coordination
  - ux
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by: 01KV18NXKHVJYSB50GYPGFGDJ5
source:
  kind: agent
  session_id: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
status_history:
  - at: 2026-06-13T19:51:54.811055Z
    evt: created
    to: open
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
created_at: 2026-06-13T19:51:54Z
updated_at: 2026-06-13T19:52:27.539592Z
---

Tool responses repeatedly showed '[mesh: 2 pending message(s)]' even after mesh.receive({filter:'new'}) returned new_messages=0. This creates coordination noise during multi-agent work. Investigate whether pending counts include task_event/self messages hidden by default, stale read state, or cross-workspace messages. The hint should say what kind/filter is needed, or clear when there are no actionable messages.
