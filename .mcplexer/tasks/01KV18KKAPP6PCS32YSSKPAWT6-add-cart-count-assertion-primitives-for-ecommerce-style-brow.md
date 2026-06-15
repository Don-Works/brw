---
id: 01KV18KKAPP6PCS32YSSKPAWT6
schema: task/v1
workspace: agent-browser
title: Add cart/count assertion primitives for ecommerce-style browser tests
status: open
priority: high
tags:
  - agent-browser
  - browsercheck
  - ecommerce
  - assertions
  - reliability
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by: 01KV18NXKHVJYSB50GYPGFGDJ5
source:
  kind: agent
  session_id: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
status_history:
  - at: 2026-06-13T19:51:11.446348Z
    evt: created
    to: open
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
created_at: 2026-06-13T19:51:11Z
updated_at: 2026-06-13T19:52:27.516808Z
---

The Decathlon acid test nearly false-passed because add-to-basket succeeded but the session already had one matching item, so mini-basket became Go to basket (2). Generic browser automation needs a reusable assertion/check pattern for cart-like counters and quantity controls: find basket/cart count, read item identity/variant/quantity, compare intended quantity, and correct or fail before checkout. This should not be Decathlon-specific: expose concise semantic assertions for item name substrings, variant substrings, quantity value, subtotal/total, and checkout-stage URL/title. Integrate into browsercheck scenario schema.
