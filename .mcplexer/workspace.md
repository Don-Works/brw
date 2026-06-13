---
name: agent-browser
source: repo
status: active
created: 2026-06-13
---

# agent-browser

Repo-local MCPlexer workspace for the agent-first browser daemon.

Current objective:

- Build a Go `agent-browserd` daemon exposing HTTP and MCP tools.
- Prefer semantic page state from DOM, accessibility tree, visible controls, and readability extraction.
- Keep screenshots as fallback/debug output.
- Preserve human visibility and takeover.

Important product note:

Vercel Labs already ships an `agent-browser` Rust CLI. This workspace is deliberately a Go daemon/MCP implementation with HTTP API, `chromedp`/`cdproto`, and a dedicated auth strategy for real installed Chrome/Chromium.
