---
id: 01KV5MSGHYYGRXPRWA6NGX1S2M
schema: memory/v1
kind: note
name: operational-gotcha-agentbrowser-real-chrome-via-df4d569c
workspace: agent-browser
tags:
  - agent-browser
  - extension-bridge
  - daemon-restart
  - operational
  - debugging
  - cdp
pinned: false
source:
  kind: agent
  session_id: eec9898e-0967-4c8d-a37f-bf0d3612b52d
created_at: 2026-06-15T12:41:05Z
updated_at: 2026-06-15T12:41:05Z
---

OPERATIONAL GOTCHA (agent-browser, real Chrome via extension bridge): killing/restarting the agent-browserd daemon to load a new binary disconnects the Chrome extension from the bridge (127.0.0.1:17311). The extension does NOT auto-reconnect within minutes — browser_* calls then fail with 'extension bridge is not connected; load/click the Chrome extension first'. The mcplexer ROUTE itself recovers fine (mcplexer respawns the stdio child; mcpx.reload_server({}) re-introspects the new tool catalog), but the extension->bridge link needs the user to click/reload the extension in Chrome. IMPLICATIONS: (1) live extension-bridge smoke-testing is blocked right after a daemon restart until the user reactivates the extension; (2) prefer verifying via the direct-CDP browsercheck --bench (launches its own Chrome, independent of the bridge) + the Go test suite, which need no extension; (3) batch binary swaps so you only restart once, ideally when the user is present to re-click the extension. Related: [[chrome-hidden-page-timer-throttling-breaks-in-page-poll-waits-agent-browser]]. Follow-up idea: have the extension service worker aggressively retry the bridge socket, or add a bridge-health/auto-reconnect path (ties to tracked task 'Expose downstream route health and restart diagnostics').
