---
id: 01KV56W7HJGFEQPZ28HHD43Q8Y
schema: task/v1
workspace: agent-browser
title: Install and verify local Chrome MCP bridge for revitt-work
status: done
priority: normal
tags:
  - agent-browser
  - mcp
  - chrome-profile
  - local-install
pinned: false
assignee:
  origin_kind: local
meta:
  account: max@revitt.co
  chrome_profile: Profile 1
  extension_version: 0.1.7
  http_control: false
  mode: bridge-mcp
  profile: revitt-work
source:
  kind: agent
  session_id: 0087ab2c-ce4e-4293-8591-c29732f742b4
status_history:
  - at: 2026-06-15T08:37:54.866019Z
    evt: created
    to: done
    by_session: 0087ab2c-ce4e-4293-8591-c29732f742b4
created_at: 2026-06-15T08:37:54Z
updated_at: 2026-06-15T08:37:54Z
---

Installed agent-browser Profile Bridge 0.1.7 into local Chrome Profile 1 (max@revitt.co / revitt.co).
Configured installed local policy revitt-work -> Chrome Profile 1.
Removed stale launchd job com.revitt.agent-browser.local that respawned a scratch-profile direct HTTP daemon.
Restarted mcplexer so agent-browser-local-revitt-work launches as pure MCP bridge: --bridge --mcp --http off --bridge-addr 127.0.0.1:17311.
Verified via MCP tools only: browser_list_tabs, browser_open, browser_focus_tab, browser_read(tab_id) against https://example.com/?agent-browser-local-mcp=1. Read returned title Example Domain and H1 Example Domain.
agent-browserctl doctor reports ok=true with bridge_extension_installed=true and source Chrome Profile 1 Secure Preferences.

## Notes
- 2026-06-15 (agent): Browser-MCP Gmail test: searched max@revitt.co Gmail tab via agent_browser tools only. Queries included `in:anywhere from:boldking`, domain variants for boldking.com/co.uk/nl, and literal `in:anywhere "Boldking"`. Gmail semantic read for `https://mail.google.com/mail/u/0/#search/in%3Aanywhere+%22Boldking%22` returned `No messages matched your search`, so count is 0 and there are no last-five subjects in this mailbox.
