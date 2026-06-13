# agent-browser

`agent-browser` is a cross-platform, agent-first browser daemon for macOS and Linux.

It controls a real, headed Chrome/Chromium browser with Chrome DevTools Protocol (CDP), while exposing an AI-friendly semantic interface over HTTP and MCP. Agents act through stable element refs such as `e17`; they do not need CSS selectors or screenshot interpretation for normal web pages.

## Current State

This repository contains the Go daemon MVP:

- Launch or attach to visible Chrome/Chromium.
- Use a persistent browser profile.
- Manage tabs.
- Extract semantic snapshots from DOM + accessibility tree.
- Extract readable page content, headings, links, forms, tables, and metadata.
- Click/type/select/press/scroll/wait using element refs.
- Capture screenshots only as a fallback/debug API.
- Expose HTTP JSON endpoints.
- Expose MCP stdio tools for harness-agnostic agent use.

## Build

```sh
cd agent-browser
go build ./cmd/browserd
```

## Run HTTP Daemon

```sh
./browserd --http :17310
```

Open a page:

```sh
curl -s localhost:17310/api/browser/open \
  -H 'content-type: application/json' \
  -d '{"url":"https://example.com"}'
```

Read semantic controls:

```sh
curl -s localhost:17310/api/page/snapshot | jq
```

Read page content:

```sh
curl -s localhost:17310/api/page/read | jq
```

Click/type with refs from `snapshot()`:

```sh
curl -s localhost:17310/api/page/click \
  -H 'content-type: application/json' \
  -d '{"ref":"e18"}'
```

## Run As MCP Server

```sh
./browserd --mcp
```

The MCP server currently exposes:

- `browser_open`
- `browser_read`
- `browser_snapshot`
- `browser_click`
- `browser_type`
- `browser_press`
- `browser_screenshot`
- `browser_wait_for`

For remote MCP over SSH, see [docs/mcp-client-config.md](docs/mcp-client-config.md) and [docs/remote-control.md](docs/remote-control.md).

## Auth Model

Chrome 136+ blocks CDP remote debugging against the default Chrome data directory. Launch mode therefore uses a persistent, non-default profile at:

```text
~/.agent-browser/chrome-profile
```

That profile is a real Chrome profile: the browser is visible, extensions can be installed, OAuth can complete, passkeys can be used where Chrome supports them, and downloads work normally. You sign in once, then reuse it.

For more detail, see [docs/auth-model.md](docs/auth-model.md).

## Safety

This project intentionally does not add bot-evasion code. It uses a normal visible browser and persistent user profile to avoid brittle headless automation, but it must not bypass CAPTCHA, MFA, fraud checks, consent gates, or site terms.

## Prior Art

The exact product name already exists: Vercel Labs ships a Rust `agent-browser` CLI. This implementation is a Go daemon/MCP variant with a different architecture. See [docs/prior-art.md](docs/prior-art.md).
