---
id: 01KV5AZ3ZWQPMRG4J3AR2AT6V8
schema: task/v1
workspace: agent-browser
title: '[browser-friction] wait_for must track freshly-opened tab navigation (no about:blank false-load)'
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
  - at: 2026-06-15T09:49:23.836662Z
    evt: created
    to: open
    by_session: c988fcc9-7e13-4b73-824c-190641734a71
created_at: 2026-06-15T09:49:23Z
updated_at: 2026-06-15T09:49:23Z
---

browser_wait_for(condition:'load') returns ok:true immediately after browser_open while the new tab is still about:blank — it resolves against the pre-navigation document. Should wait until the opened tab has a committed non-blank navigation. Workaround used: manual observe.url poll. GENERIC: post-open readiness.
