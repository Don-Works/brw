---
id: 01KV18KKB0CSKT92J8EJ0392EG
schema: task/v1
workspace: agent-browser
title: Improve browsercheck cleanup and close-tab regression coverage
status: open
priority: high
tags:
  - agent-browser
  - browsercheck
  - cleanup
  - tabs
  - max-air
pinned: false
assignee:
  origin_kind: local
meta:
  composed_by: 01KV18NXKHVJYSB50GYPGFGDJ5
source:
  kind: agent
  session_id: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
status_history:
  - at: 2026-06-13T19:51:11.456232Z
    evt: created
    to: open
    by_session: 460f7b62-34f4-4f47-abdd-5445bf6d64b4
created_at: 2026-06-13T19:51:11Z
updated_at: 2026-06-13T19:52:27.524316Z
---

User-visible tab clutter was a real problem. We patched browsercheck locally to record pre-scenario tabs and close scenario-created tabs, preserving pre-existing tabs. Need finish/verify/deploy this with regression coverage: scenarios should not leave fixture/public/popup tabs behind even on failure; cleanup must close line-by-line IDs robustly on zsh/mac; and max-air remote runs should preserve user tabs while cleaning automation tabs. Add a browsercheck self-test or fixture scenario that opens multiple tabs/popups then intentionally fails and confirms cleanup.
