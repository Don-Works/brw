---
id: 01KV0915VW83WVC0WFB125N12W
schema: task/v1
workspace: agent-browser
title: Final local and remote test review
status: done
priority: high
tags:
  - testing
  - review
  - max-air
  - max-mac
  - ai-gateway
pinned: false
assignee:
  origin_kind: local
meta:
  requires:
    - go test ./...
    - go vet ./...
    - mcplexer discovery
    - remote MCP smoke tests
source:
  kind: agent
  session_id: 27301e4c-0212-4b41-9a52-d95da51939a3
status_history:
  - at: 2026-06-13T10:39:21.980715Z
    evt: created
    to: open
    by_session: 27301e4c-0212-4b41-9a52-d95da51939a3
  - at: 2026-06-13T11:16:19.447875Z
    evt: status_changed
    from: open
    to: done
    by_session: 1774dac6-8a46-4d1a-812b-56c118706e88
  - at: 2026-06-13T11:16:19.447875Z
    evt: closed
    to: done
    by_session: 1774dac6-8a46-4d1a-812b-56c118706e88
created_at: 2026-06-13T10:39:21Z
updated_at: 2026-06-13T11:16:19Z
---

Run local tests/vet/builds, discover through mcplexer, test remote max-air and max-mac profile control via MCP, then produce a repeatability and architecture review.

## Notes
- 2026-06-13 (agent): Codex resumed with mcplexer stdio MCP available. Gateway advertised mcpx__execute_code/search_tools; agent-browser downstream is not discovered yet, so next step is to fix stdio discovery and route browser actions through mcplexer.
- 2026-06-13 (agent): Final verification completed: go test ./..., go vet ./..., local stdio MCP upstream wrapper tools/list + browser_list_tabs, and remote max-air MCP open/wait/find/fill/snapshot smoke test.
