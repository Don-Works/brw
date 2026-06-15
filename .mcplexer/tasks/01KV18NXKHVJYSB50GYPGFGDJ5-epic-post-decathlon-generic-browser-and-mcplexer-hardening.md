---
id: 01KV18NXKHVJYSB50GYPGFGDJ5
schema: task/v1
workspace: agent-browser
title: 'Epic: Post-Decathlon generic browser and mcplexer hardening'
status: open
priority: high
tags:
  - epic
  - agent-browser
  - mcplexer
  - post-decathlon
  - reliability
pinned: false
assignee:
  origin_kind: local
composes:
  - 01KV18KKAHFY6094DADTE7EVSS
  - 01KV18KKAPP6PCS32YSSKPAWT6
  - 01KV18KKAVSG79PT6J1MVXJAAC
  - 01KV18KKB0CSKT92J8EJ0392EG
  - 01KV18KKB4F2RRPY4DSH4FS9EC
  - 01KV18KKBFXP19YRVT59RES28F
  - 01KV18MXNNG4Z46YRRRH06HE4R
  - 01KV18MXNVRHN6TPX29CX3FAFB
  - 01KV18MXNY8FD4CP3B0QX8HC67
  - 01KV18MXP17EQZS523CQ67R9HX
  - 01KV18PBBJ848KJJ9V3SKJ07PC
source:
  kind: agent
  session_id: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
status_history:
  - at: 2026-06-13T19:52:27.50593Z
    evt: created
    to: open
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-13T19:52:27.510773Z
    evt: composed
    to: 01KV18KKAHFY6094DADTE7EVSS
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-13T19:52:27.514924Z
    evt: composed
    to: 01KV18KKAPP6PCS32YSSKPAWT6
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-13T19:52:27.518898Z
    evt: composed
    to: 01KV18KKAVSG79PT6J1MVXJAAC
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-13T19:52:27.522253Z
    evt: composed
    to: 01KV18KKB0CSKT92J8EJ0392EG
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-13T19:52:27.526967Z
    evt: composed
    to: 01KV18KKB4F2RRPY4DSH4FS9EC
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-13T19:52:27.530538Z
    evt: composed
    to: 01KV18KKBFXP19YRVT59RES28F
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-13T19:52:27.534443Z
    evt: composed
    to: 01KV18MXNNG4Z46YRRRH06HE4R
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-13T19:52:27.537894Z
    evt: composed
    to: 01KV18MXNVRHN6TPX29CX3FAFB
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-13T19:52:27.550672Z
    evt: composed
    to: 01KV18MXNY8FD4CP3B0QX8HC67
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-13T19:52:27.554181Z
    evt: composed
    to: 01KV18MXP17EQZS523CQ67R9HX
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-13T19:52:41.590864Z
    evt: composed
    to: 01KV18PBBJ848KJJ9V3SKJ07PC
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
created_at: 2026-06-13T19:52:27Z
updated_at: 2026-06-13T19:52:41.59087Z
---

Generic learnings from the max-air Decathlon checkout acid test. Scope covers no-focus tab-scoped browser control, robust semantic ecommerce assertions, stale-ref recovery, cleanup discipline, network observation planning, and mcplexer coordination/tooling issues discovered during the run.
