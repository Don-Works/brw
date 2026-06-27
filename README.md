# brw

Semantic browser control for agents.

Open source by [Revitt](https://revitt.co/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss), via [Don Works](https://donworks.co.uk/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss).

[![License: AGPL-3.0](https://img.shields.io/badge/license-AGPL--3.0-ff2ec4.svg)](LICENSE)
[![Website](https://img.shields.io/badge/website-brw.donworks.co.uk-ff2ec4.svg)](https://brw.donworks.co.uk/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss)
[![Part of Don Works](https://img.shields.io/badge/part%20of-Don%20Works-c6ff1a.svg)](https://donworks.co.uk/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss)

**[Website](https://brw.donworks.co.uk/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss)** &middot; **[Install](https://brw.donworks.co.uk/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss#install)** &middot; **[MCPlexer](https://mcplexer.com/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss)** &middot; **[Issues](https://github.com/Don-Works/brw/issues)**

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

For a ready-to-paste agent system prompt that encodes the fast, token-efficient
loop (act by ref, read the post-action observation instead of re-snapshotting,
use deltas, screenshot only as a fallback), run `brwd --print-system-prompt`.
See [docs/agent-guide.md](docs/agent-guide.md).

Raw private benchmark transcripts are not shipped in this repository because
they can contain prompts, machine paths, and session metadata. Treat the public
benchmark note as directional until a reproducible public harness lands.

See [docs/benchmarks.md](docs/benchmarks.md).

## Quick Start

Download a native installer from the
[GitHub releases page](https://github.com/Don-Works/brw/releases):

- Windows: `.msi`
- macOS: `.pkg`
- Linux: `.deb` or `.rpm`

After installing, run `brwd` directly from your terminal or MCP client.

## Build From Source

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
  --host browser-host \
  --user browser-user \
  --remote-brwd ~/.local/bin/brwd \
  --output ~/.local/bin/brw-browser-mcp
```

See [docs/remote-control.md](docs/remote-control.md).

## Installed browser profile (Chromium recommended)

Chrome 136+ blocks remote debugging against the default Chrome data directory.
For auth that already exists in an installed browser profile, use the `brw`
extension. It bridges the daemon to your real, signed-in browser over
`ws://127.0.0.1`, drives visible tabs via the Chrome debugger protocol, and
never reads cookies, passwords, or passkeys.

The extension is open source (AGPL-3.0) and ships with a pinned public key, so
it always loads with the same stable id — identical for load-unpacked, the
self-hosted CRX, and the Web Store build:

```
amocjcgddnoakjijfggdpnefdnboilpe
```

That id is the daemon's `DefaultBridgeExtensionID`, so an unconfigured bridge
trusts the real extension with no policy edit. Only set `bridge_extension_id`
for a different re-signed build.

### Chromium recommended (open source)

Chromium is open source and not gated by the Chrome Web Store, so you can
**force-install + auto-update** the extension from a single policy file. `brw`
self-hosts the signed package and an Omaha/`gupdate` auto-update manifest:

- Signed package (CRX): <https://brw.donworks.co.uk/brw.crx>
- Auto-update manifest: <https://brw.donworks.co.uk/updates.xml>
- `ExtensionInstallForcelist` entry:
  `amocjcgddnoakjijfggdpnefdnboilpe;https://brw.donworks.co.uk/updates.xml`

Drop the ready-made policy for your platform:

- **Linux (no MDM needed):** copy
  [`brw-chromium-policy.json`](https://brw.donworks.co.uk/policies/brw-chromium-policy.json)
  into `/etc/chromium/policies/managed/` (or `/etc/opt/chrome/policies/managed/`
  for Chrome). Chromium installs from the manifest and auto-updates.
- **macOS:** install the configuration profile
  [`brw-chromium.mobileconfig`](https://brw.donworks.co.uk/policies/brw-chromium.mobileconfig)
  manually or via MDM. macOS force-install requires a managed profile / MDM; it
  is not settable from user-domain defaults.
- **Windows:** import
  [`brw-chromium-policy.reg`](https://brw.donworks.co.uk/policies/brw-chromium-policy.reg),
  or set the equivalent GPO at
  `HKLM\SOFTWARE\Policies\Chromium\ExtensionInstallForcelist`.

`brwctl` generates these: `brwctl pack-extension --key <pem>` (CRX),
`brwctl update-xml --crx-url <url>` (manifest), and
`brwctl macos-policy --update-url <url> --install-mode force_installed`
(`.mobileconfig`). The private signing key lives outside the repo.

**Zero-click option:** `brwd --extension <dir>` launches Chromium with the
extension already loaded (it passes Chrome's `--load-extension` through), so
there is nothing to install or click. Chrome 137+ dropped reliable
`--load-extension`, so this path is Chromium-only.

Verified: Chromium 151 loads the extension with the correct id and bridges to
`brwd` end-to-end, and the auto-update endpoint (`updates.xml` + CRX) is valid
and served with the correct content-types.

### Chrome (also works)

- **Load unpacked:** run `make install-extension` to print the folder and open
  `chrome://extensions`, then enable Developer mode → Load unpacked → select
  `extension/`.
- **Chrome Web Store (one-click):** an unlisted listing is in review for
  one-click install + auto-updates, sharing the same id (not live yet).

See the [Install page](https://brw.donworks.co.uk/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss#install),
[docs/install.md](docs/install.md), and [docs/auth-model.md](docs/auth-model.md).

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
- `brw_emulate_device` for DevTools mobile/responsive emulation
- `brw_network_requests`, `brw_network_capture`, `brw_replay_request`
- `brw_console`, `brw_downloads`, `brw_trace`
- `brw_assert_visible`, `brw_assert_text`, `brw_assert_value`
- `brw_page_tools`, `brw_call_page_tool` (WebMCP)
- `brw_notify`, `brw_commit`

Use `--mcp-tools core` to advertise only the common-flow tool set while keeping
all tools callable.

Backend-specific notes:

- `brw_emulate_device` uses Chrome DevTools Protocol device emulation, not OS
  window resizing. Presets such as `iphone_se`, `iphone_14`, `pixel_7`,
  `galaxy_s20`, and `ipad_mini` apply CSS viewport size, DPR, mobile viewport
  meta behavior, touch emulation, and mobile UA/platform overrides. Pass
  `clear:true` to reset a tab.
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
- Snapshots descend into **open and closed** shadow roots and same-origin
  iframes. **Cross-origin** iframes cannot be read (the browser isolates them);
  instead of failing silently, a snapshot surfaces them in
  `metadata.cross_origin_frames` (box + origin) with a `cross_origin_note`, so an
  agent can fall back to `brw_screenshot` + `brw_click_xy`.
- `brw_snapshot` accepts `format:"compact"` for a one-line-per-element text
  rendering (ref, role, name, key state) that costs markedly fewer tokens than
  the default JSON — prefer it for small models.
- **WebMCP**: with `--enable-webmcp`, brw acts as the agent-side runtime for the
  W3C `navigator.modelContext` draft. Cooperating sites can register page tools
  that `brw_page_tools` lists and `brw_call_page_tool` invokes — more reliable and
  token-efficient than driving the DOM. Default off (it is observable to pages).
- **Navigation guardrail**: `--blocked-domains` / `--allowed-domains` (or
  `BRW_BLOCKED_DOMAINS` / `BRW_ALLOWED_DOMAINS`) gate `brw_open`,
  `brw_open_incognito`, and `brw_replay_request` so a prompt-injected agent cannot
  steer the browser to off-limits domains (subdomains included; block wins over
  allow). Opt-in; off by default.

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

### Loopback is treated as a trust boundary

`brw` drives a real signed-in Chrome and is operated by a possibly
prompt-injected agent, so the loopback surfaces are hardened against same-machine
browser attackers rather than assumed safe:

- **HTTP control plane (`127.0.0.1:17310`)** rejects cross-origin browser
  requests (CSRF) and, on a loopback bind, enforces a `Host` allowlist
  (DNS-rebinding). A non-loopback bind (e.g. a Tailscale/LAN address for the
  documented "behind SSH/Tailscale with caller auth" path) skips the `Host`
  allowlist — its legitimate `Host` may be a MagicDNS name — but still rejects
  cross-origin browser requests. CLI/MCP clients send no browser `Origin`, so
  they are unaffected.
- **Extension bridge (`127.0.0.1:17311`)** authenticates the extension with a
  per-launch token: the daemon mints it each start, persists it `0600` at
  `~/.brw/bridge-token`, and serves it only over loopback to the extension (a web
  page's cross-origin fetch gets an opaque response). The `0.2.0+` extension
  presents it in its first frame. **This is non-breaking:** a *wrong* token is
  always rejected, but a not-yet-reloaded older extension that sends *no* token
  still connects (logged once), so upgrading the daemon never bricks an installed
  extension. Set `BRW_BRIDGE_REQUIRE_TOKEN=1` to make the token mandatory once
  every extension is on `0.2.0`. Empty-Origin (non-browser) websocket clients are
  rejected regardless.
- **Cookie/passkey promise is enforced, not just asserted.** The extension
  refuses every cookie CDP method and the whole `Storage` domain, so even a rogue
  server that answered the extension's socket cannot exfiltrate cookies (including
  HttpOnly ones that page JS cannot reach) through `brw`.

The extension-side protections (token, cookie denylist, dialog handling) take
effect whenever the `0.2.0` extension next loads — reload it in
`chrome://extensions`, or it loads automatically the next time Chromium is
relaunched with `--load-extension`. Nothing breaks in the meantime.

## Part of Don Works

`brw` is part of [Don Works](https://donworks.co.uk/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss), Revitt's open-source arm.

Related:

- [Don Works](https://donworks.co.uk/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss) — the umbrella ([github.com/Don-Works](https://github.com/Don-Works)).
- [MCPlexer](https://mcplexer.com/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss) — MCP gateway and cross-harness AI runtime ([github.com/Don-Works/mcplexer](https://github.com/Don-Works/mcplexer)).
- [Revitt](https://revitt.co/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss) — the parent company.

## License

AGPL-3.0. See [LICENSE](LICENSE).
