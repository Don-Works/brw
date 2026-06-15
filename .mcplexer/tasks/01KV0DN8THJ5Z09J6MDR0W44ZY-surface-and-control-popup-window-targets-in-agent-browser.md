---
id: 01KV0DN8THJ5Z09J6MDR0W44ZY
schema: task/v1
workspace: agent-browser
title: Surface and control popup/window targets in agent-browser
status: done
priority: critical
tags:
  - mcp
  - popup
  - window
  - extension-bridge
  - max-air
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by:
    - 01KV0AGWKR7R02GH7PM6S8B8KG
    - 01KV2J1FSZGRNF6EPK977AKDDD
  touches_files:
    - agent-browser/internal/browser/manager.go
    - agent-browser/internal/browser/types.go
    - agent-browser/internal/extensionbridge/bridge.go
    - agent-browser/extension
    - agent-browser/internal/mcp/server.go
source:
  kind: agent
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
status_history:
  - at: 2026-06-13T12:00:14.673333Z
    evt: created
    to: doing
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T12:00:14.673333Z
    evt: assigned
    to: f8ed800b-c98d-4403-a5ba-ea03a0447722
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T12:00:58.154616Z
    evt: status_changed
    from: doing
    to: open
    note: lease expired, demoted from working status
  - at: 2026-06-13T12:00:58.154616Z
    evt: lease_expired
  - at: 2026-06-13T12:06:45.853863Z
    evt: status_changed
    from: open
    to: review
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T12:15:41.776543Z
    evt: status_changed
    from: review
    to: done
    by_session: 40c0aea5-3ddd-4ad5-950d-1de1fa265b82
created_at: 2026-06-13T12:00:14Z
updated_at: 2026-06-14T07:55:18.336638Z
---

When a site opens an auth popup/window, agent-browser must list it, focus it, snapshot it, and report it in action observations. Acceptance: Intervals.pro connect popup appears in browser_list_tabs with type/window metadata and is actionable through MCP.

## Notes
- 2026-06-13 (agent): Implemented local bridge/extension popup target support: list_tabs now carries window metadata including popup windows; focus_tab updates remembered execution target; active_tab extension events update daemon target; action results now include relevant targets. Local go test ./..., node --check extension service_worker.js, and make build passed.
- 2026-06-13 (agent): Implemented and verified popup/window target routing generically. Changes: extension bridge remembers active tab id from open/focus/active_tab events; CDP/evaluate defaults to the remembered target; list_tabs maps popup/window/opener metadata when the updated extension is loaded; action results include relevant targets. Added popup opener/child fixtures and browsercheck scenario fixture-popup-target-routing. Verification: jq core.json passed; go test ./... passed; node --check extension/service_worker.js passed; make build passed; local direct-CDP browsercheck fixture-popup-target-routing passed; full local browsercheck pack passed (6 passed, 7 skipped). max-air installed binaries/extension/tests refreshed; current max-air daemon is updated and connected, but Chrome is still running old extension service worker hello.version=0.1.0 until the installed extension is reloaded, so popup/window metadata from the extension is not yet live there.
- 2026-06-13 (agent): Final popup/target-routing implementation shipped to max-air install path. Extension manifest and hello marker are now 0.1.3, so /status should show hello.version=0.1.3 after Chrome reloads the installed extension. Current loaded max-air service worker still reports hello.version=0.1.0, so Chrome has not reloaded extension code yet. Local verification after final changes: node --check extension/service_worker.js passed; go test ./... passed; make build passed; full local browsercheck passed (6 passed, 7 skipped), including fixture-popup-target-routing. Final max-air installed hashes: {"agent_browserd":"50879e1ee74663942665f24084cd35750ef07d6e031daf19f2cc53ed27d3bfe0","service_worker":"b2d7ec370daa92c2399df45cf9f9787117b5ebdea72399ec0fb929b6886613db","manifest":"f8d055b3e554ca23c7a2355cb023130f72886d51fc59a89d71969df4af211a89","core_json":"0897ea2402fad7c515a215fabf6a8222bce02cf9172368f963481a827674eb85"}. One quick max-air Selenium MCP smoke timed out at mcplexer tools/call after 120s, so not counted as pass.
- 2026-06-13 (agent): Remote activation note: Tried non-destructive remote extension reload paths on max-air. Opening chrome://extensions via bridge works, but semantic access is blocked with Cannot access a chrome:// URL. AppleScript/System Events UI probe hung, likely waiting on Accessibility/UI scripting, so it was killed. Closed leftover chrome://extensions tabs. Bridge still connected but loaded extension hello.version remains 0.1.0. Manual extension reload or Chrome restart is required to activate the on-disk 0.1.3 extension code.
