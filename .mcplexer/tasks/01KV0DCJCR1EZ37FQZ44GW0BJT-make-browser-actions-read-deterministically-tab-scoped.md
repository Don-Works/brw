---
id: 01KV0DCJCR1EZ37FQZ44GW0BJT
schema: task/v1
workspace: agent-browser
title: Make browser actions/read deterministically tab-scoped
status: done
priority: critical
tags:
  - mcp
  - tabs
  - extension-bridge
  - reliability
  - browser-speed
pinned: false
assignee:
  origin_kind: local
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
source:
  kind: agent
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
status_history:
  - at: 2026-06-13T11:55:29.560215Z
    evt: created
    to: open
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:55:29.560215Z
    evt: assigned
    to: f8ed800b-c98d-4403-a5ba-ea03a0447722
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-14T08:29:19.832326Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:30:41.289665Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-13T11:55:29Z
updated_at: 2026-06-14T08:30:41Z
---

During max-air sign-off, browser_open returned a fresh Selenium tab id and browser_focus_tab was called before each step, but final browser_read still returned the Decathlon product tab. The tool surface is too active-tab dependent. Add tab_id support to read/snapshot/find/fill/click/upload/select/wait, or make the extension bridge active-tab target update deterministic after focus_tab/open. Acceptance: a batch can open Selenium, operate it, and read the submitted URL while unrelated tabs remain open.
