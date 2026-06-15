---
id: 01KV5AZ4179D267ZYVM1AG2DYT
schema: task/v1
workspace: agent-browser
title: '[browser-friction] Unify/auto-fallback transports: click_text bridge vs CDP connectivity'
status: open
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
  - at: 2026-06-15T09:49:23.879342Z
    evt: created
    to: open
    by_session: c988fcc9-7e13-4b73-824c-190641734a71
created_at: 2026-06-15T09:49:23Z
updated_at: 2026-06-15T09:49:23Z
---

browser_click_text failed 'extension bridge is not connected; load/click the Chrome extension first' while CDP-based click/read/evaluate/snapshot worked on the SAME tab same instant. Two transports with divergent health and no fallback. Auto-fallback click_text to CDP text-match, and present one coherent connectivity model.
