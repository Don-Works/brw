# brw

Semantic browser control for agents.

`brw` runs a real, visible Chrome/Chromium browser and exposes it over MCP and
HTTP. Agents use stable refs like `e17` instead of CSS selectors or screenshots
for normal web work.

## What It Does

- Controls headed Chrome/Chromium through CDP.
- Exposes stdio MCP tools for agent harnesses.
- Exposes an HTTP JSON API for custom clients.
- Returns semantic snapshots from DOM plus accessibility data.
- Reads page prose, links, headings, forms, tables, and structured product data.
- Clicks, types, fills, selects, scrolls, drags, uploads, waits, and asserts by ref.
- Returns a post-action observation after every action.
- Uses screenshots only as visual fallback, with optional Set-of-Marks overlays.
- Supports tabs, downloads, console, network capture, request replay, and cancellation.
- Reuses a persistent non-default Chrome profile for signed-in flows.
- Bridges to an already-authenticated installed Chrome profile through a Chrome extension.
- Runs cleanly over SSH so the browser profile stays on the machine that owns it.

## Bench Signal

Internal pre-release head-to-head runs against Claude-in-Chrome showed the
direction we built for: on semantic web tasks, `brw` needed fewer turns,
less token spend, less wall time, and lower estimated cost. The main reason:
agents act from refs and action observations instead of repeatedly interpreting
screenshots.

Claude-in-Chrome's durable advantage was installed-profile auth. `brw` answers
that with the profile bridge extension and SSH-first remote runtime: keep Chrome,
cookies, passkeys, downloads, and human takeover on the browser machine, while
MCP runs over stdio through SSH.

The full MCP surface is large. For lean agent contexts, run:

```sh
brwd --mcp --mcp-tools core
```

Raw private benchmark transcripts are not shipped in this repository because
they can contain prompts, machine paths, and session metadata. Treat the public
benchmark note as directional until a reproducible public harness lands.

See [docs/benchmarks.md](docs/benchmarks.md).

## Quick Start

```sh
git clone https://github.com/Don-Works/brw.git
cd brw
make build
```

Run as an MCP server:

```sh
./bin/brwd --mcp --http off
```

Run the HTTP API:

```sh
./bin/brwd --http 127.0.0.1:17310
```

Open a page and read controls:

```sh
curl -s 127.0.0.1:17310/api/browser/open \
  -H 'content-type: application/json' \
  -d '{"url":"https://example.com"}'

curl -s 127.0.0.1:17310/api/page/snapshot | jq
```

## SSH Runtime

Remote control is a first-class path. The browser stays visible on the remote
machine. SSH carries stdio MCP.

Generate a client config:

```sh
brwctl mcp-config \
  --workspace brw \
  --profile work-profile \
  --transport remote \
  --profile-policy ~/.config/brw/browser-profiles.json \
  --mode bridge
```

The policy decides which browser profile and transport a workspace may use.
See [docs/remote-control.md](docs/remote-control.md).

## Installed Chrome Profile Bridge

Chrome 136+ blocks remote debugging against the default Chrome data directory.
For auth that already exists in an installed Chrome profile, use the extension
bridge:

- development: load `extension/` once in `chrome://extensions`
- managed repeatable install: package with your own Chrome signing material
- policy: set the resulting extension ID as `bridge_extension_id`

The extension version is `0.0.1`. The public manifest does not embed a signing
key or private extension ID.

See [docs/install.md](docs/install.md) and [docs/auth-model.md](docs/auth-model.md).

## Tools

Core MCP tools include:

- `browser_open`, `browser_list_tabs`, `browser_focus_tab`, `browser_close_tab`
- `browser_list_tab_groups`, `browser_group_tabs`, `browser_ungroup_tabs`
- `browser_read`, `browser_read_data`, `browser_snapshot`, `browser_find`
- `browser_click`, `browser_click_text`, `browser_type`, `browser_fill`
- `browser_select`, `browser_press`, `browser_scroll`, `browser_hover`
- `browser_drag`, `browser_upload_file`, `browser_wait_for`
- `browser_batch`, `browser_cancel`, `browser_observe`
- `browser_screenshot`, `browser_screenshot_element`
- `browser_network_requests`, `browser_network_capture`, `browser_replay_request`
- `browser_console`, `browser_downloads`, `browser_trace`
- `browser_assert_visible`, `browser_assert_text`, `browser_assert_value`
- `browser_notify`, `browser_commit`

Use `--mcp-tools core` to advertise only the common-flow tool set while keeping
all tools callable.

With the extension bridge, agents can organize visible Chrome work into named
tab groups. Use `browser_list_tab_groups` to choose the next client-side run
name (for example `brw-1`, `brw-2`, or a short task label), pass `group` to
`browser_open` to create or reuse that titled group, then pass the
returned/listed `group_id` on later `browser_open` or `browser_group_tabs` calls
to keep the run's tabs together. Ungrouped/default tabs remain visible to
`browser_list_tabs` and can still be targeted normally by `tab_id`. Tab groups
are UI organization only; use profiles or incognito contexts for cookie/storage
isolation.

## Safety

`brw` uses a normal visible browser and persistent user profile. It does not add
stealth code, CAPTCHA bypass, MFA bypass, fraud-check bypass, consent bypass, or
cookie extraction.

Browser-control HTTP binds to loopback by default. For remote use, prefer stdio
MCP over SSH.

## License

AGPL-3.0. See [LICENSE](LICENSE).
