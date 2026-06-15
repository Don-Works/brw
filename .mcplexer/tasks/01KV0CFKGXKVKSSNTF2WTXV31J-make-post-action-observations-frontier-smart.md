---
id: 01KV0CFKGXKVKSSNTF2WTXV31J
schema: task/v1
workspace: agent-browser
title: Make post-action observations frontier-smart
status: open
priority: high
tags:
  - mcp
  - browser-speed
  - observations
pinned: false
assignee:
  origin_kind: local
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
meta:
  composed_by: 01KV0AGWKR7R02GH7PM6S8B8KG
source:
  kind: agent
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
status_history:
  - at: 2026-06-13T11:39:40.445488Z
    evt: created
    to: open
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:39:40.445488Z
    evt: assigned
    to: f8ed800b-c98d-4403-a5ba-ea03a0447722
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
created_at: 2026-06-13T11:39:40Z
updated_at: 2026-06-13T11:39:40Z
---

Replace generic first-N post-action snapshots with focused/changed/frontier observations: focused element, active dialog/drawer, live-region messages, validation errors, controls near the acted element, and newly appeared/removed controls. Include why each element was returned.

## Notes
- 2026-06-13 (agent): Corrected implementation direction: removed keyword/site-word frontier scorer. Added structural snapshot signals only (focused/focus-within, expanded, has-popup, controls, active-descendant, invalid, required, live, frontier-role) and action observations now return deterministic structural frontier buckets. No Decathlon-specific words or site heuristics remain in browser/snapshot/extensionbridge code. go test ./... and browsercheck fixture-form-actions pass locally.
- 2026-06-13 (agent): Deployed structural-only post-action observations to max-air. MCP upload smoke on selenium.dev passed in ~675 ms; post-action frontier returned structural signals only: label focus-within and file input focused. No site-word scoring.
