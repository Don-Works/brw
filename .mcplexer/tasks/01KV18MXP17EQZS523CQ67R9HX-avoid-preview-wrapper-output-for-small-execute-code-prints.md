---
id: 01KV18MXP17EQZS523CQ67R9HX
schema: task/v1
workspace: agent-browser
title: Avoid preview-wrapper output for small execute_code prints
status: open
priority: normal
tags:
  - mcplexer
  - tool-results
  - ux
  - token-efficiency
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by: 01KV18NXKHVJYSB50GYPGFGDJ5
source:
  kind: agent
  session_id: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
status_history:
  - at: 2026-06-13T19:51:54.817928Z
    evt: created
    to: open
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
created_at: 2026-06-13T19:51:54Z
updated_at: 2026-06-13T19:52:27.556051Z
---

Some mcpx execute_code print outputs came back as a {kind:text, bytes, preview} wrapper even for sub-1KB multi-line strings, while simple strings returned normally. This made exact task IDs harder to read and forced extra calls. Improve result rendering so small outputs are delivered as direct text, and reserve preview wrappers for genuinely large/truncated outputs with an obvious hydration path.
