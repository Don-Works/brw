---
id: 01KV0CFKHATC0SG83AQAGHTX4Q
schema: task/v1
workspace: agent-browser
title: Add semantic field commit and autocomplete primitives
status: done
priority: high
tags:
  - mcp
  - forms
  - autocomplete
  - browser-speed
pinned: false
assignee:
  origin_kind: local
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
source:
  kind: agent
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
status_history:
  - at: 2026-06-13T11:39:40.458182Z
    evt: created
    to: open
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:39:40.458182Z
    evt: assigned
    to: f8ed800b-c98d-4403-a5ba-ea03a0447722
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-14T08:32:09.443767Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:35:00.257084Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-13T11:39:40Z
updated_at: 2026-06-14T08:35:00Z
---

Add set_field/commit_field style primitive that fills a labeled input/searchbox/combobox, chooses exact autocomplete options when present, commits with Enter/blur when needed, and returns validation/result deltas. This should cover postcode/address/location widgets generically.
