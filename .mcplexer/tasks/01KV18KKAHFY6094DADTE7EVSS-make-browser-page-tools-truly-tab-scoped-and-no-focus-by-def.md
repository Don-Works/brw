---
id: 01KV18KKAHFY6094DADTE7EVSS
schema: task/v1
workspace: agent-browser
title: Make browser page tools truly tab-scoped and no-focus by default
status: done
priority: critical
tags:
  - agent-browser
  - browser-integration
  - tabs
  - focus
  - max-air
  - reliability
pinned: false
assignee:
  origin_kind: local
source:
  kind: agent
  session_id: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
status_history:
  - at: 2026-06-13T19:51:11.441383Z
    evt: created
    to: open
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
  - at: 2026-06-14T08:29:19.86011Z
    evt: status_changed
    from: open
    to: in_progress
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
  - at: 2026-06-14T08:30:41.2984Z
    evt: status_changed
    from: in_progress
    to: done
    by_session: a678ae39-70b1-46cd-927a-a621051ba02f
created_at: 2026-06-13T19:51:11Z
updated_at: 2026-06-14T08:30:41Z
---

Live Decathlon/max-air run showed browser automation still fights the human foreground tab. Even after focusing a target, user activity could switch Chrome to Google/chrome:// and page tools then acted/read against the wrong target. Generic fix: every page tool and HTTP endpoint should accept tab_id/target_id (snapshot/read/find/click/fill/type/select/press/scroll/wait/screenshot/upload). Direct CDP should bind context to that target; extension bridge already has cdp tabId support, so Go bridge methods need to thread the target through instead of passing "". No visible chrome.tabs.update({active:true}) except explicit browser_focus_tab or foreground_required fallback. Tool results should report target_id used. Add regression where user foreground tab changes while automation continues against inactive tab.
