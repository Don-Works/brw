---
id: 01KV0BC67AZ2X6XRWT3H1WJYZ6
schema: task/v1
workspace: agent-browser
title: Add and verify MCP file upload support
status: open
priority: high
tags:
  - mcp
  - browser-speed
  - file-upload
  - signoff
pinned: false
assignee:
  origin_kind: local
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
meta:
  blocked_by: 01KV0B84NTGR22QSQSYTPXVB5T
  composed_by: 01KV0AGWKR7R02GH7PM6S8B8KG
  touches_files:
    - agent-browser/internal/browser/manager.go
    - agent-browser/internal/extensionbridge/bridge.go
    - agent-browser/internal/mcp/server.go
    - agent-browser/internal/http/server.go
    - agent-browser/cmd/browsercheck/main.go
source:
  kind: agent
  session_id: f8ed800b-c98d-4403-a5ba-ea03a0447722
status_history:
  - at: 2026-06-13T11:20:19.946826Z
    evt: created
    to: open
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
  - at: 2026-06-13T11:20:19.946826Z
    evt: assigned
    to: f8ed800b-c98d-4403-a5ba-ea03a0447722
    by_session: f8ed800b-c98d-4403-a5ba-ea03a0447722
created_at: 2026-06-13T11:20:19Z
updated_at: 2026-06-13T11:20:19Z
---

Add first-class file upload support to agent-browser if missing. Acceptance: an MCP client can set a file input on the Selenium web form through max-air using a semantic ref/query, and the form submit/readback confirms the upload path without screenshots.

## Notes
- 2026-06-13 (agent): Implemented browser_upload_file across direct browser manager, extension bridge, HTTP API, MCP tool surface, httpclient proxy, browsercheck step, and local fixture coverage. Local browsercheck fixture-form-actions now uploads tests/fixtures/upload.txt and passes.
- 2026-06-13 (agent): max-air MCP upload smoke passed. Created /tmp/agent-browser-upload-smoke.txt on max-air, opened selenium.dev web form through agent_browser MCP, found file input e17, browser_upload_file set the file successfully, and a follow-up find saw the uploaded filename. Total around 715 ms; upload call around 406 ms.
- 2026-06-13 (agent): Reverified after structural-observation deployment: max-air browser_upload_file passed in ~178 ms for the upload call; action result focus=e17 and frontier signals were structural.
