---
id: agent-browser-bootstrap
title: Bootstrap Go agent-browser daemon
status: done
created: 2026-06-13
---

# Bootstrap Go agent-browser daemon

Acceptance criteria:

- Go module compiles on macOS/Linux.
- Headed Chrome/Chromium launch or CDP attach.
- Persistent profile support.
- HTTP API for browser/page/visual operations.
- MCP stdio server with browser tools.
- Semantic snapshot/read do not depend on screenshots.
- Stable element refs are exposed to agents and used for actions.
- Documentation records prior art and Chrome auth constraints.

Result:

- Implemented under `agent-browser/`.
- Verified with `go test ./...`.
- Verified with `go build -o bin/agent-browserd ./cmd/browserd`.
- Added remote-control and default-profile bridge notes.
