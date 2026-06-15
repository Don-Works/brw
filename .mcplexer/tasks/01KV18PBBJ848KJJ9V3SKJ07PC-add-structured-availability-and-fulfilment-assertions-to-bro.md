---
id: 01KV18PBBJ848KJJ9V3SKJ07PC
schema: task/v1
workspace: agent-browser
title: Add structured availability and fulfilment assertions to browsercheck
status: open
priority: high
tags:
  - agent-browser
  - browsercheck
  - availability
  - stock-check
  - assertions
  - ecommerce
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by: 01KV18NXKHVJYSB50GYPGFGDJ5
source:
  kind: agent
  session_id: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
status_history:
  - at: 2026-06-13T19:52:41.586743Z
    evt: created
    to: open
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
created_at: 2026-06-13T19:52:41Z
updated_at: 2026-06-13T19:52:41.592812Z
---

The Decathlon run showed that generic stock checks are more subtle than seeing green text. A test needs to assert the selected location/postcode, fulfilment mode (delivery / collect / nearby pickup), availability text, and any chosen store when present. Add browsercheck assertions that can verify availability panels generically by semantic labels and text, without site-specific Decathlon heuristics. This should support ecommerce and booking sites where location-specific availability gates checkout.
