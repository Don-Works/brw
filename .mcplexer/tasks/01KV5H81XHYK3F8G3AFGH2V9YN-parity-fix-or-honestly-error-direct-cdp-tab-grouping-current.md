---
id: 01KV5H81XHYK3F8G3AFGH2V9YN
schema: task/v1
workspace: agent-browser
title: '[parity] Fix or honestly-error direct-CDP tab grouping (currently silent no-op)'
status: review
priority: normal
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by: 01KV2J1FSZGRNF6EPK977AKDDD
  source: claude-parity-gap-analysis wf_c783b57f-d06
  touches_files:
    - internal/browser/manager.go
source:
  kind: agent
  session_id: eec9898e-0967-4c8d-a37f-bf0d3612b52d
status_history:
  - at: 2026-06-15T11:39:08.081817Z
    evt: created
    to: open
    by_session: eec9898e-0967-4c8d-a37f-bf0d3612b52d
  - at: 2026-06-15T12:13:44.854282Z
    evt: status_changed
    from: open
    to: review
    by_session: eec9898e-0967-4c8d-a37f-bf0d3612b52d
created_at: 2026-06-15T11:39:08Z
updated_at: 2026-06-15T12:13:44Z
---

## Notes
- 2026-06-15 (agent): ROUND 1 Tab-grouping bug FIXED: direct-CDP now returns honest ErrTabGroupingUnsupported (CDP genuinely cannot create tab groups — extension-only) instead of silent fake success. Integrated, tested.
