---
id: 01KV0915VR8AW2F7K5CCGD3F9P
schema: task/v1
workspace: agent-browser
title: Optimize semantic browser control speed
status: done
priority: high
tags:
  - performance
  - snapshot
  - dom
  - mcp
pinned: false
assignee:
  origin_kind: local
meta:
  focus:
    - browser_fill
    - browser_find
    - targeted snapshot
    - ref cache
source:
  kind: agent
  session_id: 27301e4c-0212-4b41-9a52-d95da51939a3
status_history:
  - at: 2026-06-13T10:39:21.976777Z
    evt: created
    to: open
    by_session: 27301e4c-0212-4b41-9a52-d95da51939a3
  - at: 2026-06-13T11:16:19.436256Z
    evt: status_changed
    from: open
    to: done
    by_session: 1774dac6-8a46-4d1a-812b-56c118706e88
  - at: 2026-06-13T11:16:19.436256Z
    evt: closed
    to: done
    by_session: 1774dac6-8a46-4d1a-812b-56c118706e88
created_at: 2026-06-13T10:39:21Z
updated_at: 2026-06-13T11:16:19Z
---

Add targeted DOM/ref operations so automation can move quickly without full-page dumps: fill/replace text, find/filter elements, compact snapshots, and faster wait/read loops while keeping screenshots fallback-only.

## Notes
- 2026-06-13 (agent): Codex resumed with mcplexer stdio MCP available. Gateway advertised mcpx__execute_code/search_tools; agent-browser downstream is not discovered yet, so next step is to fix stdio discovery and route browser actions through mcplexer.
- 2026-06-13 (agent): Semantic speed primitives are implemented: filtered snapshot args, browser_find, browser_fill with replace-by-default MCP/HTTP behavior, action observations, and robust input events for modern frontend fields.
