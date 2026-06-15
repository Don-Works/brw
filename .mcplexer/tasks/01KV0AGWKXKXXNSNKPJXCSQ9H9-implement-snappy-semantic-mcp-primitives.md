---
id: 01KV0AGWKXKXXNSNKPJXCSQ9H9
schema: task/v1
workspace: agent-browser
title: Implement snappy semantic MCP primitives
status: blocked
priority: high
tags:
  - implementation
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
status_history:
  - at: 2026-06-13T11:05:25.373894Z
    evt: created
    to: doing
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:05:25.373894Z
    evt: assigned
    to: f8ed800b-c98d-4403-a5ba-ea03a0447722
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:05:58.159628Z
    evt: status_changed
    from: doing
    to: open
    note: lease expired, demoted from working status
  - at: 2026-06-13T11:05:58.159628Z
    evt: lease_expired
  - at: 2026-06-13T11:16:19.468044Z
    evt: status_changed
    from: open
    to: done
    by_session: 1774dac6-8a46-4d1a-812b-56c118706e88
  - at: 2026-06-13T11:16:19.468044Z
    evt: closed
    to: done
    by_session: 1774dac6-8a46-4d1a-812b-56c118706e88
  - at: 2026-06-13T11:18:07.301837Z
    evt: status_changed
    from: done
    to: blocked
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
created_at: 2026-06-13T11:05:25Z
updated_at: 2026-06-13T11:18:07Z
---

Compact+structured MCP output, browser_find/browser_fill, snapshot options, lazy AX, post-action observations.

## Notes
- 2026-06-13 (agent): Core implementation patched and go test ./... plus make build are green locally. Local fixture smoke is in progress; max-air Decathlon e2e follows.
- 2026-06-13 (agent): Implemented snappy semantic MCP primitives across daemon manager, extension bridge, HTTP API, stdio MCP tools, and tests.
