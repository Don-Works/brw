---
id: 01KV58F26AKWJK2NHQADQW5D86
schema: task/v1
workspace: agent-browser
title: Add tab_id support to weak browser tools
status: done
priority: normal
tags:
  - agent-browser
  - mcp
  - browser-tools
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by: 01KV58F25TFBZW7NDV29JN69PN
source:
  kind: agent
  session_id: 0087ab2c-ce4e-4293-8591-c29732f742b4
status_history:
  - at: 2026-06-15T09:05:40.554124Z
    evt: created
    to: doing
    by_session: 0087ab2c-ce4e-4293-8591-c29732f742b4
  - at: 2026-06-15T09:19:06.686848Z
    evt: status_changed
    from: doing
    to: done
    by_session: 0087ab2c-ce4e-4293-8591-c29732f742b4
  - at: 2026-06-15T09:19:06.686848Z
    evt: closed
    to: done
    by_session: 0087ab2c-ce4e-4293-8591-c29732f742b4
created_at: 2026-06-15T09:05:40Z
updated_at: 2026-06-15T09:19:06Z
---

Make evaluate/find/click_xy/wait_for and related helpers honor explicit tab targets to prevent active-window drift.

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
