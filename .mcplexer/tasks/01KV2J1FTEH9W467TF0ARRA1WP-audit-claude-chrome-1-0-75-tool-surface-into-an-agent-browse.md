---
id: 01KV2J1FTEH9W467TF0ARRA1WP
schema: task/v1
workspace: agent-browser
title: Audit Claude Chrome 1.0.75 tool surface into an agent-browser gap matrix
status: done
priority: high
tags:
  - research
  - claude-parity
  - agent-browser
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:55:18.22231Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:39:47.252898Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:41:20.534321Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:55:18Z
updated_at: 2026-06-14T08:41:20Z
---

Read the local Claude extension package and produce a concise gap matrix: capability, Claude source file, current agent-browser equivalent, missing behavior, safety decision, test fixture needed. Start at:
/Users/max/Library/Application Support/Google/Chrome/Profile 1/Extensions/fcoeoabgfenejglbffodgkkbkcdhcgfn/1.0.75_0/manifest.json
/Users/max/Library/Application Support/Google/Chrome/Profile 1/Extensions/fcoeoabgfenejglbffodgkkbkcdhcgfn/1.0.75_0/managed_schema.json
/Users/max/Library/Application Support/Google/Chrome/Profile 1/Extensions/fcoeoabgfenejglbffodgkkbkcdhcgfn/1.0.75_0/assets/mcpPermissions-8PlHLvdl.js
/Users/max/Library/Application Support/Google/Chrome/Profile 1/Extensions/fcoeoabgfenejglbffodgkkbkcdhcgfn/1.0.75_0/assets/service-worker.ts-BsAUV92e.js
/Users/max/Library/Application Support/Google/Chrome/Profile 1/Extensions/fcoeoabgfenejglbffodgkkbkcdhcgfn/1.0.75_0/assets/accessibility-tree.js-CCweLwU2.js
/Users/max/Library/Application Support/Google/Chrome/Profile 1/Extensions/fcoeoabgfenejglbffodgkkbkcdhcgfn/1.0.75_0/assets/agent-visual-indicator.js-DVYDybPo.js
/Users/max/Library/Application Support/Google/Chrome/Profile 1/Extensions/fcoeoabgfenejglbffodgkkbkcdhcgfn/1.0.75_0/assets/sidepanel-BL0NRfq2.js

Do not copy proprietary code verbatim; extract behavior and product patterns only.
