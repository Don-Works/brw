---
id: 01KV2J52VMM9MHHHASVCN5W2K8
schema: task/v1
workspace: agent-browser
title: Preserve open-web and financial-site access in browser policy design
status: done
priority: critical
tags:
  - policy
  - open-web
  - financial-sites
  - safety
  - agent-browser
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:57:16.02013Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:35:00.301474Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:36:49.375827Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:57:16Z
updated_at: 2026-06-14T08:36:49Z
---

Product constraint: preserve open-web browsing, including financial pages. Do not copy Claude-style URL category blocking as a default product behavior. Agent-browser should not decide broad categories of the web are off-limits; that is an anti-web pattern and would undermine the value of controlling the user's real browser profile.

Safety should be expressed as user/workspace-owned policy and action semantics: explicit profile/workspace authorization, transparent audit logs, pause/kill switch, sensitive-value redaction, domain-drift warnings, and human handoff for MFA/CAPTCHA/fraud/consent gates. Financial sites must remain reachable and controllable when the user has authorized the profile/session, while the agent must not bypass site security controls.

Acceptance criteria:
- No default vendor/category blocklist that prevents browsing financial, healthcare, government, or other high-risk-but-legitimate sites.
- Admin/user blocklists remain possible only as explicit local policy, not hardcoded product judgment.
- High-risk handling focuses on transparent confirmations, auditability, and manual handoff for MFA/CAPTCHA/fraud checks.
- Tests/docs make clear that financial pages are supported when the user-authorized Chrome profile can access them.
