---
id: 01KV5A3X61WX1RDN0HYDZT2GYT
schema: task/v1
workspace: agent-browser
title: browser optimisation
status: open
priority: high
tags:
  - epic
  - browser
  - optimisation
  - agent-browser
pinned: false
assignee:
  origin_kind: local
composes:
  - 01KV5AGZYNKGEQPPDJ149K1X9Y
  - 01KV5AZ3ZWQPMRG4J3AR2AT6V8
  - 01KV5AZ408QRBQW7HE5MNGCQZF
  - 01KV5AZ40KE5KGJ35X3QWMAPCS
  - 01KV5AZ40W4NJ34X5CHPCH6DKK
  - 01KV5AZ4179D267ZYVM1AG2DYT
  - 01KV5AZ41TBW13S3AP0RWT64QE
  - 01KV5AZ425KR9XSW78RAX2S0NQ
  - 01KV5CKY1ZVK9TX3NXZPRP5VGE
meta:
  focus: general browser automation ergonomics and speed
  kind: epic
  not_site_specific: true
source:
  kind: agent
  session_id: 0087ab2c-ce4e-4293-8591-c29732f742b4
status_history:
  - at: 2026-06-15T09:34:32.129062Z
    evt: created
    to: open
    by_session: 0087ab2c-ce4e-4293-8591-c29732f742b4
  - at: 2026-06-15T09:41:40.951145Z
    evt: composed
    to: 01KV5AGZYNKGEQPPDJ149K1X9Y
    by_session: e38a3838-7a61-4a9e-be28-d1fa62656aad
  - at: 2026-06-15T09:49:23.837239Z
    evt: composed
    to: 01KV5AZ3ZWQPMRG4J3AR2AT6V8
    by_session: c988fcc9-7e13-4b73-824c-190641734a71
  - at: 2026-06-15T09:49:23.849571Z
    evt: composed
    to: 01KV5AZ408QRBQW7HE5MNGCQZF
    by_session: c988fcc9-7e13-4b73-824c-190641734a71
  - at: 2026-06-15T09:49:23.859879Z
    evt: composed
    to: 01KV5AZ40KE5KGJ35X3QWMAPCS
    by_session: c988fcc9-7e13-4b73-824c-190641734a71
  - at: 2026-06-15T09:49:23.869014Z
    evt: composed
    to: 01KV5AZ40W4NJ34X5CHPCH6DKK
    by_session: c988fcc9-7e13-4b73-824c-190641734a71
  - at: 2026-06-15T09:49:23.879776Z
    evt: composed
    to: 01KV5AZ4179D267ZYVM1AG2DYT
    by_session: c988fcc9-7e13-4b73-824c-190641734a71
  - at: 2026-06-15T09:49:23.899407Z
    evt: composed
    to: 01KV5AZ41TBW13S3AP0RWT64QE
    by_session: c988fcc9-7e13-4b73-824c-190641734a71
  - at: 2026-06-15T09:49:23.909588Z
    evt: composed
    to: 01KV5AZ425KR9XSW78RAX2S0NQ
    by_session: c988fcc9-7e13-4b73-824c-190641734a71
  - at: 2026-06-15T10:18:14.485633Z
    evt: composed
    to: 01KV5CKY1ZVK9TX3NXZPRP5VGE
    by_session: 59050316-1ea7-4a5b-9634-46aa6a29fb4f
created_at: 2026-06-15T09:34:32Z
updated_at: 2026-06-15T10:18:14.48564Z
---

Epic for making agent-browser feel fast and competent on difficult real-world sites without adding site-specific or ecommerce-only heuristics.

Problem statement:
The Decathlon test exposed too much low-level browser plumbing in normal use: tab targeting recovery, sparse semantic reads, fragile custom-control activation, weak post-action confirmation, and too many serial observe/find/evaluate cycles. The browser agent should expose sensible browser automation primitives that work across ordinary web apps, shops, forms, dashboards, and search flows.

Principles:
- Do not build ecommerce-first heuristics.
- Improve general browser actionability, state extraction, waiting, target scoping, and compound operations.
- Keep the visible-browser, human-takeover, normal-profile model intact.
- Prefer semantic DOM/browser state over screenshots as the primary state model.

Candidate workstreams:
- Make tab creation/focus/target IDs consistent and cheap to use.
- Improve browser_read so important visible controls, selected state, badges, dialogs, drawers, and action results are surfaced reliably.
- Add better actionability/autowait around click, select, fill, and text-click.
- Make custom controls such as listbox, combobox, menu, disclosure, and modal flows selectable without ref archaeology.
- Add compound browser operations that perform action plus internal verification in one tool call.
- Improve post-action observations so badge/count/text/navigation changes are detected and explained.
- Add performance traces/scorecards for round trips, wait time, extraction time, and avoidable retries.
- Extend browsercheck coverage with hard generic fixtures and selected real-site smoke tests.
