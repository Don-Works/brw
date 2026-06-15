---
id: 01KV5EQDFPFS25T75FD4YXXY7S
schema: task/v1
workspace: agent-browser
title: '[browser-speed] Apply in-page fast-click to direct-CDP Manager.Click (clickElementCenter)'
status: done
priority: normal
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by: 01KV5CKY1ZVK9TX3NXZPRP5VGE
  touches_files:
    - agent-browser/internal/browser/manager.go
source:
  kind: agent
  session_id: eec9898e-0967-4c8d-a37f-bf0d3612b52d
status_history:
  - at: 2026-06-15T10:55:05.718839Z
    evt: created
    to: doing
    by_session: eec9898e-0967-4c8d-a37f-bf0d3612b52d
  - at: 2026-06-15T11:09:39.365194Z
    evt: status_changed
    from: doing
    to: review
    by_session: eec9898e-0967-4c8d-a37f-bf0d3612b52d
  - at: 2026-06-15T11:15:06.722522Z
    evt: status_changed
    from: review
    to: done
    by_session: eec9898e-0967-4c8d-a37f-bf0d3612b52d
created_at: 2026-06-15T10:55:05Z
updated_at: 2026-06-15T11:15:06Z
---

## Notes
- 2026-06-15 (agent): Done + bonus root-cause. (1) clickElementCenter now actuates via in-page snapshot.ClickXY (one Runtime.evaluate) with chromedp.MouseClickXY as fallback — mirrors bridge.clickRef. clickElementCenter dropped ~900ms→155ms (150ms of that is the deliberate post-click settle). (2) BENCH REVEALED the real dominant cost was NOT CDP dispatch but snapshot.WaitForActionable: its setInterval(100ms) stability poll was clamped to ~1Hz by Chrome background-timer-throttling on hidden/headless pages → ~700-900ms/click. Fixed two ways: setTimeout(16) chain instead of setInterval/rAF (rAF is PAUSED, not just throttled, on hidden pages), AND added --disable-background-timer-throttling/--disable-backgrounding-occluded-windows/--disable-renderer-backgrounding to cdp/launcher.go defaults. RESULT: direct-CDP bench clicks ~900-1000ms → ~172ms each; total bench 3057ms→716ms. go build + go test ./... all green. NO focus-stealing required — the fix is launch flags + event-paced waits. Files: internal/browser/manager.go, internal/snapshot/scripts.go, internal/cdp/launcher.go. Uncommitted.
