# brw

Semantic browser control for agents.

Open source by [Revitt](https://revitt.co/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss), via [Don Works](https://donworks.co.uk/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss).

[![License: AGPL-3.0](https://img.shields.io/badge/license-AGPL--3.0-ff2ec4.svg)](LICENSE)
[![Website](https://img.shields.io/badge/website-brw.donworks.co.uk-ff2ec4.svg)](https://brw.donworks.co.uk/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss)
[![Part of Don Works](https://img.shields.io/badge/part%20of-Don%20Works-c6ff1a.svg)](https://donworks.co.uk/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss)

**[Website](https://brw.donworks.co.uk/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss)** &middot; **[Install](https://brw.donworks.co.uk/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss#install)** &middot; **[MCPlexer](https://mcplexer.com/?utm_source=brw&utm_medium=readme&utm_campaign=donworks_oss)** &middot; **[Issues](https://github.com/Don-Works/brw/issues)**

`brw` runs a real, visible Chromium-based browser — Chrome, Chromium, Edge,
Brave, Vivaldi, Opera or Arc — and exposes it over MCP and HTTP. Agents use stable refs like `e17` instead of CSS selectors or screenshots
for normal web work.

## What It Does

- Controls any headed Chromium browser through CDP.
- Exposes stdio MCP tools for agent harnesses.
- Exposes an HTTP JSON API for custom clients.
- Drives that same API from a shell with `brw <verb>`, refs and all.
- Returns semantic snapshots from DOM plus accessibility data.
- Reads page prose, links, headings, forms, tables, and structured product data.
- Clicks, types, fills, selects, scrolls, drags, uploads, waits, and asserts by ref.
- Returns a post-action observation after every action.
- Uses screenshots only as visual fallback, with optional Set-of-Marks overlays and a locked-session print-renderer fallback.
- Supports tabs, downloads, console, network capture, request replay, and cancellation.
- Turns a completed flow into a replayable `brw_batch` script, with identity guards.
- Finds and runs immutable deterministic browser recipes from a private provider, with timers and pre-armed page/browser events.
- Bundles an agent skill that searches before repeating work, promotes stable reusable flows, and repairs failures as new immutable recipe versions.
- Stores page text, semantic JSON, screenshots, PDFs, downloads, and short video as browser-host artifacts instead of flooding model context.
- Grows its advertised tool catalogue on demand instead of shipping all of it every turn.
- Serves an opt-in loopback dashboard: the live viewport, an activity feed of every step, and gated human takeover.
- Reuses a persistent non-default Chrome profile for signed-in flows.
- Bridges to an already-authenticated installed Chrome profile through a Chrome extension.
- Runs cleanly over SSH so the browser profile stays on the machine that owns it.

See [private recipes and browser-host artifacts](docs/recipes-and-artifacts.md)
for the architecture and [the browser automation review](docs/browser-automation-review.md)
for measured gains, security gates, competitive gaps, and prioritized next work.

The extension screenshot path is bounded and background-safe. Chrome may suspend
its compositor while a desktop session is locked; when that happens, `brw`
automatically uses Chrome's still-available print renderer and rasterizes the
requested viewport/crop. macOS uses the built-in `sips`; Linux/other installs can
provide `pdftoppm`, ImageMagick `magick`, or `convert`. This fallback runs only
after the normal fast screenshot path stalls.

## Measurement

Two harnesses run from a clean checkout, against the fixtures in `tests/`, with
no network beyond the loopback origin they start themselves and no account of
any kind:

```sh
task bench                # per-command wall time, CDP round trips, transport bytes, observation tokens
task agent-eval           # four agent-level tasks graded on the page's end state
task agent-eval-verify    # the same four, sabotaged, to prove the grading can fail
```

Neither measurement runs in `go test ./...` or `task check`: a timing that fails
because CI was busy is a gate nobody can act on. What does run there is
assertions rather than timings — one evaluation task in both modes, as the guard
that the grading can report a failure, and one check that the harness browser
cannot reach off this machine.

The recorded first run, with the environment fingerprint that says whether your
run is comparable to it, is in [docs/benchmarks.md](docs/benchmarks.md). That
page also lists the head-to-head claims against Claude-in-Chrome that used to
stand here and have been removed: they were measured before release, never
published, and nothing here reproduces them.

What `brw` does differently is a design statement, not a measured one: actions
return semantic observations, so an agent acts from refs instead of
re-interpreting a screenshot per step. Where a signed-in session is the point,
the installed browser profile is what holds it — the `brw` Chrome extension and
the SSH-first remote runtime keep Chrome, cookies, passkeys, downloads and human
takeover on the browser machine while MCP runs over stdio through SSH. Feature
comparisons live in the parity matrix in
[docs/browser-automation-review.md](docs/browser-automation-review.md);
scheduling is deliberately external.

The full MCP surface is large. For lean agent contexts, run:

```sh
brwd --mcp --mcp-tools auto     # 14 tools to start, grows on demand
brwd --mcp --mcp-tools core     # 26 tools, ~10.5k tokens of catalogue
brwd --mcp --mcp-tools minimal  # 13 tools, ~6.0k tokens of catalogue
```

For a ready-to-paste agent system prompt that encodes the fast, token-efficient
loop (act by ref, read the post-action observation instead of re-snapshotting,
use deltas, screenshot only as a fallback), run `brwd --print-system-prompt`.
See [docs/agent-guide.md](docs/agent-guide.md).

For what answers a `brw_wait_for` on each transport, see
[docs/waiting.md](docs/waiting.md).

For repeated site workflows and large observations, see
[Private recipes and browser-host artifacts](docs/recipes-and-artifacts.md).

brw stores no secret. When a recipe has to sign in, it names a credential
(`secret://<name>`) and an operator-installed plugin resolves it at the moment
that step runs — see [plugins and capabilities](docs/plugins.md), which also
states what a capability can and cannot reach and why the list is as short as
it is.

## Quick Start

```sh
curl -fsSL https://brw.donworks.co.uk/install.sh | sh
```

No sudo, nothing written outside `$HOME`. The installer verifies the release
checksum and, when `gh` is present, its build provenance attestation, then runs
`brwctl setup`: a profile policy, a per-user `brwd --bridge` service on
loopback, MCP registration with your agent client, and the bundled skill.
`brwctl setup --dry-run` prints the plan without performing it.

`brwctl doctor` diagnoses an install and prints a fix command for every failing
check; `brwctl upgrade` replaces it with a later release under the same checksum
and provenance rules, refusing while a daemon is mid-operation.

Then load the extension into the browser you want driven — `chrome://extensions`
-> Developer mode -> Load unpacked -> `<app-dir>/extension` — and click **Enable
local browser control** in the Options page that opens. Nothing connects before
you do.

Also available:

```sh
brew install don-works/tap/brw
```

and platform packages on the
[releases page](https://github.com/Don-Works/brw/releases) — `.pkg`, `.msi`,
`.deb`, `.rpm` — for managed machines where a system-wide install is wanted.

Setup binds to whichever Chromium-based browser you already use:
`--browser chrome|chromium|edge|brave|vivaldi|opera|arc`, or any other Chromium
build with `--browser <name> --user-data-dir <path>`.
Those need an administrator and are not yet code-signed; see
[docs/install.md](docs/install.md).

Verify any release artifact:

```sh
gh attestation verify <artifact> --repo Don-Works/brw
```

## Build From Source

Builds run through [Task](https://taskfile.dev) (`brew install go-task`,
or see the Task install docs). `task --list` shows every target.

```sh
git clone https://github.com/Don-Works/brw.git
cd brw
task build
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

## CLI

`brw` drives a running `brwd` from a shell: one process per action, over the
same HTTP API. Refs print as `@e17`, so a line of output pastes straight into
the next command.

```sh
brw open https://example.com
brw find "sign in"                 # @e17  button  Sign in
brw click @e17
brw fill @e18 someone@example.test
brw press Enter
brw get url
brw get text @e17
brw hover @e17
brw goto https://example.com/next
brw back
brw eval 'document.title'
brw cookies
brw console --errors
brw locale en-GB Europe/London
brw init-script add 'window.__ready=true' --origin https://example.com
brw batch '[{"action":"click","ref":"e1"},{"action":"wait","condition":"text:ok"}]'
brw read                           # the page as text
brw snapshot --limit 20            # refs for the whole page
brw screenshot --out shot.png
brw tabs
brw tab close 1234
brw artifact read art_01H...
```

`--json` prints the daemon's response envelope verbatim, for piping into `jq`.
The daemon comes from the profile policy, the same discovery `brwctl daemons`
reports; `--daemon <url>` or `BRW_URL` overrides it and `--profile <name>` picks
between several. Exit codes are `0` success, `1` the action failed, `2` usage,
`3` no daemon reachable — so a script can tell "start brwd" apart from "the page
said no".

Shell completion:

```sh
brw completion zsh > "${fpath[1]}/_brw"
brw completion bash > /etc/bash_completion.d/brw
```

`brw skill` prints brw's operating manual out of the daemon you are talking to,
stamped with that daemon's build — the same answer `brw_skill` gives over MCP and
`GET /api/skill` over HTTP. Prefer it to whatever copy is on disk: that one was
written by whichever brw was installed when `brwctl setup` last ran, and after an
upgrade it describes a surface the daemon no longer has.

## Scheduling

brw ships no scheduler. `brw run <recipe-id>` is the entry point launchd,
systemd or cron drives: one recipe, one JSON object on stdout, diagnostics on
stderr, and an exit code that tells a postcondition failure (4) apart from a
policy refusal (5), a broken daemon (3) and another run already holding the
profile (6). Two runs against one browser profile serialise on a lock keyed by
the profile rather than the daemon, so they can never interleave on one tab, and
anything that would prompt a human is refused rather than waited on.

```sh
brw run example.invoices.download --recipe-version 3 --digest <sha256>
```

Working launchd and systemd examples are in
[docs/scheduling.md](docs/scheduling.md).

## Configuring a daemon

Every `brwd` setting is a flag, and a flag is also an environment variable
(`--http` / `BRW_HTTP_ADDR`). `brw.json` is the third place, for the settings a
machine should have without repeating them in every unit file and shell alias:

```json
{
  "defaults": {"site-consent": true, "usage-log": "auto"},
  "profiles": {
    "chrome-work": {"http": "127.0.0.1:17320", "idle-exit": "2h"}
  }
}
```

It lives at `brw.json` in your user config directory, or wherever `--config` /
`BRW_CONFIG` points. Keys are flag names; a key that is not a flag is a startup
error rather than a setting that quietly does nothing. It is the weakest source:
the command line wins, then the environment, then the profile section, then the
file's defaults. The flags that say what an invocation *is* (`--mcp`,
`--login`) and the `--unsafe-*` diagnostic overrides cannot be set from a file —
those have to be typed.

Two more distribution flags:

* `--remote auto` attaches to a browser that is already running: brw reads
  `DevToolsActivePort` in the user data directory (the only place an ephemeral
  debugging port is written down) and then tries the conventional loopback
  debugging ports, attaching only to something that answers `/json/version` as a
  browser. Under a profile the workspace policy bars from direct CDP, the
  question asked is about the browser and not about how discovery found it:
  only an endpoint that names the user data directory that profile names may be
  driven, because the daemon reports the policy's own `user_data_dir` and
  `profile_directory` as the identity of whatever it attached to. Anything else
  — a guessed port, or another browser's `DevToolsActivePort` — is refused by
  name, and `--remote <endpoint>` typed out is how you say you meant it.
* `--idle-exit <duration>` shuts a daemon down cleanly after that long with no
  use. Off by default, because the default daemon is persistent; use it for a
  daemon started for one job, which would otherwise hold a browser and a port
  until the machine reboots. Use means an HTTP API request, or an MCP tool call
  in `--mcp` mode; a `/health` poll does not count, so a supervisor cannot keep
  an abandoned daemon alive. It needs the HTTP listener: with `--http off` on a
  `--mcp` daemon the same duration arms `--mcp-idle-exit` instead, and with
  neither listener nor stdio session it can never fire and says so. It is a
  note, not a startup failure, because `BRW_IDLE_EXIT` is an environment
  default and an exported one must not turn `brwd --mcp --http off` into a
  daemon that will not start.

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
load-unpacked and self-hosted builds use the same stable id:

```
amocjcgddnoakjijfggdpnefdnboilpe
```

That id is the daemon's `DefaultBridgeExtensionID`, so an unconfigured bridge
trusts the real extension with no policy edit. Only set `bridge_extension_id`
for a different re-signed build. Before publishing through the Chrome Web
Store, verify that the draft item resolves to this same id.

After install or update, Options opens with the browser-data disclosure. The
extension stays disconnected until you explicitly click **Enable local browser
control**.

The extension keeps its service worker alive so the bridge does not drop while
Chrome idles in the background — see [docs/reliability.md](docs/reliability.md)
for how brw stays connected, the one-time macOS App Nap setup, and how to verify.

### The transports

Three ways to set brw up, and a fourth `brw_identity` can report.
`brw_identity` names which one a namespace resolved to; `brwctl doctor` names it
with the capabilities it implies.

| | Extension bridge | Direct CDP | Chrome opt-in |
|---|---|---|---|
| Browser | The real signed-in Chromium browser you already use | A separate brw-owned instance | The real signed-in Chrome you already use |
| Needs | The brw extension loaded | Nothing | Chrome 144+ with remote debugging switched on by hand at `chrome://inspect` |
| Existing logins | Yes | No, unless pointed at a cloned profile | Yes |
| Chrome tab groups | Yes | No | No |
| `brw_open_incognito` | No | Yes | Yes |
| `brw_cookies`, incl. HttpOnly | No | Yes | Yes |
| `brw_state` session snapshots | No | Yes | No, same refusal as the bridge |
| Deterministic download capture | No | Yes | No, uses the browser's own download folder |
| Headless | No | Yes | No |

The lanes are supported at once: one `brwd` per profile, one MCP server per
daemon. `brwctl setup --transport direct-cdp` configures the second. The third
is `brwd --chrome-opt-in`, and only after a person has turned the switch on —
brw never does that for you. See
[docs/install.md](docs/install.md#the-chrome-opt-in-lane).

`brwd --remote <endpoint>` attaches to a DevTools endpoint some other process
opened, and reports itself as a fourth transport, `remote-cdp`. It has
everything direct CDP has except deterministic download capture: brw did not
start that browser, and `Browser.setDownloadBehavior` applies to a whole browser
context, so staging its downloads would move files brw does not own. That is the
only difference, and it holds however the endpoint was produced — including when
it is the port a Chrome opt-in published.

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

- **Load unpacked:** run `task install-extension` to print the folder and open
  `chrome://extensions`, then enable Developer mode → Load unpacked → select
  `extension/`.
- **Chrome Web Store (one-click):** the store package and listing are prepared
  in [`docs/web-store-listing.md`](docs/web-store-listing.md) and not yet
  published. Verify the item id against the pinned id before publishing.

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
- `brw_read_url` — read a public page with no tab, no lease, no navigation
- `brw_get` — one typed fact (text, value, attr, box, count, visible, …) without
  hand-written JS, resolved across iframes and open shadow roots
- `brw_storage` — localStorage / sessionStorage for the current origin
- `brw_frame` — scope snapshot/find/get to one iframe, or back to `main`
- `brw_focus` — focus an element by ref without clicking it
- `brw_key_down`, `brw_key_up` — hold a key, so Ctrl+drag and Shift+click work
- `brw_pushstate` — change the route through the History API with no reload
- `brw_clipboard` — read or write the system clipboard (the permission it grants
  outlives the call; `action:"revoke"` clears it)
- `brw_dialog` — pre-arm the answer to a confirm/prompt before the click that
  raises it, and see which dialogs were answered
- `brw_diff` — mark, act, compare: did the page actually change?
- `brw_route` — mock or abort matching requests without touching the network
- `brw_batch`, `brw_cancel`, `brw_observe`
- `brw_screenshot`, `brw_screenshot_element`
- `brw_artifact_capture`, `brw_artifact_info`, `brw_artifact_read`,
  `brw_artifact_search`, `brw_artifact_delete`
- `brw_recipe_search`, `brw_recipe_run` (only when a private provider is configured)
- `brw_emulate_device` for DevTools mobile/responsive emulation
- `brw_set_geolocation`, `brw_set_network_conditions`, `brw_emulate_media` —
  what the page believes about where it is, whether it has a network, and which
  media it renders for (not on the extension bridge)
- `brw_set_extra_headers` — extra request headers for the origins you name and
  nothing else; `brw_set_user_agent`; `brw_authenticate` for one credentialed
  navigation (not on the extension bridge)
- `brw_set_download_path` — send completed downloads somewhere you can open
  them. Not on the extension bridge, and not on the Chrome opt-in lane: the
  DevTools command applies to a whole browser context, so on the browser you are
  signed into it would redirect the files you download by hand
- `brw_network_requests`, `brw_network_capture`, `brw_replay_request`
- `brw_console`, `brw_downloads`, `brw_trace`
- `brw_vitals` — LCP, CLS, INP, TTFB and FCP for the current navigation, each
  rated against the published thresholds
- `brw_a11y_audit` — axe-core, embedded in the binary rather than fetched, with
  the failures summarised by impact and the full report written to an artifact
- `brw_highlight` — outline an element for a human watching the browser;
  `clear:true` undoes it
- `brw_assert_visible`, `brw_assert_text`, `brw_assert_value`
- `brw_assert` for deterministic URL, HTTP status, element count, element
  state, attribute and download-digest checks
- `brw_page_tools`, `brw_call_page_tool`, `brw_page_tool_result`,
  `brw_page_tool_cancel` (WebMCP)
- `brw_state` — seal the cookies a brw-created context holds for the origins
  you name and put them back into a later throwaway context, so a run does not
  log in again. Save and restore only: no action returns a stored value.
  Encrypted on the browser host, expiring, and refused on a browser you are
  signed into — the extension bridge and the Chrome opt-in lane
  (see [docs/auth-model.md](docs/auth-model.md))
- `brw_baseline` — gate a page against a stored regression baseline keyed by
  recipe digest, step and an environment fingerprint, comparing pixels with a
  tolerance and named ignore regions AND the page's ARIA structure. Updating a
  baseline takes an explicit action; a check never writes. A baseline for a
  recipe the private provider owns is stored with that provider; the local
  `--baseline-root` holds the rest
- `brw_notify`, `brw_commit`
- `brw_window_resize` (real OS window, unlike `brw_emulate_device`)
- `brw_tools` (find and disclose a tool by describing the task)

Use `--mcp-tools` to shrink the advertised catalogue while keeping every tool
callable. The catalogue is re-sent on every request, so a narrower profile saves
tokens on every turn: on a direct-CDP daemon `all` costs ~30.9k tokens across
90 tools, `core` ~10.5k, `minimal` ~6.0k, and `auto` starts at ~6.3k and grows
only as the agent discovers tools it needs via `brw_tools` (measure with
`scripts/measure-tool-catalogue.py`).

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
  A tool slower than the caller's patience is started with `detach:true` and
  collected by id with `brw_page_tool_result` / stopped with
  `brw_page_tool_cancel`; a waited call that outlasts its timeout returns the
  same id rather than abandoning the work. Tools are registered per document, so
  `frame` targets a same-origin iframe's own tools. Arguments are capped at 64KB
  and refused above it, never truncated, and the tool's own result is windowed
  like `brw_evaluate`. An invocation no document in the polled tab holds — its
  page navigated away, or the poll landed on another tab — is reported as
  `status:"lost"` instead of waiting out the timeout; every report carries the
  `tab_id` to poll back into.
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
- **Site permissions**: `--site-consent` records what the user actually allowed,
  per origin, with a scope (`read` / `act`), a grantor and an expiry, stored with
  the profile policy. Every record carries an HMAC over its own authorising
  fields keyed by a 0600 file, so a local process cannot write itself a grant;
  a record that does not verify is discarded and reported. Un-granted origins are
  refused by name and scope, revocation applies on the next action with no daemon
  restart, and a shipped `financial-services` / `adult` / `pirated` category
  blocklist needs an explicit `--override-category` that is written to the
  consent ledger. Every tool carries a rule or a written reason it needs none, so
  reads of the live page are gated too, not only the navigation that reached it.
  `--confirm-actions` gates publishing, purchasing, personal-data form
  submissions and anything on a blocklisted category, on single tools and on
  plan/batch steps alike, and fails CLOSED when nobody is there to confirm. List and revoke with `brwctl grants`, the extension
  options page, or `brw grants`. Opt-in; off by default.
  See [docs/site-permissions.md](docs/site-permissions.md).
- **Content-boundary navigation guard**: `--content-nav-guard` (not on the extension bridge)
  refuses a top-level navigation that page content initiated to another site — an
  injected link click, a meta refresh, a script `location` assignment — while
  what the agent asked for still works: the destination it named, and the
  navigation its own click, keypress or form submission causes. Opt-in; off by
  default.

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

## Live dashboard

`BRW_DASHBOARD=1` turns on `/dashboard`, a loopback-only page that shows the
browser `brw` is driving. It is off by default and refuses non-loopback clients
even when the daemon is bound wider: it streams the rendered pixels of a
signed-in session. To watch a remote browser, forward the port
(`ssh -L 17310:127.0.0.1:17310`) so the pixels travel inside the tunnel.

- **Viewport.** Frames come from Chrome's compositor (`Page.startScreencast`)
  on every CDP transport, so an idle page costs nothing. The extension bridge
  has no compositor stream and polls screenshots instead.
- **Activity feed.** A line per step — action, the ref and accessible name it
  acted on, outcome, latency — off the same trace stream `/api/session/stream`
  publishes, after credential redaction. A typed password reaches the feed as a
  redacted action with no value, and a failure's reason reaches it with any URL
  in it replaced. It is a live view rather than a log: a watcher that falls
  behind during a fast plan or batch loses its oldest queued lines, so read the
  feed as what is happening now and `brw_trace` as the record.
- **Takeover.** A human can drive the tab directly; pointer and keyboard events
  are forwarded as `Input.dispatchMouseEvent` / `Input.dispatchKeyEvent`. It
  needs an explicit per-session enable and it exists only when `brwd` is bound
  to loopback — on a wider bind the control is not rendered and the routes
  answer 404. The hold is bound to the tab it was taken on, so the human's next
  click cannot land in a tab the agent moved to since, and it expires if the
  page stops renewing it, so a closed tab does not lock the agent out.

  While a human holds takeover, every agent action that would touch that
  browser is refused by name: the input verbs, `brw_evaluate` with a caller's own
  expression, each step of a plan or batch already in flight, and the tab verbs
  (`brw_open`, `brw_focus_tab`, `brw_close_tab`) that would move the target out
  from under them. Over HTTP the refusal is `409` with `"code":
  "takeover_held"` and the holder and expiry alongside it, and the usage log
  counts it under that same class, so an agent never has to match on prose.
  Reads continue — snapshots, `brw_read`, `brw_get`, `brw_frame` and
  `brw_assert` — so the agent can see what the human did before it resumes.

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
  per-launch token: the daemon mints it each start, keeps it in memory, and
  serves it only over loopback to the extension (a web page's cross-origin fetch
  gets an opaque response). The `0.2.0+` extension presents it in its first
  frame. **The token is required.** A *wrong* token and a *missing* token are both
  rejected: the `chrome-extension://` Origin that gets a caller as far as the
  handshake is a header any local process can forge, so a tokenless hello
  authenticates nothing. `BRW_BRIDGE_ALLOW_TOKENLESS=1` restores the old
  permissive behaviour for a pre-`0.2.0` extension and logs a warning for as long
  as it is set. Empty-Origin (non-browser) websocket clients are rejected
  regardless. What excludes a web page and any *other* extension is that Origin
  pin, which the browser sets and neither can change; the token's own job is
  identity — it binds a connection to **this** daemon launch, so an extension
  pointed at the wrong port is refused rather than silently driving another
  profile's browser.

  What this does *not* defend against: anything that can send a loopback GET can
  read the token off `/status`, exactly as the extension does — including any
  process running as you. The bridge's boundary is the browser and the network,
  not other processes with your uid; see
  [docs/auth-model.md](docs/auth-model.md) for why moving it needs OS isolation
  rather than a better protocol. The token is no longer written to
  `~/.brw/bridge-token`; `BRW_BRIDGE_TOKEN_FILE=<path>` asks for that file back
  and re-creates the copy at rest.
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
