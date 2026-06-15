---
id: 01KV5CKY1ZVK9TX3NXZPRP5VGE
schema: task/v1
workspace: agent-browser
title: '[browser-speed] Out-of-box latency+token wins + benchmark harness'
status: review
priority: high
pinned: false
assignee:
  origin_kind: local
composes:
  - 01KV5EQDFPFS25T75FD4YXXY7S
meta:
  composed_by: 01KV5A3X61WX1RDN0HYDZT2GYT
  kind: speed-epic-child
  source: user-ask-2026-06-15
source:
  kind: agent
  session_id: 59050316-1ea7-4a5b-9634-46aa6a29fb4f
status_history:
  - at: 2026-06-15T10:18:14.463671Z
    evt: created
    to: open
    by_session: 59050316-1ea7-4a5b-9634-46aa6a29fb4f
  - at: 2026-06-15T10:46:19.842514Z
    evt: status_changed
    from: open
    to: review
    by_session: 59050316-1ea7-4a5b-9634-46aa6a29fb4f
  - at: 2026-06-15T10:55:05.719633Z
    evt: composed
    to: 01KV5EQDFPFS25T75FD4YXXY7S
    by_session: eec9898e-0967-4c8d-a37f-bf0d3612b52d
created_at: 2026-06-15T10:18:14Z
updated_at: 2026-06-15T10:55:05.719636Z
---

Brainstorm + benchmark generic speedups for agent-browser (no site-specifics). Tracks: (1) in-page macro runtime (inject actuator once, call by name; stop re-shipping multi-KB scripts); (2) network/API shortcut path (replay site XHR/JSON + read __NEXT_DATA__/JSON-LD via fetch instead of DOM render); (3) direct-CDP vs bridge hop; (4) event-driven settle (drop fixed 75ms; MutationObserver quiet-window); (5) hammerspoon window-sizing to stop off-canvas control clipping (the guest-button bug) + OS fallback actuation; (6) ref->element cache to avoid full shadow-DOM walk per resolveBox; (7) token-side: delta-only results, columnar/compact, single-intent macro tools. Build a browsercheck-based benchmark scorecard: per-action wall-clock + returned-byte count, current vs fixed vs shortcut paths, on fixtures + 1-2 real sites. Optionally delegate an implementation+benchmark spike to an external model (grok).

## Notes
- 2026-06-15 (agent): BENCH DELIVERED + COMMITTED (791151e). browsercheck --bench scorecard, custom-combobox.html, direct-CDP headless: open 25ms/196B, wait 0ms, snapshot 2ms/1895B, find 1ms/582B, read 1ms/377B, click_continue_blocked 1081ms, click_combobox 998ms, select_option 172ms, click_continue 822ms, assert 0ms. TOTAL 3102ms/7861B. FINDING: observation primitives 1-2ms; clicks ~0.8-1.1s EACH via direct-CDP on a trivial local fixture => the in-page fast-click fix (applied to bridge clickRef) must ALSO be applied to internal/browser Manager.Click (direct-CDP). High-value follow-up.
- 2026-06-15 (agent): BOTH SPIKES LANDED on fix/browser-click-latency-actuation (build+test green): bench 791151e; browser_read_data fast-read fc1cd5d (merged 7938a14, worktree removed). Reviews: bench 86, fast-read 90. FOLLOW-UPS: (a) apply in-page fast-click to internal/browser Manager.Click (bench shows ~0.8-1.1s/click direct-CDP too); (b) live-smoke browser_read_data on real product page; (c) network XHR observe/replay; (d) bench multi-run averaging; (e) browsercheck read_data scenario.
