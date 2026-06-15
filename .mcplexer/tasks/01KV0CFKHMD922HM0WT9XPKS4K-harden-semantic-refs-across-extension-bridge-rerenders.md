---
id: 01KV0CFKHMD922HM0WT9XPKS4K
schema: task/v1
workspace: agent-browser
title: Harden semantic refs across extension bridge rerenders
status: done
priority: high
tags:
  - mcp
  - refs
  - extension-bridge
  - reliability
pinned: false
assignee:
  origin_kind: local
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
source:
  kind: agent
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
status_history:
  - at: 2026-06-13T11:39:40.468698Z
    evt: created
    to: open
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:39:40.468698Z
    evt: assigned
    to: f8ed800b-c98d-4403-a5ba-ea03a0447722
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-14T08:30:41.302914Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:32:09.428701Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-13T11:39:40Z
updated_at: 2026-06-14T08:32:09Z
---

Store richer ref fingerprints and re-resolution data: stable selector path, label/name/type, form ancestry, optional backend node id where available. Re-resolve refs across extension bridge isolated worlds, rerenders, and attributes that do not persist.
