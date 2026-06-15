---
id: 01KV5AZ41TBW13S3AP0RWT64QE
schema: task/v1
workspace: agent-browser
title: '[browser-friction] Semantic cache stale: page-state version never bumps on SPA mutations'
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
  - at: 2026-06-15T09:49:23.898866Z
    evt: created
    to: open
    by_session: c988fcc9-7e13-4b73-824c-190641734a71
created_at: 2026-06-15T09:49:23Z
updated_at: 2026-06-15T09:49:23Z
---

ROOT CAUSE finding. observe.version stuck at 1 across real DOM mutations; browser_read kept returning pre-mutation main-text ('1 item') long after live DOM showed '0 item'. read/snapshot cache keyed on a version that doesn't increment on SPA DOM changes → serves stale content. Invalidate/bump on MutationObserver + SPA route change.
