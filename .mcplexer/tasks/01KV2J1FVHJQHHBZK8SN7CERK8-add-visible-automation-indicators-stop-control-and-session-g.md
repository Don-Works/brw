---
id: 01KV2J1FVHJQHHBZK8SN7CERK8
schema: task/v1
workspace: agent-browser
title: Add visible automation indicators, stop control, and session grouping
status: done
priority: high
tags:
  - extension-bridge
  - ux
  - tab-groups
  - safety
  - claude-parity
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:55:18.257498Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:35:00.289444Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:36:49.353765Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:55:18Z
updated_at: 2026-06-14T08:36:49Z
---

Claude has in-page running/static indicators, phantom cursor, STOP_AGENT, tab group tracking, and secondary-tab indicators. Design the agent-browser equivalent for visible user trust: extension badge plus optional in-page indicator, stop/pause kill switch, tab-group/session metadata, and no surprise foreground stealing.
