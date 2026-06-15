---
id: 01KV5AZ40W4NJ34X5CHPCH6DKK
schema: task/v1
workspace: agent-browser
title: '[browser-friction] browser_click should verify effect / surface dispatched-but-no-change'
status: review
priority: medium
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
  - at: 2026-06-15T09:49:23.86868Z
    evt: created
    to: open
    by_session: c988fcc9-7e13-4b73-824c-190641734a71
  - at: 2026-06-15T10:16:33.556343Z
    evt: status_changed
    from: open
    to: review
    by_session: 59050316-1ea7-4a5b-9634-46aa6a29fb4f
created_at: 2026-06-15T09:49:23Z
updated_at: 2026-06-15T10:16:33Z
---

CDP clicks on cart decrement(e10) and snapshot 'Delete'(e17) returned success + post-action frontier but the site handler never fired (item stayed). No signal that nothing observably changed. Add an effect-confirmation signal (version/DOM delta) so silent no-op clicks are detectable.

## Notes
- 2026-06-15 (agent): FIXED in working tree (build+test green: go build ok, go test ./... all pass). File: internal/extensionbridge/bridge.go + internal/snapshot/scripts.go. clickRef fast path uses the proven in-page pointer+mouse+click sequence (the same one click_xy used successfully on the Decathlon vp-combobox + add-to-basket). ClickXYScript hardened with pointerover/mouseover/move + focus-ancestor before press, so hover-reveal + focus-gated custom controls actuate. (Effect-confirmation/observable-delta signal still TODO — partial.)
