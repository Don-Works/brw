---
id: 01KV0AGWN2Q2KK6T9Q0AWXV5HT
schema: task/v1
workspace: agent-browser
title: Verify max-air Decathlon shoe basket and stock flow
status: blocked
priority: high
tags:
  - max-air
  - e2e
  - decathlon
pinned: false
assignee:
  origin_kind: local
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
source:
  kind: agent
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
status_history:
  - at: 2026-06-13T11:05:25.410855Z
    evt: created
    to: open
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:05:25.410855Z
    evt: assigned
    to: f8ed800b-c98d-4403-a5ba-ea03a0447722
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:16:19.482084Z
    evt: status_changed
    from: open
    to: done
    by_session: 1774dac6-8a46-4d1a-812b-56c118706e88
  - at: 2026-06-13T11:16:19.482084Z
    evt: closed
    to: done
    by_session: 1774dac6-8a46-4d1a-812b-56c118706e88
  - at: 2026-06-13T11:18:07.310171Z
    evt: status_changed
    from: done
    to: blocked
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
created_at: 2026-06-13T11:05:25Z
updated_at: 2026-06-13T11:18:07Z
---

Using max-air bridge mode, test Decathlon shoe product flow: local stock around s61tx/S6 1TX and add to basket; stop before checkout/payment.

## Notes
- 2026-06-13 (agent): Verified max-air remote MCP path through mcplexer against a public form page and earlier Decathlon basket/stock flow. Remote profile auth boundary behaves as human-assist, not bypass.
