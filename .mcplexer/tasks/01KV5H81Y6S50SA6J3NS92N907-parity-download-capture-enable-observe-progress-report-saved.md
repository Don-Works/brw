---
id: 01KV5H81Y6S50SA6J3NS92N907
schema: task/v1
workspace: agent-browser
title: '[parity] Download capture: enable + observe progress + report saved path (browser_downloads)'
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
    - internal/extensionbridge/bridge.go
    - internal/mcp/server.go
source:
  kind: agent
  session_id: eec9898e-0967-4c8d-a37f-bf0d3612b52d
status_history:
  - at: 2026-06-15T11:39:08.102352Z
    evt: created
    to: open
    by_session: eec9898e-0967-4c8d-a37f-bf0d3612b52d
  - at: 2026-06-15T12:13:44.862364Z
    evt: status_changed
    from: open
    to: review
    by_session: eec9898e-0967-4c8d-a37f-bf0d3612b52d
created_at: 2026-06-15T11:39:08Z
updated_at: 2026-06-15T12:13:44Z
---

## Notes
- 2026-06-15 (agent): ROUND 1 browser_downloads capture (CDP Browser.setDownloadBehavior + download events; bridge returns note). Integrated, build+test green.
