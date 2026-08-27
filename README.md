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
- Uses screenshots only as visual fallback, with optional Set-of-Marks overlays and a locked-session print-renderer fallback.
- Supports tabs, downloads, console, network capture, request replay, and cancellation.
- Turns a completed flow into a replayable `brw_batch` script, with identity guards.
- Grows its advertised tool catalogue on demand instead of shipping all of it every turn.
- Reuses a persistent non-default Chrome profile for signed-in flows.
- Bridges to an already-authenticated installed Chrome profile through a Chrome extension.
- Runs cleanly over SSH so the browser profile stays on the machine that owns it.

The extension screenshot path is bounded and background-safe. Chrome may suspend
its compositor while a desktop session is locked; when that happens, `brw`
automatically uses Chrome's still-available print renderer and rasterizes the
requested viewport/crop. macOS uses the built-in `sips`; Linux/other installs can
provide `pdftoppm`, ImageMagick `magick`, or `convert`. This fallback runs only
after the normal fast screenshot path stalls.

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
brwd --mcp --mcp-tools auto     # 13 tools to start, grows on demand
brwd --mcp --mcp-tools core     # 24 tools, ~7.0k tokens of catalogue
brwd --mcp --mcp-tools minimal  # 12 tools, ~3.8k tokens of catalogue
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
refuses every cookie CDP method and the site-storage domains — so it never reads
HttpOnly cookies or bulk-exports a site's stored credentials, and sensitive form
fields are redacted from snapshots, reads, and captured network headers.

The extension is open source (AGPL-3.0) and ships with a pinned public key, so
it always loads with the same stable id — identical for load-unpacked, the
self-hosted CRX, and the Web Store build:

```
amocjcgddnoakjijfggdpnefdnboilpe
```

That id is the daemon's `DefaultBridgeExtensionID`, so an unconfigured bridge
trusts the real extension with no policy edit. Only set `bridge_extension_id`
for a different re-signed build.

The extension keeps its service worker alive so the bridge does not drop while
Chrome idles in the background — see [docs/reliability.md](docs/reliability.md)
for how brw stays connected, the one-time macOS App Nap setup, and how to verify.

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

- `brw_identity` — which browser profile this namespace drives (needs no tab
  or connected bridge; call it first when several `brw_*` namespaces exist)
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
- `brw_window_resize` (real OS window, unlike `brw_emulate_device`)
- `brw_tools` (find and disclose a tool by describing the task)

Use `--mcp-tools` to shrink the advertised catalogue while keeping every tool
callable. The catalogue is re-sent on every request, so a narrower profile saves
tokens on every turn: `all` costs ~12.1k tokens, `core` ~7.0k, `minimal` ~3.8k,
and `auto` starts at ~4.1k and grows only as the agent discovers tools it needs
via `brw_tools`.

MCP stdio lifecycle: `brwd --mcp` exits cleanly on SIGTERM/SIGINT (including
while blocked waiting for input), when its stdin closes, and when it is orphaned
by its parent. For supervisors that spawn one `brwd --mcp --upstream-http`
proxy per session and may abandon it without closing stdin, the proxy defaults
to a 90-minute idle exit. Override it with `--mcp-idle-exit` or
`BRW_MCP_IDLE_EXIT` (`0` explicitly disables): the proxy is a disposable
stateless shim, so the supervisor respawns it on the next call instead of
accumulating zombie children.
Requests on one stdio connection may complete out of order, as JSON-RPC allows;
this is what lets `brw_cancel` and MCP `notifications/cancelled` interrupt a
running plan or batch on that same connection instead of waiting behind it.

Backend-specific notes:

- `brw_emulate_device` uses Chrome DevTools Protocol device emulation, not OS
  window resizing. The presets are exactly `iphone_se`, `iphone_12`,
  `iphone_13`, `iphone_14`, `iphone_14_pro_max`, `pixel_5`, `pixel_7`,
  `galaxy_s20`, `ipad_mini`, and `ipad`; each applies CSS viewport size, DPR,
  mobile viewport meta behavior, touch emulation, and mobile UA/platform
  overrides. For any other device pass `responsive`/`custom` with explicit
  `width` and `height` instead of guessing a model name. Pass `clear:true` to
  reset a tab.
- `brw_upload_file` accepts the file from exactly one source: `path`/`paths`
  (files already on the browser host), `bytes_base64` (inline base64 contents,
  no host filesystem access needed), or `url` (the daemon fetches it over
  http(s)). For `bytes_base64`/`url` the daemon materializes a temp file on the
  browser host and removes it after the upload; use `filename` to control the
  name the page sees.
- `brw_evaluate` truncates oversized results with an explicit
  `…[truncated: returned N of M bytes]` marker instead of ever returning an
  empty result; page through large payloads with the `offset`/`max_bytes` params.
- `brw_downloads` works on both backends: direct CDP consumes Browser download
  events, while the extension bridge uses `chrome.downloads` and returns the same
  normalized entries. A pre-download-support extension reports
  `supported:false` with an upgrade note instead of pretending the list is empty.
- `brw_console` is a bounded drain: direct CDP captures native console events
  without changing page globals; the extension installs a bounded interceptor.
  On the direct-CDP backend `brw_open` attaches to a blank target before
  navigating, so console output and uncaught exceptions from the page's own
  load are captured — "open the page and check the console for errors" works on
  the first read, not only after a reload. Narrow a noisy page with
  `only_errors`, `level`, `pattern`, and `limit`; messages a filter skips stay
  buffered, so a later wider read still sees them.
  `brw_trace` records timed action outcomes on both backends.
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
  `brw_open_incognito`, `brw_navigate_to`, plan/batch `open` steps,
  `brw_replay_request`, and URL-backed uploads. Inputs are canonicalized before
  checking, and committed destinations reached by redirects or link clicks are
  checked again; a denied final tab is closed or reset to `about:blank`
  (subdomains included; block wins over allow). Opt-in; off by default. This is an
  agent guardrail, not a network firewall: the final-destination check happens
  after Chrome commits, so use DNS/firewall controls when the request itself must
  never leave the machine.

With the extension bridge, each agent session automatically gets its own
**per-agent tab group**: tabs opened without an explicit `group` land in a
group titled from the MCP client's display name (or `BRW_AGENT_NAME`) plus a
short per-session suffix, with a stable per-session color — so concurrent
agents each get a visible, named lane in the tab strip with no grouping calls
required. Pass `group` on `brw_open` only for a deliberately different
run-scoped group, then reuse its `group_id` on later opens or `brw_group_tabs`
calls. Track every returned tab id, including `new_tab_id` from clicks; close
scratch tabs promptly and close every run-owned tab before finishing unless
deliberately handing it to the human. Never close pre-existing tabs, and never
put secrets or customer data in custom group titles. Tab groups are UI
organization only — a visual mirror of the lease table, never the enforcement
boundary; use profiles or incognito contexts for cookie/storage isolation. If a
human drags a tab out of an agent's group, `brw_list_tabs` flags it with
`lease.group_drift` (ownership unchanged). Explicitly claimed pre-existing tabs
are never regrouped. Native horizontal and vertical tab strips are supported:
new groups explicitly target the opened tab's real window instead of an MV3
service worker's ambiguous “current window”. A rare rejected assignment returns
`group_warning`, leaves the tab safely owned, and records a privacy-safe
degraded event rather than retrying forever.

brw also defends its background tabs against Chrome's power features: tabs it
opens are opted out of Memory Saver's automatic discard, and before driving any
tab it revives a **discarded** one (renderer killed — CDP would hang) by
reloading it and a **frozen** one (collapsed group / Energy Saver — event loop
paused) by expanding its group and briefly flashing it active inside its own
window. `brw_list_tabs` reports `discarded`/`frozen` per tab, and an
unrevivable tab fails fast with the `tab_discarded`/`tab_frozen` error class.

**Tab isolation (default).** When driving your real Chrome over the bridge, the
daemon defaults to *isolation*: brw acts only on tabs it owns. It opens its tabs
in the session's per-agent group (falling back to `--bridge-tab-group`, `brw` by
default, when no session identity is present), in the **background**,
and a no-`tab_id` action targets brw's own working tab — never the tab you have
focused. If brw owns no tab yet, the first page action opens a fresh one instead
of hijacking whatever you are looking at. To act on one of *your* existing tabs,
pass its `tab_id` (from `brw_list_tabs`). This keeps automation (and parallel
agent runs) from stomping your open tabs. To restore the legacy behavior where
brw follows your manually-focused tab, run with `--bridge-follow-focus` (or
`BRW_BRIDGE_FOLLOW_FOCUS=1`).

When MCP proxies share that daemon, brw additionally enforces exclusive tab
leases per logical agent session. `brw_list_tabs` labels targets `mine`,
`leased`, or `available`; page reads and writes, focus, close, grouping, plans,
and batches all reject a tab leased by another session with HTTP 409 /
`tab_contended`. A no-`tab_id` call reuses the caller's leased working tab or
opens a new background tab. Leases renew on use, cannot expire during an active
operation, release on brw-driven close, and expire after 30 idle minutes.

### Privacy-safe usage ledger

`brwd` records a bounded, owner-only NDJSON operations ledger by default so
timeouts, retries, bridge reconnects, latency, and tab cleanup can be analysed
after agent runs. It stores operation metadata and random correlation ids only:
never prompts, tool arguments, typed values, page content, URLs, headers,
response bodies, screenshots, paths, cookies, or credentials. Raw error text is
also excluded. The default is 20 MiB with seven rotated backups; configure it
with `--usage-log`, `--usage-log-max-mb`, and `--usage-log-backups`, or disable it
with `--usage-log off`. See [docs/usage-logs.md](docs/usage-logs.md).

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
- **Cookie/storage promise is enforced, not just asserted.** The extension
  refuses every cookie CDP method and the whole family of site-storage domains
  (`Storage`, `DOMStorage`, `IndexedDB`, `CacheStorage`, `Database`), so even a
  rogue server that answered the extension's socket cannot read HttpOnly cookies
  (which page JS cannot reach) or bulk-export a site's stored data through `brw`.
  Credential request headers (`Authorization`, `Cookie`, …) are redacted from
  captured network traffic for the same reason. `Runtime.evaluate` stays available
  because `brw` needs it to drive the page, so a caller can still read a
  non-HttpOnly `document.cookie` or an input value — the enforced line is "no
  HttpOnly cookies, no bulk storage export," not "no script access at all."

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
