---
id: 01KV2J9CPV3J4X42BANJ0RXNK9
schema: task/v1
workspace: agent-browser
title: Build semantic intent engine and ranked locator explanations
status: done
priority: high
tags:
  - intent-engine
  - locators
  - semantic-refs
  - quality
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:59:37.179021Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:35:00.308124Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:36:49.380196Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:59:37Z
updated_at: 2026-06-14T08:36:49Z
---

Implement query-to-ref ranking over role/name/label/placeholder/value, form ownership, aria controls/describedby/errormessage, nearby text, URL/route context, prior successful actions, and per-origin affordance hints. Return confidence and reasons. Use this for browser_find, fill query, click query, and stale-ref recovery.
