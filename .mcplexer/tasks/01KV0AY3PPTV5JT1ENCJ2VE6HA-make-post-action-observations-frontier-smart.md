---
id: 01KV0AY3PPTV5JT1ENCJ2VE6HA
schema: task/v1
workspace: agent-browser
title: Make post-action observations frontier-smart
status: open
priority: high
tags:
  - performance
  - observation
  - agent-browser
pinned: false
assignee:
  origin_kind: local
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
meta:
  composed_by: 01KV0AGWKR7R02GH7PM6S8B8KG
  touches_files:
    - agent-browser/internal/browser/manager.go
    - agent-browser/internal/extensionbridge/bridge.go
    - agent-browser/internal/snapshot/scripts.go
source:
  kind: agent
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
status_history:
  - at: 2026-06-13T11:12:38.614606Z
    evt: created
    to: open
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:12:38.614606Z
    evt: assigned
    to: f8ed800b-c98d-4403-a5ba-ea03a0447722
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
created_at: 2026-06-13T11:12:38Z
updated_at: 2026-06-13T11:12:38Z
---

Live Decathlon run showed click observations return the first capped controls, not the most relevant changed controls after opening postcode/modal UI. Implement semantic frontier ranking: focused element, dialogs, live regions, validation/status, newly visible controls, and query/action-neighborhood before generic page controls.
