---
kind: fact
name: prior-art-agent-browser
created: 2026-06-13
tags:
  - browser
  - mcp
  - prior-art
---

# Prior Art

The exact public name `agent-browser` is already used by Vercel Labs for a Rust CLI oriented around accessibility snapshots and refs.

Related tools include:

- Microsoft Playwright MCP: MCP server using structured accessibility snapshots.
- Chrome DevTools MCP: DevTools/CDP-oriented MCP for debugging and browser inspection.
- Chrome debug MCP variants: attach to a user-launched Chrome debugging port.

This repository is still useful if the product goal is specifically a Go daemon with `chromedp`/`cdproto`, HTTP API, MCP stdio, and a profile/auth architecture that can later support an installed-profile extension bridge.

Chrome DevTools MCP is a companion diagnostics MCP here, not the primary agent
surface. Use it only through a wrapper that correlates to the same
workspace-approved Chrome profile.
