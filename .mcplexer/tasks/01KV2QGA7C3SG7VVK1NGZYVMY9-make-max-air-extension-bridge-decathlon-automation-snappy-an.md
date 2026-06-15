---
id: 01KV2QGA7C3SG7VVK1NGZYVMY9
schema: task/v1
workspace: agent-browser
title: Make max-air extension bridge Decathlon automation snappy and resilient
status: done
priority: high
tags:
  - agent-browser
  - performance
  - extension-bridge
  - max-air
  - decathlon
  - resilience
pinned: false
assignee:
  origin_kind: local
meta:
  touches_files:
    - agent-browser/internal/extensionbridge/**
    - agent-browser/extension/**
    - agent-browser/internal/http/**
    - agent-browser/tests/**
source:
  kind: agent
  session_id: 13714cf8-8577-4d17-8c73-25ba9dcacfca
status_history:
  - at: 2026-06-14T09:30:46.892554Z
    evt: created
    to: todo
    by_session: 13714cf8-8577-4d17-8c73-25ba9dcacfca
  - at: 2026-06-14T09:32:15.548028Z
    evt: status_changed
    from: todo
    to: doing
    by_session: 13714cf8-8577-4d17-8c73-25ba9dcacfca
  - at: 2026-06-14T09:32:15.548028Z
    evt: assigned
    to: 13714cf8-8577-4d17-8c73-25ba9dcacfca
    by_session: 13714cf8-8577-4d17-8c73-25ba9dcacfca
  - at: 2026-06-14T09:37:47.787112Z
    evt: status_changed
    from: doing
    to: open
    note: lease expired, demoted from working status
  - at: 2026-06-14T09:37:47.787112Z
    evt: lease_expired
  - at: 2026-06-14T09:40:20.638687Z
    evt: status_changed
    from: open
    to: review
    by_session: 13714cf8-8577-4d17-8c73-25ba9dcacfca
  - at: 2026-06-14T16:25:38.28976Z
    evt: status_changed
    from: review
    to: done
    by_session: 625e0536-e114-4eed-a254-5d65131de659
  - at: 2026-06-14T16:25:38.28976Z
    evt: closed
    to: done
    by_session: 625e0536-e114-4eed-a254-5d65131de659
created_at: 2026-06-14T09:30:46Z
updated_at: 2026-06-14T16:25:38Z
---

Observed live Decathlon demo on max-air is functionally correct but too slow: semantic search/product navigation works, but visible bridge actions/read/snapshot often take multiple seconds, and the extension websocket can disconnect until its service worker wakes/reloads. Fix both latency and reconnect resilience. Add local regression tests that fail on slow fallback paths and disconnected/reconnect handling where possible. Use Decathlon-style live/fixture flows as acceptance surface: search, category/product navigation, size/cart flow should feel snappy on max-air.

## Notes
- 2026-06-14 (agent): Work started locally after max-air went offline. Failure notes from live run: Decathlon visible control works, but the extension bridge disconnected until service worker wake/reload, and the live driver had to fall back to broad snapshot discovery because find(query/searchbox) missed the visible header searchbox while snapshot(mode=all) found it. Next: fix extension bridge resilience/latency locally, add regression tests, then commit/push.
- 2026-06-14 (agent): Local fix implemented and verified before commit:
  - Extension bridge snapshot cache is now keyed by snapshot options, fixing stale cache reuse that made live Decathlon find(role=searchbox) miss the visible searchbox until mode=all was forced.
  - browser_batch/browser_plan on the extension bridge now use raw action primitives and take only one final observation, instead of calling observed wrappers for every step.
  - Extension reconnect cadence hardened: connect alarm ensured on startup/load, alarm period reduced to 0.5 min, websocket keepalive reduced to 5s.
  - Direct CDP WaitFor now retries transient navigation execution-context teardown inside the caller deadline.
  - Bridge protocol/manifest bumped to 0.1.6.
  Verification: node --check extension/service_worker.js passed; go test -count=1 ./... passed; local headless core browsercheck passed 12 passed / 7 expected skips / 0 failed; local Decathlon fixture passed 1 passed / 0 failed.
- 2026-06-14 (agent): Committed and pushed isolated fix as 484dea4 (fix: speed up extension bridge batches) to origin/main. Max-air went offline before redeploy/retest, so remaining validation is: deploy 0.1.6 to max-air, reload extension, verify bridge connected, then rerun live Decathlon/batch demo and max-air core/Decathlon browsercheck.
- 2026-06-14 (agent): Closed after max-air MCP route repair and live verification. Runtime route agent-browser-max-air was misconfigured to start a second extension bridge with --bridge --bridge-addr 127.0.0.1:17311, which crashed with bind: address already in use and surfaced as "no response from downstream". Updated the registered downstream command to the upstream wrapper: agent-browserd --mcp --http off --upstream-http http://127.0.0.1:17310 over ssh to max-air. Reloaded mcplexer server catalog: 34 tools refreshed. Verified through agent_browser MCP, not direct HTTP: browser_list_tabs ok 19ms, browser_open example.com ok 36ms, browser_read ok 131ms. Gmail Boldking test through agent_browser MCP returned 25 messages and newest five subjects. Previous max-air 0.1.7 validation remains green: Decathlon 1 passed/0 failed in 2.6s; core suite 12 passed/7 skipped/0 failed; commit 0c12a6d pushed to origin/main.
