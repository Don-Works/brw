---
id: 01KV58F25TFBZW7NDV29JN69PN
schema: task/v1
workspace: agent-browser
title: Harden browser MCP shopping-flow ergonomics after Decathlon cart test
status: done
priority: normal
tags:
  - agent-browser
  - mcp
  - browser-tools
  - decathlon
  - ergonomics
pinned: false
assignee:
  origin_kind: local
composes:
  - 01KV58F26AKWJK2NHQADQW5D86
  - 01KV58F26M0ZZNXJDPV8YCKT88
  - 01KV58F26ZEXH27075GX4ZGK7Y
  - 01KV58F278MGCKGRB6WJV8CS74
meta:
  requested_by: user
  source: live Decathlon cart test
  touches_files:
    - agent-browser/**
source:
  kind: agent
  session_id: 0087ab2c-ce4e-4293-8591-c29732f742b4
status_history:
  - at: 2026-06-15T09:05:40.538527Z
    evt: created
    to: doing
    by_session: 0087ab2c-ce4e-4293-8591-c29732f742b4
  - at: 2026-06-15T09:05:40.554789Z
    evt: composed
    to: 01KV58F26AKWJK2NHQADQW5D86
    by_session: 0087ab2c-ce4e-4293-8591-c29732f742b4
  - at: 2026-06-15T09:05:40.564485Z
    evt: composed
    to: 01KV58F26M0ZZNXJDPV8YCKT88
    by_session: 0087ab2c-ce4e-4293-8591-c29732f742b4
  - at: 2026-06-15T09:05:40.575611Z
    evt: composed
    to: 01KV58F26ZEXH27075GX4ZGK7Y
    by_session: 0087ab2c-ce4e-4293-8591-c29732f742b4
  - at: 2026-06-15T09:05:40.585935Z
    evt: composed
    to: 01KV58F278MGCKGRB6WJV8CS74
    by_session: 0087ab2c-ce4e-4293-8591-c29732f742b4
  - at: 2026-06-15T09:19:06.70093Z
    evt: status_changed
    from: doing
    to: done
    by_session: 0087ab2c-ce4e-4293-8591-c29732f742b4
  - at: 2026-06-15T09:19:06.70093Z
    evt: closed
    to: done
    by_session: 0087ab2c-ce4e-4293-8591-c29732f742b4
created_at: 2026-06-15T09:05:40Z
updated_at: 2026-06-15T09:19:06Z
---

Implement improvements discovered during the visible Decathlon flow: explicit tab scoping for action/read helpers, robust visible-text clicking, better custom combobox/listbox selection, and richer post-action observations/safety signals.

## Notes
- 2026-06-15 (agent): Implemented and verified ergonomics hardening:
  - Exposed optional tab_id consistently in MCP schemas and propagated tab_id through HTTP upstream client requests.
  - Added browser_click_text with visible/actionable scoring and tab-scoped post-action observation.
  - Added tab_id to ActionResult and BatchResult observations.
  - Snapshot option values now include data-value/value attributes, fixing ARIA option selection by hidden data value.
  - Added checkout/payment warning text for click_text on checkout/place-order-style controls.
  
  Verification:
  - go test ./... passed.
  - browsercheck fixture-custom-aria-combobox-commit passed.
  - browsercheck decathlon-basket-flow passed.
  - Live MCP test selected custom combobox value stable via browser_select(tab_id), clicked Continue with browser_click_text(tab_id), and read Result: committed channel stable.
  - Live MCP checkout-label test returned warning: checkout navigation clicked; stop before payment or place-order controls unless explicitly confirmed.
