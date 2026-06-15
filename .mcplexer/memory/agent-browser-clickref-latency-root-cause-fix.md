---
id: 01KV5CK7X03T0BGCAKKRERR6F4
schema: memory/v1
kind: note
name: agent-browser clickRef latency root-cause + fix
workspace: agent-browser
tags:
  - agent-browser
  - mcp
  - browser-automation
  - performance
  - root-cause
  - cdp
  - project-fact
pinned: false
source:
  kind: agent
  session_id: 59050316-1ea7-4a5b-9634-46aa6a29fb4f
created_at: 2026-06-15T10:17:51Z
updated_at: 2026-06-15T10:17:51Z
---

Repo /Users/max/github/revitt/brw (agent-browser: Go MCP driving real Chrome via extension bridge). ROOT CAUSE of ~5.1s browser_click latency: clickRef (internal/extensionbridge/bridge.go) did resolveBox + THREE sequential CDP Input.dispatchMouseEvent calls; CDP Input commands block on renderer-ack (~1.5s each on heavy React pages) while Runtime.evaluate round-trips are ~5ms. browser_click_xy uses ONE in-page Runtime.evaluate (ClickXYScript: elementFromPoint + dispatch pointer/mouse/click) = 13-30ms AND actuates React custom controls (Decathlon vp-combobox, add-to-basket) fine. Untrusted in-page dispatchEvent DOES fire React onClick (React doesn't gate click on isTrusted); earlier 'synthetic failed' was a sequencing bug (combobox not open), not trusted-ness. FIX 2026-06-15 (build+test green, NOT yet live-reloaded — daemon PID is installed binary in ~/Library/Application Support/agent-browser/bin, stdio MCP child of mcplexer): clickRef uses ClickXYScript-at-box-centre as fast primary, CDP dispatch fallback; ClickXYScript hardened with pointerover/mouseover/move+focus; bridge.evaluate treats empty by-value result as success(null) not error. Generic lesson: actuate via in-page trusted-equivalent events + actionability gate; avoid CDP Input on hot path. Decathlon is a standard React/Vitamin site — gaps were ours.
