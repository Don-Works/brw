---
id: 01KV15QQGX3YSW26CHXY19GQ30
schema: task/v1
workspace: agent-browser
title: Make browser_scroll target active modal/drawer scroll containers
status: done
priority: high
tags:
  - agent-browser
  - efficiency
  - scroll
  - modal
  - generic
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by:
    - 01KV0AGWKR7R02GH7PM6S8B8KG
    - 01KV2J1FSZGRNF6EPK977AKDDD
  touches_files:
    - agent-browser/internal/snapshot/scripts.go
    - agent-browser/internal/browser/manager.go
    - agent-browser/internal/extensionbridge/bridge.go
    - agent-browser/tests/fixtures/*
    - agent-browser/tests/scenarios/core.json
source:
  kind: agent
  session_id: 920d1707-1199-4611-a29e-71740df38255
status_history:
  - at: 2026-06-13T19:01:01.08523Z
    evt: created
    to: doing
    by_session: 920d1707-1199-4611-a29e-71740df38255
  - at: 2026-06-13T19:01:01.08523Z
    evt: assigned
    to: 920d1707-1199-4611-a29e-71740df38255
    by_session: 920d1707-1199-4611-a29e-71740df38255
  - at: 2026-06-13T19:01:19.092497Z
    evt: status_changed
    from: doing
    to: open
    note: lease expired, demoted from working status
  - at: 2026-06-13T19:01:19.092497Z
    evt: lease_expired
  - at: 2026-06-13T19:06:25.28184Z
    evt: status_changed
    from: open
    to: done
    by_session: 920d1707-1199-4611-a29e-71740df38255
  - at: 2026-06-13T19:06:25.28184Z
    evt: closed
    to: done
    by_session: 920d1707-1199-4611-a29e-71740df38255
created_at: 2026-06-13T19:01:01Z
updated_at: 2026-06-14T07:55:18.355385Z
---

Decathlon exposed a generic failure mode: a right-side purchase drawer/modal has its own scroll container, while browser_scroll only scrolls window. This strands refs inside nested panels and makes dynamic commerce flows slow and confusing.

Acceptance:
- browser_scroll detects visible scrollable ancestors/overlays/dialogs and scrolls the active/topmost scroll container before window.
- Works without site-specific selectors or Decathlon heuristics.
- Action result reports changed_state based on semantic state after nested scroll.
- Add/extend static fixture for nested drawer scroll if feasible.

## Notes
- 2026-06-13 (agent): Shipped generic active-scroll-container behavior. browser_scroll now evaluates visible scrollable candidates and prefers focused/topmost modal/drawer/listbox/dialog containers before window scroll. Added static drawer-scroll fixture; verified local browsercheck and max-air MCP. Remote proof: browser_scroll reported target:element "Scrollable task drawer", then the drawer-only button became visible and clicked with result text "Drawer confirmed at scroll 806".
