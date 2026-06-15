---
id: 01KV0CFKGA4PQ78GXPG6DEPDG8
schema: task/v1
workspace: agent-browser
title: Add browser_observe semantic event journal
status: done
priority: high
tags:
  - mcp
  - browser-speed
  - event-journal
  - token-efficiency
pinned: false
assignee:
  origin_kind: local
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
source:
  kind: agent
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
status_history:
  - at: 2026-06-13T11:39:40.426779Z
    evt: created
    to: open
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:39:40.426779Z
    evt: assigned
    to: f8ed800b-c98d-4403-a5ba-ea03a0447722
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-14T08:24:43.297842Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:27:52.985951Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-13T11:39:40Z
updated_at: 2026-06-14T08:27:52Z
---

Add daemon-side event journal for DOM mutations, focus changes, open dialogs/drawers, live-region text, URL/title changes, and selected lightweight network events. Every action should return a cursor/version; browser_observe({since, filters}) returns compact changed parts instead of a full snapshot.
