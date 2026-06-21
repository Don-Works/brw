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
that with the `brw` Chrome extension and SSH-first remote runtime: keep Chrome,
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
For installed Chrome profiles, prefer a long-lived remote bridge daemon plus a
generated SSH stdio wrapper:

```sh
brwctl remote-mcp-wrapper \
  --host max-air \
  --user maxrevitt \
  --remote-brwd ~/.local/bin/brwd \
  --output ~/.local/bin/brw-max-air-mcp
```

See [docs/remote-control.md](docs/remote-control.md).

## Installed Chrome Profile

Chrome 136+ blocks remote debugging against the default Chrome data directory.
For auth that already exists in an installed Chrome profile, use the `brw`
extension:

- development: load `extension/` once in `chrome://extensions`
- managed repeatable install: package with your own Chrome signing material
- policy: set the resulting extension ID as `bridge_extension_id`

The extension version is `0.0.1`. The public manifest does not embed a signing
key or private extension ID.

See [docs/install.md](docs/install.md) and [docs/auth-model.md](docs/auth-model.md).

## Tools

Core MCP tools include:

- `brw_open`, `brw_list_tabs`, `brw_focus_tab`, `brw_close_tab`
- `brw_list_tab_groups`, `brw_group_tabs`, `brw_ungroup_tabs`
- `brw_read`, `brw_read_data`, `brw_snapshot`, `brw_find`
- `brw_click`, `brw_click_text`, `brw_type`, `brw_fill`
- `brw_select`, `brw_press`, `brw_scroll`, `brw_hover`
- `brw_drag`, `brw_upload_file`, `brw_wait_for`
- `brw_batch`, `brw_cancel`, `brw_observe`
- `brw_screenshot`, `brw_screenshot_element`
- `brw_network_requests`, `brw_network_capture`, `brw_replay_request`
- `brw_console`, `brw_downloads`, `brw_trace`
- `brw_assert_visible`, `brw_assert_text`, `brw_assert_value`
- `brw_notify`, `brw_commit`

Use `--mcp-tools core` to advertise only the common-flow tool set while keeping
all tools callable.

Backend-specific notes:

- `brw_upload_file` accepts the file from exactly one source: `path`/`paths`
  (files already on the browser host), `bytes_base64` (inline base64 contents,
  no host filesystem access needed), or `url` (the daemon fetches it over
  http(s)). For `bytes_base64`/`url` the daemon materializes a temp file on the
  browser host and removes it after the upload; use `filename` to control the
  name the page sees.
- `brw_evaluate` truncates oversized results with an explicit
  `…[truncated: returned N of M bytes]` marker instead of ever returning an
  empty result; page through large payloads with the `offset`/`max_bytes` params.
- `brw_downloads` captures downloads on the direct-CDP backend
  (`supported: true`). On the extension-bridge backend it returns an empty list
  with `supported: false` plus an explanatory `note`, because the bridge cannot
  observe CDP download events; branch on `supported` to detect this case.

With the extension bridge, agents can organize visible Chrome work into named
tab groups. Use `brw_list_tab_groups` to choose the next client-side run
name (for example `brw-1`, `brw-2`, or a short task label), pass `group` to
`brw_open` to create or reuse that titled group, then pass the
returned/listed `group_id` on later `brw_open` or `brw_group_tabs` calls
to keep the run's tabs together. Ungrouped/default tabs remain visible to
`brw_list_tabs` and can still be targeted normally by `tab_id`. Tab groups
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
