---
id: 01KV14NWX71KNYKN1KQ4RNCXCD
schema: task/v1
workspace: agent-browser
title: Harden generic select/activation success verification
status: done
priority: high
tags:
  - agent-browser
  - efficiency
  - automation
  - generic
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by:
    - 01KV0AGWKR7R02GH7PM6S8B8KG
    - 01KV2J1FSZGRNF6EPK977AKDDD
  touches_files:
    - agent-browser/internal/snapshot/scripts.go
    - agent-browser/internal/browser/manager.go
    - agent-browser/internal/extensionbridge/bridge.go
    - agent-browser/internal/browser/types.go
    - agent-browser/tests/fixtures/*
    - agent-browser/tests/scenarios/core.json
source:
  kind: agent
  session_id: e35c872e-ae0e-4910-9901-bd30cbc0347d
status_history:
  - at: 2026-06-13T18:42:32.487335Z
    evt: created
    to: doing
    by_session: e35c872e-ae0e-4910-9901-bd30cbc0347d
  - at: 2026-06-13T18:42:32.487335Z
    evt: assigned
    to: e35c872e-ae0e-4910-9901-bd30cbc0347d
    by_session: e35c872e-ae0e-4910-9901-bd30cbc0347d
  - at: 2026-06-13T18:42:55.313895Z
    evt: status_changed
    from: doing
    to: open
    note: lease expired, demoted from working status
  - at: 2026-06-13T18:42:55.313895Z
    evt: lease_expired
  - at: 2026-06-13T19:06:25.272743Z
    evt: status_changed
    from: open
    to: done
    by_session: 920d1707-1199-4611-a29e-71740df38255
  - at: 2026-06-13T19:06:25.272743Z
    evt: closed
    to: done
    by_session: 920d1707-1199-4611-a29e-71740df38255
created_at: 2026-06-13T18:42:32Z
updated_at: 2026-06-14T07:55:18.351456Z
---

Decathlon exposed a generic failure mode: browser_select/browser_click can return ok even when a custom ARIA combobox/listbox or button activation does not actually commit page state.

Acceptance:
- Select helper checks returned script status and surfaces errors instead of unconditional ok.
- Custom ARIA combobox/listbox option selection is supported generically via refs/roles/aria-controls/aria-activedescendant, without site-specific heuristics.
- Action observations include a clear no-change/verification signal when an action executes but the semantic state is unchanged.
- Static fixture covers custom combobox -> commit -> action result, independent of Decathlon.

## Notes
- 2026-06-13 (agent): Shipped generic action/select hardening. Changes: bridge now checks SelectElementScript {ok,error}; native select verifies requested option by value/text; custom ARIA combobox/listbox selection finds visible role=option and clicks it; select supports an already-selected/idempotent state when the element exposes the selected value; click sends pointer move before press/release in bridge mode; action results include changed_state plus a no-change warning. Static custom-combobox fixture added and verified locally and through max-air MCP. Decathlon size combobox now commits UK 13C EU32 generically; add-to-basket outcome remains a separate real-site activation/outcome issue.
