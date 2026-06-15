---
id: 01KV0915VM3Q4R3TW0NWCP7Q52
schema: task/v1
workspace: agent-browser
title: Make agent-browser first-class mcplexer downstream
status: done
priority: critical
tags:
  - mcplexer
  - stdio
  - profile-policy
  - max-air
  - ai-gateway
pinned: false
assignee:
  origin_kind: local
meta:
  touches_files:
    - agent-browser/internal/mcp/server.go
    - agent-browser/cmd/agent-browserctl/main.go
    - .mcplexer/config/browser-profiles.json
    - .mcplexer/config/mcp-servers.json
source:
  kind: agent
  session_id: 27301e4c-0212-4b41-9a52-d95da51939a3
status_history:
  - at: 2026-06-13T10:39:21.972273Z
    evt: created
    to: doing
    by_session: 27301e4c-0212-4b41-9a52-d95da51939a3
  - at: 2026-06-13T11:16:19.416474Z
    evt: status_changed
    from: doing
    to: done
    by_session: 1774dac6-8a46-4d1a-812b-56c118706e88
  - at: 2026-06-13T11:16:19.416474Z
    evt: closed
    to: done
    by_session: 1774dac6-8a46-4d1a-812b-56c118706e88
created_at: 2026-06-13T10:39:21Z
updated_at: 2026-06-13T11:16:19Z
---

Make agent-browser visible and configurable as a standard stdio MCP server in mcplexer. Ensure workspace-to-Chrome-profile mapping is explicit, profile pinning is env/config driven, and max-air/max-mac/ai-gateway transports are repeatable.

## Notes
- 2026-06-13 (agent): Codex resumed with mcplexer stdio MCP available. Gateway advertised mcpx__execute_code/search_tools; agent-browser downstream is not discovered yet, so next step is to fix stdio discovery and route browser actions through mcplexer.
- 2026-06-13 (agent): mcplexer downstream fixed: agent-browser-max-air now discovers 14 tools through mcpx.reload_server. Root causes: daemon ssh could not read ~/.ssh/known_hosts; moved host key state to agent-browser app dir, then retried after the first bridge listener was up. Normal project search now shows agent_browser__browser_* tools.
- 2026-06-13 (agent): agent-browser-max-air is a first-class mcplexer stdio downstream for workspace agent-browser. It exposes browser_find/browser_fill plus the original browser tools through mcplexer routing, with profile pinning via AGENT_BROWSER_WORKSPACE/PROFILE/PROFILE_POLICY.
