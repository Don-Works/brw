---
id: 01KV2J1FTQH5P63PS5CRFKQJV7
schema: task/v1
workspace: agent-browser
title: Redact sensitive form values in snapshots and readability output
status: done
priority: critical
tags:
  - security
  - snapshots
  - privacy
  - claude-parity
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:55:18.231797Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:17:05.832179Z
    evt: status_changed
    from: open
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:55:18Z
updated_at: 2026-06-14T08:17:05Z
---

Claude redacts password, hidden, one-time-code, and credit-card-like fields in its accessibility tree. Our snapshot/readability paths currently expose ordinary input values. Add default redaction for password/OTP/credit-card/CVC/expiry/autocomplete-sensitive fields, while preserving labels/names so agents can still target fields. Add fixture coverage.
