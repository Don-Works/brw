---
id: 01KV5AZ408QRBQW7HE5MNGCQZF
schema: task/v1
workspace: agent-browser
title: '[browser-friction] Cut per-click latency (~5.1s fixed settle on browser_click)'
status: done
priority: high
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by: 01KV5A3X61WX1RDN0HYDZT2GYT
  kind: friction-fix
  source: live-decathlon-drive
source:
  kind: agent
  session_id: c988fcc9-7e13-4b73-824c-190641734a71
status_history:
  - at: 2026-06-15T09:49:23.848793Z
    evt: created
    to: open
    by_session: c988fcc9-7e13-4b73-824c-190641734a71
  - at: 2026-06-15T10:16:33.548907Z
    evt: status_changed
    from: open
    to: review
    by_session: 59050316-1ea7-4a5b-9634-46aa6a29fb4f
  - at: 2026-06-15T10:55:05.701578Z
    evt: status_changed
    from: review
    to: doing
    by_session: eec9898e-0967-4c8d-a37f-bf0d3612b52d
  - at: 2026-06-15T10:55:05.701578Z
    evt: assigned
    to: eec9898e-0967-4c8d-a37f-bf0d3612b52d
    by_session: eec9898e-0967-4c8d-a37f-bf0d3612b52d
  - at: 2026-06-15T10:56:58.777243Z
    evt: status_changed
    from: doing
    to: done
    by_session: eec9898e-0967-4c8d-a37f-bf0d3612b52d
  - at: 2026-06-15T11:00:24.081905Z
    evt: lease_expired
created_at: 2026-06-15T09:49:23Z
updated_at: 2026-06-15T11:00:24.101132Z
---

Every browser_click round-trip measured ~5100ms incl. no-op clicks. Suggests a fixed multi-second post-action settle / network-idle wait. Make settle event-driven/adaptive (DOM-mutation or short quiet-window) so fast pages return fast. Biggest perceived jank.

## Notes
- 2026-06-15 (agent): FIXED in working tree (build+test green: go build ok, go test ./... all pass). File: internal/extensionbridge/bridge.go + internal/snapshot/scripts.go. clickRef now actuates via a single in-page click round-trip (ClickXYScript at the resolved box centre) instead of 3 sequential CDP Input.dispatchMouseEvent calls that each block ~1.5s on renderer-ack on heavy pages. Trusted CDP dispatch retained as fallback when the point isn't hit-testable in-page. Expected ~5.1s -> tens of ms (matches measured click_xy). Needs live-reload to benchmark end-to-end.
- 2026-06-15 (agent): LIVE-VERIFIED (fresh session, real Chrome extension bridge, Decathlon 333374 running shorts). Timed browser_click round-trips incl. post-action observation: size combobox=112ms, size-option select=150ms, add-to-basket=153ms — vs ~5.1s pre-fix. Add-to-basket XHR succeeded end-to-end: basket indicator advanced to 'Go to basket (2)', button reset from Loading. Running daemon (pid 12103, binary built 11:46 > commit 88d5159 at 11:20) confirmed to carry the fix. Diagnostic confirmed the daemon was NOT stale.
