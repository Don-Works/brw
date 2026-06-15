---
id: 01KV5AZ40KE5KGJ35X3QWMAPCS
schema: task/v1
workspace: agent-browser
title: '[browser-friction] browser_find must expose visibility + not return dead/duplicate refs'
status: open
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
  - at: 2026-06-15T09:49:23.859537Z
    evt: created
    to: open
    by_session: c988fcc9-7e13-4b73-824c-190641734a71
created_at: 2026-06-15T09:49:23Z
updated_at: 2026-06-15T09:49:23Z
---

find('remove') returned removeItem refs e14/e16 that browser_click rejected as 'not found or not visible'; results lacked visible/in_viewport flags. frontier snapshot gave the correct visible ref. find should include visibility/actionability and rank/return the visible twin (or click should auto-resolve to it).
