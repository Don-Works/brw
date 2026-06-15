---
id: 01KV2JHR07XGZ61E1CRS68ENRG
schema: task/v1
workspace: agent-browser
title: Move deterministic browser intelligence into the extension and page kernel
status: done
priority: critical
tags:
  - architecture
  - extension-bridge
  - page-kernel
  - token-efficiency
  - browser-speed
  - quality
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T08:04:10.887455Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:39:47.26837Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:41:20.542277Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T08:04:10Z
updated_at: 2026-06-14T08:41:20Z
---

Architecture principle: make the browser extension/page kernel/daemon as compos mentis as possible. Any deterministic browser work that can happen locally in Chrome is faster than a model round trip and costs zero tokens. Prefer local computation for actionability checks, target ranking, stale-ref recovery, semantic diffs, form lenses, assertions, waits, event journals, scroll/container selection, and batch execution.

The model should provide intent, approve ambiguous/high-risk choices, and inspect compact evidence. It should not repeatedly re-read full page state or reason through mechanics the browser runtime can compute exactly.

Acceptance criteria for future browser work:
- Every new model-visible tool asks: can the extension/page kernel do this internally and return a compact result?
- Avoid exposing raw state when a local ranked/filtered/validated answer is possible.
- Prefer one local transaction over multiple model/tool turns.
- Local intelligence must be deterministic, inspectable, and safety-gated; no hidden site-security bypass logic.
- Performance metrics should count model round trips and output tokens avoided, not just wall-clock latency.
