---
id: 01KV0AY3Q2JKJX5PXG6EKX959M
schema: task/v1
workspace: agent-browser
title: Improve mcplexer tool result rendering for repeated small observations
status: open
priority: normal
tags:
  - mcplexer
  - ux
  - token-efficiency
pinned: false
assignee:
  origin_kind: local
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
meta:
  composed_by: 01KV0AGWKR7R02GH7PM6S8B8KG
source:
  kind: agent
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
status_history:
  - at: 2026-06-13T11:12:38.626746Z
    evt: created
    to: open
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:12:38.626746Z
    evt: assigned
    to: f8ed800b-c98d-4403-a5ba-ea03a0447722
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
created_at: 2026-06-13T11:12:38Z
updated_at: 2026-06-13T11:12:38Z
---

During browser_find loops, multiple print calls and compacted previews made output harder to read. Investigate better single-summary formatting or mcplexer display hints for downstream structuredContent arrays without losing token savings.
