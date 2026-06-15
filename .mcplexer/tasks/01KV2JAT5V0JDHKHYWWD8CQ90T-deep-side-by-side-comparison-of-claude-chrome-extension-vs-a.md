---
id: 01KV2JAT5V0JDHKHYWWD8CQ90T
schema: task/v1
workspace: agent-browser
title: Deep side-by-side comparison of Claude Chrome extension vs agent-browser extension bridge
status: done
priority: critical
tags:
  - research
  - extension-bridge
  - claude-parity
  - competitive
  - security
  - performance
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T08:00:23.738999Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:39:47.261038Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:41:20.53816Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T08:00:23Z
updated_at: 2026-06-14T08:41:20Z
---

Do a side-by-side deep comparison of the installed Claude Chrome extension and the agent-browser extension bridge. This is a research/review task only unless explicitly expanded later.

Claude extension to inspect:
- /Users/max/Library/Application Support/Google/Chrome/Profile 1/Extensions/fcoeoabgfenejglbffodgkkbkcdhcgfn/1.0.75_0/manifest.json
- /Users/max/Library/Application Support/Google/Chrome/Profile 1/Extensions/fcoeoabgfenejglbffodgkkbkcdhcgfn/1.0.75_0/managed_schema.json
- /Users/max/Library/Application Support/Google/Chrome/Profile 1/Extensions/fcoeoabgfenejglbffodgkkbkcdhcgfn/1.0.75_0/assets/service-worker.ts-BsAUV92e.js
- /Users/max/Library/Application Support/Google/Chrome/Profile 1/Extensions/fcoeoabgfenejglbffodgkkbkcdhcgfn/1.0.75_0/assets/mcpPermissions-8PlHLvdl.js
- /Users/max/Library/Application Support/Google/Chrome/Profile 1/Extensions/fcoeoabgfenejglbffodgkkbkcdhcgfn/1.0.75_0/assets/accessibility-tree.js-CCweLwU2.js
- /Users/max/Library/Application Support/Google/Chrome/Profile 1/Extensions/fcoeoabgfenejglbffodgkkbkcdhcgfn/1.0.75_0/assets/agent-visual-indicator.js-DVYDybPo.js
- /Users/max/Library/Application Support/Google/Chrome/Profile 1/Extensions/fcoeoabgfenejglbffodgkkbkcdhcgfn/1.0.75_0/assets/sidepanel-BL0NRfq2.js

agent-browser extension to inspect:
- /Users/max/github/revitt/brw/agent-browser/extension/manifest.json
- /Users/max/github/revitt/brw/agent-browser/extension/service_worker.js
- /Users/max/github/revitt/brw/agent-browser/extension/README.md
- /Users/max/github/revitt/brw/agent-browser/internal/extensionbridge/bridge.go

Compare at least:
- Manifest permissions, host permissions, CSP, externally_connectable, native messaging, debugger usage, download/offscreen/notification/tabGroup use.
- Transport shape: Claude nativeMessaging/cloud bridge/side panel vs agent-browser local WebSocket/daemon/SSH MCP.
- CDP coverage: Input, Page, Runtime, Network, console, dialogs, downloads, screenshots, tab/window/group operations.
- Page-state model: Claude text accessibility tree + WeakRef refs vs agent-browser semantic refs/snapshots/readability/frontier/events.
- Safety model: permission prompts, domain transition, managed policy, financial/open-web access constraint, sensitive redaction, kill switch, auditability.
- Speed model: batch semantics, page kernel/events, cacheability, action observations, output bytes/tokens, number of round trips.
- UX: side panel, status badge, in-page indicators, phantom cursor, stop action, task/workflow recording.
- Bugs/risk: overbroad permissions, stale refs, focus stealing, popup/tab scoping, prompt injection/extension hijack surfaces, sensitive data leakage.

Deliverable: a matrix with Claude capability, agent-browser equivalent, gap, implementation priority, and whether to copy, avoid, or leapfrog. Include specific file references and quote only tiny snippets when necessary. Do not copy proprietary code verbatim.
