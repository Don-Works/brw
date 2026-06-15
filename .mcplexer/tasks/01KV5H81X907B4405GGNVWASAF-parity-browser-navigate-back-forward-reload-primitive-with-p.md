---
id: 01KV5H81X907B4405GGNVWASAF
schema: task/v1
workspace: agent-browser
title: '[parity] browser_navigate back/forward/reload primitive with post-nav observation'
status: review
priority: normal
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by: 01KV2J1FSZGRNF6EPK977AKDDD
  source: claude-parity-gap-analysis wf_c783b57f-d06
  touches_files:
    - internal/mcp/server.go
    - internal/browser/manager.go
    - internal/extensionbridge/bridge.go
source:
  kind: agent
  session_id: eec9898e-0967-4c8d-a37f-bf0d3612b52d
status_history:
  - at: 2026-06-15T11:39:08.073114Z
    evt: created
    to: open
    by_session: eec9898e-0967-4c8d-a37f-bf0d3612b52d
  - at: 2026-06-15T12:13:44.84421Z
    evt: status_changed
    from: open
    to: review
    by_session: eec9898e-0967-4c8d-a37f-bf0d3612b52d
created_at: 2026-06-15T11:39:08Z
updated_at: 2026-06-15T12:13:44Z
---

## Notes
- 2026-06-15 (agent): ROUND 1 browser_navigate (back/forward/reload) — integrated to HEAD, build+test green. New manager_navigate.go + bridge + http + httpclient + chromedp test. + generic isTransientNavigationError hardening.
