---
id: 01KV2J1FVR7YVCFPD8JGH95SNZ
schema: task/v1
workspace: agent-browser
title: Add policy and permission gates inspired by Claude managed controls
status: done
priority: high
tags:
  - policy
  - permissions
  - security
  - claude-parity
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 6305571c-bd47-4d55-9849-77e043e8753a
status_history:
  - at: 2026-06-14T07:55:18.264564Z
    evt: created
    to: open
    by_session: 6305571c-bd47-4d55-9849-77e043e8753a
  - at: 2026-06-14T08:35:00.296131Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:36:49.37054Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-14T07:55:18Z
updated_at: 2026-06-14T08:36:49Z
---

Claude supports managed blockedUrlPatterns, forceLoginOrgUUID, URL category checks, per-action permission prompts, and domain-transition checks. Extend agent-browser profile policy with site/path blocklists or allowlists, domain transition warnings, high-risk action confirmation hooks, denied-action semantics, and audit-friendly errors. Preserve current no-bypass safety boundary.

## Notes
- 2026-06-14 (agent): Important constraint from Max:
  
  Product constraint: preserve open-web browsing, including financial pages. Do not copy Claude-style URL category blocking as a default product behavior. Agent-browser should not decide broad categories of the web are off-limits; that is an anti-web pattern and would undermine the value of controlling the user's real browser profile.
  
  Safety should be expressed as user/workspace-owned policy and action semantics: explicit profile/workspace authorization, transparent audit logs, pause/kill switch, sensitive-value redaction, domain-drift warnings, and human handoff for MFA/CAPTCHA/fraud/consent gates. Financial sites must remain reachable and controllable when the user has authorized the profile/session, while the agent must not bypass site security controls.
  
  This should reshape the policy task: use Claude's managed controls only as inspiration for user/admin-owned policy surfaces, not as a default category blocker.
