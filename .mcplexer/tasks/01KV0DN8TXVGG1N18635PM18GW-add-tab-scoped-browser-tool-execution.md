---
id: 01KV0DN8TXVGG1N18635PM18GW
schema: task/v1
workspace: agent-browser
title: Add tab-scoped browser tool execution
status: done
priority: critical
tags:
  - mcp
  - tabs
  - extension-bridge
  - reliability
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by:
    - 01KV0AGWKR7R02GH7PM6S8B8KG
    - 01KV2J1FSZGRNF6EPK977AKDDD
  touches_files:
    - agent-browser/internal/browser/manager.go
    - agent-browser/internal/extensionbridge/bridge.go
    - agent-browser/internal/mcp/server.go
    - agent-browser/internal/http/server.go
source:
  kind: agent
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
status_history:
  - at: 2026-06-13T12:00:14.684991Z
    evt: created
    to: doing
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T12:00:14.684991Z
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
  - at: 2026-06-13T12:06:45.858026Z
    evt: status_changed
    from: open
    to: review
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T12:15:41.7763Z
    evt: status_changed
    from: review
    to: done
    by_session: 40c0aea5-3ddd-4ad5-950d-1de1fa265b82
created_at: 2026-06-13T12:00:14Z
updated_at: 2026-06-14T07:55:18.341122Z
---

Allow core semantic tools to target a specific tab_id/window target, or reliably use focus_tab as execution target. Acceptance: open Selenium while Decathlon is open, run form submit, and read/snapshot the Selenium tab deterministically.

## Notes
- 2026-06-13 (agent): Implemented deterministic bridge target routing: open/focus/list active focused target update daemon active_tab_id; CDP/evaluate defaults to remembered tab id and clears stale ids on no-tab errors. This should address reads/actions drifting to another active Chrome tab after focus.
- 2026-06-13 (agent): Implemented and verified popup/window target routing generically. Changes: extension bridge remembers active tab id from open/focus/active_tab events; CDP/evaluate defaults to the remembered target; list_tabs maps popup/window/opener metadata when the updated extension is loaded; action results include relevant targets. Added popup opener/child fixtures and browsercheck scenario fixture-popup-target-routing. Verification: jq core.json passed; go test ./... passed; node --check extension/service_worker.js passed; make build passed; local direct-CDP browsercheck fixture-popup-target-routing passed; full local browsercheck pack passed (6 passed, 7 skipped). max-air installed binaries/extension/tests refreshed; current max-air daemon is updated and connected, but Chrome is still running old extension service worker hello.version=0.1.0 until the installed extension is reloaded, so popup/window metadata from the extension is not yet live there.
- 2026-06-13 (agent): Final popup/target-routing implementation shipped to max-air install path. Extension manifest and hello marker are now 0.1.3, so /status should show hello.version=0.1.3 after Chrome reloads the installed extension. Current loaded max-air service worker still reports hello.version=0.1.0, so Chrome has not reloaded extension code yet. Local verification after final changes: node --check extension/service_worker.js passed; go test ./... passed; make build passed; full local browsercheck passed (6 passed, 7 skipped), including fixture-popup-target-routing. Final max-air installed hashes: {"agent_browserd":"50879e1ee74663942665f24084cd35750ef07d6e031daf19f2cc53ed27d3bfe0","service_worker":"b2d7ec370daa92c2399df45cf9f9787117b5ebdea72399ec0fb929b6886613db","manifest":"f8d055b3e554ca23c7a2355cb023130f72886d51fc59a89d71969df4af211a89","core_json":"0897ea2402fad7c515a215fabf6a8222bce02cf9172368f963481a827674eb85"}. One quick max-air Selenium MCP smoke timed out at mcplexer tools/call after 120s, so not counted as pass.
- 2026-06-13 (agent): Focus stealing observed during live max-air Decathlon checkout test. Current bridge/tools are active-tab based, so the driver has to call browser_focus_tab and Chrome visibly activates the target; user activity can steal it back and page tools may then run against google/chrome://. Generic fix: add tab_id/target_id to page tools (snapshot/read/find/click/fill/type/select/press/scroll/wait/screenshot/upload), thread it through HTTP+MCP+browsercheck, and make extension bridge evaluate/action against that tabId without chrome.tabs.update({active:true}) except for explicit browser_focus_tab or foreground-only fallback. Background-capable operations should run in inactive tabs; user-activation/trusted-input cases should return a clear foreground_required warning/result.
