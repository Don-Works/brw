---
id: 01KV5AZ425KR9XSW78RAX2S0NQ
schema: task/v1
workspace: agent-browser
title: '[browser-friction] browser_evaluate should treat void/undefined result as success'
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
  - at: 2026-06-15T09:49:23.909235Z
    evt: created
    to: open
    by_session: c988fcc9-7e13-4b73-824c-190641734a71
  - at: 2026-06-15T10:16:33.565032Z
    evt: status_changed
    from: open
    to: review
    by_session: 59050316-1ea7-4a5b-9634-46aa6a29fb4f
created_at: 2026-06-15T09:49:23Z
updated_at: 2026-06-15T10:16:33Z
---

evaluate('location.reload()') throws 'runtime evaluation returned no by-value result'. Void/side-effecting expressions error instead of succeeding; forces appending ;'ok'. Treat undefined as ok; also verify result is returned consistently for plain expressions vs IIFEs.

## Notes
- 2026-06-15 (agent): FIXED in working tree (build+test green: go build ok, go test ./... all pass). File: internal/extensionbridge/bridge.go + internal/snapshot/scripts.go. bridge.evaluate now treats an empty by-value result (location.reload(), assignments, void calls) as success returning JSON null, instead of erroring 'runtime evaluation returned no by-value result'.
