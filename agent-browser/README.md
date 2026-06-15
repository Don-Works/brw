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
- Bridge to an already-authenticated installed Chrome profile through a stable installed Chrome extension transport.
- Ship `agent-browserctl` for install verification, MCP config generation, Chrome policy generation, and extension packaging.

## Build

```sh
cd agent-browser
go build ./cmd/browserd
go build ./cmd/agent-browserctl
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
- `browser_open_incognito`
- `browser_close_context`
- `browser_list_tabs`
- `browser_focus_tab`
- `browser_close_tab`
- `browser_read`
- `browser_read_data`
- `browser_snapshot`
- `browser_find`
- `browser_click`
- `browser_drag`
- `browser_mouse_down`
- `browser_mouse_up`
- `browser_click_text`
- `browser_navigate`
- `browser_hover`
- `browser_evaluate`
- `browser_network_requests`
- `browser_network_capture`
- `browser_replay_request`
- `browser_type`
- `browser_fill`
- `browser_upload_file`
- `browser_select`
- `browser_press`
- `browser_scroll`
- `browser_screenshot`
- `browser_screenshot_element`
- `browser_wait_for`
- `browser_plan`
- `browser_batch`
- `browser_cancel`
- `browser_observe`
- `browser_group_tabs`
- `browser_ungroup_tabs`
- `browser_assert_visible`
- `browser_assert_text`
- `browser_assert_value`
- `browser_assert_hidden`
- `browser_commit`
- `browser_notify`
- `browser_click_xy`
- `browser_console`
- `browser_downloads`
- `browser_trace`
- `browser_clear_trace`

Most page tools accept optional `tab_id` from `browser_list_tabs`; use it for
multi-window flows where the OS-visible active tab can drift. `browser_click_text`
finds visible actionable controls by accessible-name/text match when semantic refs
are stale or hidden inside custom components. `browser_snapshot` and `browser_find`
support `text_content` to match on prose, and `browser_snapshot` reports
`visual_islands` so an agent can detect opaque visual content (canvas/iframe/media)
that carries no semantic refs. `browser_screenshot` accepts `annotate:true` for a
Set-of-Marks capture: each in-viewport frontier element is drawn with a labelled box
whose label is the same ref returned by `browser_snapshot`, and the response carries
a legend mapping each ref to its box — so a vision model can read a label off the
image and act on it with `browser_click` using that exact ref. Incognito tools
(`browser_open_incognito` / `browser_close_context`) and `browser_downloads` are
direct-CDP only; on the extension bridge they return an explanatory note. Note that
`browser_evaluate`'s `fetch()` runs under the current page's Content-Security-Policy,
so cross-origin calls must originate from a tab whose origin permits them.

For remote MCP over SSH, see [docs/mcp-client-config.md](docs/mcp-client-config.md) and [docs/remote-control.md](docs/remote-control.md).

## Installed Profile Bridge

For an existing installed Chrome profile such as `max-gmail`, use the extension bridge after the bridge extension is installed in that Chrome profile:

```sh
AGENT_BROWSER_WORKSPACE=agent-browser \
AGENT_BROWSER_PROFILE=max-gmail \
AGENT_BROWSER_PROFILE_POLICY=../.mcplexer/config/browser-profiles.json \
agent-browserd --bridge --mcp --http off
```

Generate a remote SSH MCP config:

```sh
agent-browserctl mcp-config \
  --workspace agent-browser \
  --profile max-gmail \
  --transport max-air \
  --profile-policy ../.mcplexer/config/browser-profiles.json \
  --mode bridge
```

The runtime transport is stdio MCP over SSH. The Chrome profile remains on the
browser machine. See [docs/install.md](docs/install.md).

## Auth Model

Chrome 136+ blocks CDP remote debugging against the default Chrome data directory. Launch mode therefore uses a persistent, non-default profile at:

```text
~/.agent-browser/chrome-profile
```

That profile is a real Chrome profile: the browser is visible, extensions can be installed, OAuth can complete, passkeys can be used where Chrome supports them, and downloads work normally. You sign in once, then reuse it.

The extension bridge is the route for carrying over auth that already exists in
your installed Chrome profile. It does not extract cookies, edit Chrome profile
databases, or copy profile data.

Workspace bindings in `.mcplexer/config/browser-profiles.json` define which
Chrome profiles and transports a workspace may use. For this workspace the
default binding is `agent-browser -> max-gmail over max-air`; direct CDP remains
restricted to the non-default `agent-revitt` profile.

For more detail, see [docs/auth-model.md](docs/auth-model.md).

## Safety

This project intentionally does not add bot-evasion code. It uses a normal visible browser and persistent user profile to avoid brittle headless automation, but it must not bypass CAPTCHA, MFA, fraud checks, consent gates, or site terms.

## Prior Art

The exact product name already exists: Vercel Labs ships a Rust `agent-browser` CLI. This implementation is a Go daemon/MCP variant with a different architecture. See [docs/prior-art.md](docs/prior-art.md).
