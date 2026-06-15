---
id: 01KV141TDBK1NV0BAB6QNDVDRS
schema: task/v1
workspace: agent-browser
title: Add pushed DOM visibility/change events from extension bridge
status: done
priority: high
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: e35c872e-ae0e-4910-9901-bd30cbc0347d
status_history:
  - at: 2026-06-13T18:31:34.570938Z
    evt: created
    to: open
    by_session: e35c872e-ae0e-4910-9901-bd30cbc0347d
  - at: 2026-06-14T08:35:00.324737Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:36:49.392966Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-13T18:31:34Z
updated_at: 2026-06-14T08:36:49Z
---

Implement true extension-origin page mutation/change events so browser actions can receive near-realtime DOM deltas without polling full snapshots. Use MutationObserver/visibility/intersection/focus/ARIA state tracking in the page context, coalesce events, and push compact records over the extension WebSocket: nodes became visible/hidden, text changed, modal/dialog opened, aria-expanded/disabled/invalid changed, focused element changed, controlled element became active, new target/popup opened. MCP action results should include these deltas when available and fall back to structural frontier snapshots. Acceptance: Decathlon-style click that opens options/modal surfaces the newly visible actionable controls immediately from delta events; static conformance fixture covers visibility toggles, popup form, modal open/close, delayed DOM, and aria-live changes; token output remains bounded.
