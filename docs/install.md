# Install

## One line, no sudo

```sh
curl -fsSL https://brw.donworks.co.uk/install.sh | sh
```

The installer writes nothing outside `$HOME` and never asks for a privileged
step. It resolves the latest release, downloads the archive for your OS and
architecture, verifies its SHA256, verifies the GitHub build provenance
attestation when `gh` is installed and authenticated, unpacks it, and then runs
`brwctl setup` (below). Read it first if you would rather not pipe a URL into a
shell: <https://brw.donworks.co.uk/install.sh>.

| Variable | Effect |
|---|---|
| `BRW_VERSION` | Release to install, with or without the leading `v`. Default: the latest release tag. |
| `BRW_INSTALL_DIR` | Payload directory. Must be absolute. |
| `BRW_BIN_DIR` | Where the command symlinks go. Defaults to `~/.local/bin`, or to `<install dir>/bin` when `BRW_INSTALL_DIR` is set, so a relocated install stays self-contained. |
| `BRW_BASE_URL` | Where to fetch the archive from. For mirrors and testing. |
| `BRW_NO_SETUP` | Stop after unpacking; do not run `brwctl setup`. |
| `BRW_SKIP_ATTESTATION` | Skip the provenance check. The SHA256 check still runs. |

A checksum mismatch aborts before anything is written. An explicit attestation
failure aborts; an unauthenticated `gh` warns and continues. Re-running replaces
`bin extension tests skills doc` and preserves `config/` and
`extension/bridge-defaults.json`.

## Homebrew

```sh
brew install don-works/tap/brw
```

Homebrew puts the whole tree in the formula prefix, so that prefix is the app
directory:

```sh
brwctl doctor --app-dir "$(brew --prefix brw)"
```

## Native installers

For managed and multi-user machines, GitHub releases ship platform packages:

- macOS: `brw_<version>_macos_universal.pkg`
- Windows: `brw_<version>_windows_amd64.msi` and `..._arm64.msi`
- Debian/Ubuntu: `brw_<version>_linux_amd64.deb` and `..._arm64.deb`
- Fedora/RHEL: `brw_<version>_linux_amd64.rpm` and `..._arm64.rpm`

These install to the system PATH and need sudo or an administrator. They put the
extension, tests, bundled agent skill, README and license in the platform share
directory: `/usr/local/share/brw/` on macOS, `C:\Program Files\brw\share\` on
Windows, `/usr/share/brw/` on Linux.

The packages are not yet code-signed. macOS Gatekeeper reports an unidentified
developer: open the `.pkg` with right-click -> Open rather than stripping the
quarantine attribute. Windows SmartScreen shows "Windows protected your PC".
The `.deb` and `.rpm` are unsigned and there is no distribution GPG key.
[`release-signing.md`](release-signing.md) tracks what is needed to change that.

## Verify what you downloaded

Every release artifact carries a SHA256 in `SHA256SUMS.txt` and a GitHub build
provenance attestation:

```sh
gh attestation verify brw_<version>_macos_universal.pkg --repo Don-Works/brw
```

The checksum proves the file matches the release page. The attestation proves
this repository's release workflow built it from that tag, which is the part a
checksum cannot tell you. It needs `gh` 2.49+ and `gh auth login`.

## brwctl setup

```sh
brwctl setup
```

Takes a machine with brw binaries on it to a connected bridge. Every step is
idempotent and reports what it did or why it was already satisfied. `--dry-run`
prints the whole plan and performs none of it.

1. **Profile policy.** Writes the policy into the platform user config
   directory (0600) — `~/Library/Application Support/brw/browser-profiles.json`
   on macOS, `~/.config/brw/browser-profiles.json` on Linux — reusing a policy
   already present in any of the standard locations and backing it up to
   `<path>.bak.<UTC timestamp>` before merging. It binds to a
   browser profile directory that exists, so the browser must have been run at
   least once; `--browser` and `--profile-directory` override the choice. No
   package ships a policy.
2. **App Nap.** Sets `NSAppSleepDisabled` for the chosen browser's bundle id on
   macOS. It applies at that browser's next full launch — Cmd-Q, not just
   closing the window.
3. **Background service.** A per-user service running
   `brwd --bridge`, bound to `127.0.0.1:17310` (control) and `127.0.0.1:17311`
   (extension WebSocket); `--http-port N` moves both, the bridge to N+1.
   macOS: `~/Library/LaunchAgents/co.donworks.brwd.<profile>.plist`, logging to
   `~/Library/Logs/brw/brwd-<profile>.log`. Linux:
   `~/.config/systemd/user/brwd-<profile>.service`, logging to
   `~/.local/state/brw/brwd-<profile>.log`; with no user systemd instance, setup
   prints a `nohup` line instead. Windows: a logon scheduled task.
   Setup refuses to replace a pre-existing service that drives the same profile
   or binds the same ports, and prints the path, label and reason.
4. **MCP client registration.** `claude mcp add -s user brw -- …`, derived from
   the same code as `brwctl mcp-config`. brw never edits `~/.claude.json`
   directly, because Claude Code rewrites that file while it runs. Codex with
   `--mcp-client codex|both`; `--mcp-client none` prints the `mcpServers` block.
   A named client is recorded as `mcp_client` in the policy. `brwctl doctor`
   reads it: with `none` recorded, an absent registration is a warning rather
   than a failure, because the config went to a client brw cannot read.
5. **Agent skill.** Copies `skills/brw` into `~/.claude/skills/brw`,
   `~/.agents/skills/brw` and `~/.codex/skills/brw`.
6. **Verify.** Runs the doctor checks and prints what is left to do by hand.

Flags: `--profile` `--workspace` `--browser` `--profile-directory`
`--user-data-dir` `--transport` `--mcp-client` `--http-port` `--profile-policy`
`--app-dir` `--skills-dir` `--no-service` `--dry-run` `--yes` `--help`.

Default names come from the browser and lane: workspace `brw-chrome-profile`
with profile `chrome-profile`, `brw-chromium-profile` with `chromium-profile`,
`brw-chrome-agent` with `chrome-agent` for direct CDP.

### Which browsers

Every Chromium-based browser works over both lanes: the daemon speaks the same
CDP to all of them and the extension loads in all of them. `--browser` takes:

| `--browser` | | Profile directory |
|---|---|---|
| `chrome` | Google Chrome | `~/Library/Application Support/Google/Chrome`, `~/.config/google-chrome` |
| `chromium` | Chromium | `~/Library/Application Support/Chromium`, `~/.config/chromium` |
| `edge` | Microsoft Edge | `~/Library/Application Support/Microsoft Edge`, `~/.config/microsoft-edge` |
| `brave` | Brave Browser | `~/Library/Application Support/BraveSoftware/Brave-Browser`, `~/.config/BraveSoftware/Brave-Browser` |
| `vivaldi` | Vivaldi | `~/Library/Application Support/Vivaldi`, `~/.config/vivaldi` |
| `opera` | Opera | `~/Library/Application Support/com.operasoftware.Opera`, `~/.config/opera` |
| `arc` | Arc | `~/Library/Application Support/Arc/User Data` (macOS only) |

With no `--browser`, setup picks the one that has actually been run — a browser
that is merely installed has no profile directory to bind to — and Chrome breaks
the tie.

A Chromium build that is not in that table still works; it just has to say where
its profiles live:

```sh
brwctl setup --browser comet --user-data-dir ~/Library/Application\ Support/Comet
```

Firefox and Safari are not supported.

Firefox does speak an automation protocol — WebDriver BiDi, over the same
`--remote-debugging-port` flag, with no driver binary. brw has a measured
prototype against Firefox 155 and the primitives are largely there; what stops
it being a lane is not the protocol. BiDi can only be switched on at startup, so
brw would have to be what launches your browser rather than attaching to the one
you already have open. While the remote agent is enabled Firefox sets
`navigator.webdriver = true` on every page, whether or not anything is driving
it, so every page of a signed-in session would be told it is automated. And
there is no extension lane to fall back on: Firefox has no equivalent of
`chrome.debugger`, and a content script can only produce untrusted events.

[bidi-prototype.md](bidi-prototype.md) records what the prototype measured,
primitive by primitive, and the decision taken from it.

Safari's automation route hands you a clean window rather than your signed-in
session, which is the opposite of what brw is for.

### What setup cannot do for you

Loading an unpacked extension is a browser-UI action with no command-line
equivalent, so it stays manual until the Chrome Web Store listing is live:

1. Open `chrome://extensions`, turn on Developer mode (top right), click Load
   unpacked, and select `<app-dir>/extension`. The id must read
   `amocjcgddnoakjijfggdpnefdnboilpe`.
2. Open the extension's Options, read the browser-data disclosure, and click
   **Enable local browser control**. Nothing connects before you do.
3. Quit the browser completely and reopen it, so the App Nap default applies.
4. Restart your MCP client, or run `/mcp` in Claude Code, so it picks up the new
   server.

Between setup and step 1, `brwctl doctor` reports that the extension is not
installed and exits non-zero. That is the expected state, not a broken install.

### Claude Code's own Chrome integration

Claude Code ships a separate Chrome integration, enabled in `~/.claude.json`.
With both switched on the agent sees two browser tool sets and may drive the
wrong one. `brwctl doctor` emits the `claude_in_chrome_enabled` warning when it
finds it; run `/chrome` in Claude Code and turn it off.

## The transports, and what each can do

Three ways to set brw up, and a fourth `brw_identity` can report — `brwd
--remote <endpoint>`, covered below the table. `brw_identity` names which one a
namespace resolved to, and `brwctl doctor` names it with the capabilities it
implies. `tools/list` is narrowed to the lane, so a tool that cannot work on it
is not advertised at all.

| | Extension bridge | Direct CDP | Chrome opt-in |
|---|---|---|---|
| Browser | The real signed-in Chromium browser you already use | A separate brw-owned instance | The real signed-in Chrome you already use |
| How it connects | The brw extension, `chrome.debugger` per operation | brw launches Chrome with a debugging port | Full browser-target CDP, no extension |
| Needs | The extension loaded and enabled | Nothing | Chrome 144+ with the opt-in switched on by hand |
| Existing logins | Yes | No, unless you point it at a cloned profile | Yes |
| Chrome tab groups | Yes | No | No |
| Incognito contexts (`brw_open_incognito`) | No | Yes | Yes |
| Cookies incl. HttpOnly (`brw_cookies`) | No | Yes | Yes |
| Deterministic download capture | No, uses the browser's download folder | Yes, staged in brw's cache | No, uses the browser's download folder |
| Session snapshots (`brw_state`) | No, by policy | Yes | No, by the same policy |
| Headless | No | Yes | No, it is your window |
| `brw_identity` transport | `extension-bridge` | `direct-cdp` | `chrome-opt-in-cdp` |

`brwd --remote <endpoint>` attaches to a DevTools endpoint another process
opened, and reports `transport: "remote-cdp"`. It is the direct-CDP column with
one row changed: no deterministic download capture. `Browser.setDownloadBehavior`
has no scope narrower than a browser context, and brw did not start that
browser, so pointing its downloads at brw's staging directory would move files
belonging to whoever did — and brw deletes that directory when the daemon stops.
`brw_downloads` still reports every download, with `file_paths: false` and no
path. That holds however the endpoint was produced, including when it is the
port a Chrome opt-in published: the refusal reads whether brw started the
browser, not which flag the operator passed.

A loaded `browser.provider` plugin gives a fifth, `transport: "off-host-cdp"`: a
CDP websocket to a browser on the provider's own machine. It is the direct-CDP
column with every row that names something on THIS machine changed, because a
path resolved at the other end of that socket is a path on the provider's disk.
`brw_downloads`, `brw_set_download_path`, `brw_upload_file` and `brw_clipboard`
are refused by name, and a recipe declaring `"requires": ["profile_session"]` is
refused before its first action rather than run signed out. That is the whole
difference from `remote-cdp`, which is pointed at an endpoint on this machine
and keeps all four. See [plugins.md](plugins.md).

### The Chrome opt-in lane

Chrome 136 stopped honouring `--remote-debugging-port` against the default user
data directory, and an extension cannot attach `chrome.debugger` to the BROWSER
target at all. Between them those are why the bridge has no incognito, no
HttpOnly cookie read and no deterministic download capture: the capability is
not missing from brw, it is unreachable from where the bridge stands.

Chrome 144 added the sanctioned way back: at `chrome://inspect/#remote-debugging`
a person can switch remote debugging on for their own browser. That is a third
lane — full browser-target CDP against the profile you are signed into, with no
extension involved.

```sh
brwd --chrome-opt-in
```

It is a human action by design, and brw treats it that way:

- brw never turns the opt-in on and has no flag that would. With it off, `brwd
  --chrome-opt-in` says so, names what to do, and exits. It does not fall back
  to launching Chrome with a debugging flag — doing that would be brw granting
  itself the access Chrome asks you to grant.
- `brwctl doctor` reports the lane as a `chrome_opt_in` check. Off is a skip,
  not a failure: it is the default and the other two lanes work without it. The
  check's fix opens `chrome://inspect/#remote-debugging`; the switch is still
  yours to flip.
- `brw_identity` reports `transport: "chrome-opt-in-cdp"`, distinct from
  `direct-cdp`, because the catalogue differs in both directions.

The port is discovered, not configured: the opt-in allocates one dynamically
and Chrome records it in `DevToolsActivePort` in the user data directory, which
is the only place it appears. Measured on Chrome 153.0.8010.37: with the opt-in
on, that file holds `9222` and `/devtools/browser/<uuid>`; start Chrome with an
explicit `--remote-debugging-port` instead and there is no file at all, because
the caller already knows the port. brw never passes that flag on this lane, so
the file is the channel.

**The opt-in serves no DevTools HTTP endpoints.** Measured on the same Chrome,
headless and headed alike: `/json/version`, `/json/list`, `/json` and `/` all
answer 404 on an opted-in browser, where a Chrome started with
`--remote-debugging-port` serves them. So the second line of
`DevToolsActivePort` is not a convenience — it is the only place the browser
target appears, and brw reads it. It builds the WebSocket URL itself from
loopback, the recorded port and the recorded path, which is narrower than
trusting `/json/version`: no listener gets to name the address brw dials. A
second line carrying a scheme or an authority is refused for that reason. There
is no version to read that way, so `brwctl doctor` says so rather than printing
an empty version.

When the endpoint does answer `/json/version` — a Chrome someone started with
`--remote-debugging-port=0` — brw uses that answer and checks that the browser
WebSocket URL it reports points back at the same loopback port, because a file
left behind by an exited Chrome names a port anything else on the machine may
since have taken. The checked URL is then the one brw dials, rather than
re-asking the endpoint at connect time: a listener holding a stale port can
answer the first question honestly and the second one however it likes.

**Chrome asks before it answers.** On Chrome 144+ each remote debugging
connection is approved by the person at the browser, so the WebSocket handshake
sits unanswered until they allow it — measured against Chrome 153, the dial
never completed and nothing on the wire said why. brw waits two minutes for
that and then fails with a sentence naming the prompt, rather than hanging until
someone kills the daemon. It cannot answer the prompt for you, by the same
design that keeps it from turning the opt-in on.

`brw_state` is refused on this lane. It is the same refusal as on the extension
bridge and for the same reason: sealing the cookies of the browser you are
personally signed into is the "no cookie extraction" non-goal in
[auth-model.md](auth-model.md). The CDP to do it is right there, which is
exactly why the refusal is in the controller and not only in `tools/list`.

`brw_set_download_path` is refused for a related reason, and this one is about
your files rather than your cookies. `Browser.setDownloadBehavior` has no
per-tab and no per-download scope — the narrowest thing it applies to is a whole
browser context, and on this lane the default browser context is your own
windows. Staging downloads there would send every file you downloaded by hand
into a brw directory, named by download id instead of by filename, and brw
deletes that directory when the daemon stops. So on this lane brw turns the
download event stream on and leaves the destination alone: `brw_downloads`
reports every download with its filename, size, state and source tab, and
reports `file_paths: false` and no path, because the file is yours and where it
went was your browser's decision. `brw_assert(assertion:"download")` and
`brw_artifact_capture(kind:"download")` need a path, so they return a named
capability error here and work on a direct-CDP profile.

Neither refusal is a capability gap that a later Chrome could close. Both are
brw declining to use access it has, against a browser it is a guest in.

A profile policy has to grant this lane explicitly: `"chrome_opt_in_allowed":
true` on the profile. It is a separate bit from `direct_cdp_allowed`, which
says brw may launch its own browser against a profile directory. A profile
restricted to the extension bridge is exactly the profile that restriction
exists to protect, so the opt-in lane does not inherit permission from either of
the other two. Without a `--profile` or `--workspace` there is no policy to
consult and no gate, as elsewhere in brwd.

`--chrome-opt-in` attaches to a browser brw did not start, so it cannot be
combined with `--bridge`, `--remote`, `--upstream-http`, `--headless`,
`--login`, `--extension`, `--chrome-arg`, `--chrome-path`,
`--remote-debugging-port`, `--user-data-dir`, `--profile-directory`,
`--unsafe-real-profile`, `--unsafe-allow-default-profile-cdp`, or the launch
network switches. Each is refused by name rather than ignored. Which directory
this lane reads is `--chrome-opt-in-user-data-dir`, or the resolved profile's
`user_data_dir`, or the platform default for `--chrome-opt-in-browser` — and
when a policy profile names a directory, an endpoint discovered anywhere else
is refused, because `brw_identity` would otherwise report a profile the daemon
is not driving.

### Page environment and launch flags

These override what a page believes about its surroundings. All of them are
DevTools Protocol session overrides or Chrome launch switches, and the extension
bridge holds neither: it attaches and detaches `chrome.debugger` around each
operation, and a detach drops every override that session installed. On the
bridge each tool returns a named capability error and is not advertised in
`tools/list` at all, so an agent never spends a call finding out.

`brw_set_download_path` is the one the Chrome opt-in lane also lacks, and there
it is a refusal rather than a gap — see the lane's section above.

| Capability | Tool | Extension bridge | Direct CDP | Chrome opt-in |
|---|---|---|---|---|
| Geolocation override | `brw_set_geolocation` | No | Yes | Yes |
| Offline / latency / throughput | `brw_set_network_conditions` | No | Yes | Yes |
| Media type and `prefers-*` features | `brw_emulate_media` | No | Yes | Yes |
| Per-origin extra request headers | `brw_set_extra_headers` | No | Yes | Yes |
| User agent, Accept-Language, platform | `brw_set_user_agent` | No | Yes | Yes |
| Per-call HTTP credentials | `brw_authenticate` | No | Yes | Yes |
| Download directory | `brw_set_download_path` | No | Yes | No, it would move your own downloads |
| Proxy | `--proxy-server`, `--proxy-bypass-list` | No | Yes, at launch | No, you launched it |
| Certificate errors ignored | `--ignore-https-errors` | No | Yes, at launch | No, you launched it |
| Private CA accepted | `--ca-cert` | No | Yes, at launch | No, you launched it |
| Viewport / device emulation | `brw_emulate_device` | Yes | Yes | Yes |

The four launch switches are read once, when Chrome starts, so `brwd` refuses
them alongside `--bridge`, `--remote`, `--upstream-http` and `--chrome-opt-in`,
which all attach to a browser someone else launched.

`--ignore-https-errors` turns certificate validation off for the whole browser.
It is opt-in per launch and `brw_identity` reports it as `ignore_https_errors`,
because an agent reading a page over that daemon otherwise has no way to tell a
real site from an intercepted one. A proxy that adopts an upstream daemon's
identity reports its upstream's value.

`--ca-cert <bundle.pem>` is the narrow alternative: brw hashes each certificate's
SubjectPublicKeyInfo and passes the hashes to Chrome's
`--ignore-certificate-errors-spki-list`, so a site behind that private CA loads
while every other certificate error in the session is still a real error. It does
NOT install the CA: nothing outside this browser instance is affected, and the
connection remains an error Chrome was told to overlook rather than a validated
one. Adding a root to the profile's NSS database or the OS keychain is the only
way to make a CA genuinely trusted, and brw deliberately does not write to
either — a tool that silently adds roots to your trust store is a tool that can
silently intercept your traffic long after it exits.

`brw_set_extra_headers` binds a header set to named origins and attaches it to
nothing else. The browser-wide way to add headers puts them on every request a
page makes, so an `Authorization` header set that way also reaches the page's
analytics beacons, font CDNs and tracking pixels; brw attaches them from the
request interceptor instead, per request, after checking the request's origin.
Header values are never echoed back in results.

`brw_authenticate` takes the credentials for one navigation and drops them before
it returns. There is no "store these credentials" call and no brw-side credential
state to clear afterwards.

The browser keeps its own copy. Once a challenge has been answered, Chrome holds
that credential in its HTTP-auth cache and re-sends it for that origin for the
rest of the browser session, and CDP has no command to empty that cache. On a
persistent profile that means every later load of the origin is authenticated,
including one a human makes in a visible window. The result says so as
`authentication.browser_cached`. Bound it by authenticating in an incognito
context (`brw_open_incognito`) and disposing it with `brw_close_context`, which
discards the cache with the context; otherwise the credential lives until the
browser restarts.

### Request interception and HAR fixtures

`brw_route` answers a request instead of letting it reach the network. The two
transports intercept by completely different mechanisms, and only one of them
can produce a response.

Direct CDP pauses each request with the DevTools `Fetch` domain and answers it
from the daemon, so it can supply a status, headers and a body. The extension
bridge drives the user's real signed-in Chrome, where interception is a
`declarativeNetRequest` session rule: the rule decides whether a request happens
and Chrome never hands the extension the response. CDP's `Fetch` domain would,
but only through the `chrome.debugger` session the extension attaches and
detaches around each operation, and interception dropped at a detach is worse
than none — the page would reach the real endpoint while the caller believed it
was mocked. So each gap below is a named error, never a silent passthrough.

Interception is the DevTools `Fetch` domain, so the Chrome opt-in lane behaves
exactly as direct CDP does here; the column below covers both.

| Capability | `brw_route` call | Extension bridge | Direct CDP and Chrome opt-in |
|---|---|---|---|
| Refuse a request | `{action:"add", behaviour:"abort"}` | Yes, a `declarativeNetRequest` session rule scoped to the tab, covering every resource type including the top-level navigation | Yes |
| Answer from a body | `{action:"add", behaviour:"fulfill"}` | No | Yes |
| Replay a recorded HAR | `{action:"replay", har_artifact_id}` | No | Yes, fetch and XHR only |
| Redirect a request | — (not an advertised shape) | No, refused by name — see below | No, refused by name — see below |
| Retire a rule after N matches | `times` | No, a declarative rule reports no match count | Yes |
| Report how often a rule fired | `routes[].matched` | No, for the same reason | Yes |

#### Why there is no redirect

Both transports can express a redirect, so this is a decision rather than a
missing primitive. Any `brw_route` call carrying `behaviour:"redirect"` returns
the same named refusal on each, because there is no profile where it would work.
The behaviour is checked at the tool surface, before the action, the pattern,
the tab and the HAR a replay names, so the answer does not change with the shape
the word arrives in.

`redirect` is not among the behaviours `brw_route` advertises, though: the
`behaviour` enum offers `fulfill` and `abort`, because a tool that offers a
value every call for it rejects documents a capability it does not have. brw
does no server-side enum validation, so a client that sends the value through
reaches the refusal above and its reason; a client that validates `inputSchema`
locally rejects the call itself and shows a schema error instead. That reader
gets the reason from the `brw_route` description, which states it in prose.

On the extension bridge a `declarativeNetRequest` redirect action needs host
permissions for the request URL **and** for the request's initiator. brw's
extension holds host permissions for loopback only, on purpose. Chrome does not
refuse a rule it cannot apply: `updateSessionRules` accepts it and
`getSessionRules` lists it, and it simply never fires — measured in
`TestDeclarativeNetRequestRedirectNeverFiresUnderShippedPermissions`, which
moves the two grants independently. The rule does not fire for an off-permission
request URL, and does not fire for a loopback request URL either when the page
asking for it is off-permission; add the one missing grant in each case and the
same rule fires. Shipping the rule without that access is the worst of the
options: an agent would be told a third-party origin was stubbed while the page
reached it for real. Buying the access means holding host permissions for the
sites the user browses, which is a permanent "read and change all your data on
all websites" grant for one behaviour.

On direct CDP the rewrite is `Fetch.continueRequest` with a new URL, and Chrome
does **not** pause the rewritten request again — only the server's own 30x hop
comes back through the interception (`TestRewrittenRequestURLIsNotPausedAgain`).
Every other route behaviour answers a request brw was already shown; a redirect
destination is one the containment listener never sees, so the single behaviour
that widens what a page can reach would also be the one the allowlist could not
gate.

To mock an endpoint use `behaviour:"fulfill"` or `action:"replay"`. To drive a
page against a different live server, point the page at that server.

A HAR fixture is recorded and replayed with the ordinary artifact tools:

```text
brw_artifact_capture {kind:"har"}                  -> artifact_id
brw_route {action:"replay", har_artifact_id, pattern:"*/api/*",
           match:["method","url"], on_miss:"fail"}
```

**A replay answers fetch and XHR, and nothing else.** brw builds a HAR from the
in-page `fetch`/`XMLHttpRequest` wrappers, so the recording holds no document, no
script, no stylesheet and no image. Those requests always go to the network, even
under `pattern:"*"` with `on_miss:"fail"`, because refusing the navigation would
leave a page that never loads; the route reports how many it let through as
`fixture.not_replayable`. `on_miss:"fail"` therefore means the page's API calls
can reach nothing that was not recorded, not that the page is offline.
`on_miss:"passthrough"` (the default) sends an unrecorded call to the network.
Either way the unmatched method and URL are recorded in `fixture.misses`, and
`brw_observe` reports the count and the most recent reasons alongside
`active_routes`.

`match` defaults to `[method,url]`. Add `body` only for a recording whose entries
differ by request body — and only for one captured with `redaction:"none"`: an
ordinary capture stores `[redacted by brw]` in place of every request body, so a
body-keyed replay of one can never match. That combination is refused at install
time with an error naming the capture that supports it.

The same refusal covers a recording whose request bodies were **clipped**. The
2 KiB cap below applies to what a page sent as well as to what it received,
whatever the redaction setting, so an entry recording a larger request holds a
prefix that the whole body the live page sends can never equal. A body-keyed
replay of such a capture would miss every one of those requests, so it is
refused at install time with an error naming the cap.

One body-keyed miss is not preventable at install time: Chrome does not hand the
interception a request body it judges too long, and a body made of file parts
carries no bytes at all. brw will not match that against the empty string — a
recording of a request that genuinely had no body would answer it — so the
request is recorded as a miss whose reason says the body never arrived and that
dropping `body` from `match` would answer it.

Redaction happens at record time. A HAR captured with the default redaction
carries `[redacted by brw]` where a credential header or a request body was, and
replays with those values; there is no un-redacted replay mode.

Bodies in a brw-exported HAR are capture snippets clipped at 2 KiB, requests and
responses alike. A recording of a larger response replays clipped — valid bytes,
but a page parsing it as JSON gets a syntax error. brw does not hide that: the
decoded fixture flags each clipped entry, the `action:"replay"` note says how
many of the recordings are snippets, and the route reports `truncated_entries`
and `served_truncated`. A clipped request body is refused rather than reported,
because it is a match key and nothing downstream can recover from it (above).
There is no import path for an externally produced HAR, so a fixture that needs
whole bodies has to be recorded from endpoints whose traffic fits under the cap.

`brwctl setup --transport direct-cdp` configures the second lane. Running both
against different profiles is supported: one `brwd` per profile, one MCP server
per daemon. Testing several signed-in roles at once wants a second browser
profile rather than a second transport.

## Runtime layout

macOS:

```text
~/Library/Application Support/brw/
  bin/
  config/browser-profiles.json
  extension/
  skills/brw/
  tests/
```

Linux:

```text
~/.local/share/brw/{bin,extension,skills,tests}
~/.local/bin/{brw,brwd,brwctl,brwcheck,brw-devtools-mcp}   # symlinks
```

If `~/.local/bin` is not on your PATH, the installer prints the line to add.

## Build from source

Builds run through [Task](https://taskfile.dev) rather than Make:
`brew install go-task` on macOS, or see the Task install docs. `task --list`
shows every target; `task check` is the full release gate CI runs.

```sh
task test
task build
```

Built binaries: `bin/brw`, `bin/brwd`, `bin/brwctl`, `bin/brwcheck`,
`bin/brw-devtools-mcp`. `task install` puts them in the user-local layout above.
`task package-tarballs` builds the archives the one-line installer consumes.

## Remote install

Copy the built binaries, `extension/`, `skills/`, `tests/`, and a profile policy
to the browser machine. Then generate MCP client config from the policy:

```sh
brwctl mcp-config \
  --workspace brw \
  --profile work-profile \
  --transport remote \
  --profile-policy ~/.config/brw/browser-profiles.json \
  --mode bridge
```

For an installed Chrome profile, the recommended production shape is a
long-lived bridge daemon on the browser machine plus a generated SSH stdio
wrapper on the agent machine:

```sh
# Browser machine
brwd --bridge --http 127.0.0.1:17310 --bridge-addr 127.0.0.1:17311

# Agent machine
brwctl remote-mcp-wrapper \
  --host browser-host \
  --user browser-user \
  --remote-brwd ~/.local/bin/brwd \
  --output ~/.local/bin/brw-browser-mcp
```

The generated wrapper is what MCP clients should run. It keeps browser-control
HTTP bound to loopback on the browser machine and relies on SSH for transport
security.

## brw Extension

The extension is open source (AGPL-3.0). Its public key in
`extension/manifest.json` gives load-unpacked and self-hosted builds the stable
id `amocjcgddnoakjijfggdpnefdnboilpe`. That id is the daemon's
`profilepolicy.DefaultBridgeExtensionID`, so an unconfigured bridge already
trusts the real extension; you only set `bridge_extension_id` for a different
(re-signed) build. Verify that the draft Chrome Web Store item resolves to the
same id before publishing it.

The extension bridges the brw daemon to your real, signed-in browser over
`ws://127.0.0.1` after you review its disclosure and explicitly enable browser
control. It drives visible tabs via the Chrome debugger protocol, blocks
HttpOnly-cookie and bulk-storage CDP access, and does not access Chrome's
password store, passkey store, or profile files. Page-visible values can still
be handled when you ask your configured agent to do so. It is a normal visible
browser, with no stealth / CAPTCHA / MFA bypass.

### Chromium recommended (open source)

Chromium is the browser brw champions. Because Chromium is open source and not
gated by the Chrome Web Store, you can force-install **and** auto-update the
extension from a single policy file — and on Linux you do not need any MDM.

brw self-hosts the distribution on its own site:

- Signed package (CRX): <https://brw.donworks.co.uk/brw.crx>
- Auto-update manifest: <https://brw.donworks.co.uk/updates.xml> (gupdate / Omaha protocol)

The force-install line referenced by every platform's policy is the stable id
joined to the update manifest:

```text
amocjcgddnoakjijfggdpnefdnboilpe;https://brw.donworks.co.uk/updates.xml
```

Once that entry is present, Chromium installs from the update manifest and polls
<https://brw.donworks.co.uk/updates.xml> for new versions automatically.

Ready-made policy files:

- Linux JSON: <https://brw.donworks.co.uk/policies/brw-chromium-policy.json>
- macOS profile: <https://brw.donworks.co.uk/policies/brw-chromium.mobileconfig>
- Windows reg: <https://brw.donworks.co.uk/policies/brw-chromium-policy.reg>

**Linux (no MDM needed).** Drop the policy JSON into the managed-policy
directory; Chromium picks it up on next launch, installs from the update
manifest, and auto-updates:

```sh
# Chromium
sudo cp brw-chromium-policy.json /etc/chromium/policies/managed/
# Chrome
sudo cp brw-chromium-policy.json /etc/opt/chrome/policies/managed/
```

**macOS (profile / MDM required).** Force-install on macOS is only settable
through a managed configuration profile — it is *not* settable from user-domain
defaults. Install the `.mobileconfig` manually or push it via MDM.

**Windows.** Import the `.reg`, or set the equivalent GPO at
`HKLM\SOFTWARE\Policies\Chromium\ExtensionInstallForcelist`.

brw generates these artifacts for you. The private signing key lives outside the
repo:

```sh
brwctl pack-extension --key /path/to/chrome-extension.pem   # builds brw.crx
brwctl update-xml \
  --workspace brw \
  --profile work-profile \
  --profile-policy ~/.config/brw/browser-profiles.json \
  --crx-url https://brw.donworks.co.uk/brw.crx \
  --output dist/extension/updates.xml                        # builds updates.xml
brwctl macos-policy \
  --workspace brw \
  --profile work-profile \
  --profile-policy ~/.config/brw/browser-profiles.json \
  --update-url https://brw.donworks.co.uk/updates.xml \
  --install-mode force_installed \
  --output dist/brw-chromium.mobileconfig                    # builds the .mobileconfig
```

(Tested: Chromium 151 loads the extension with the correct id and bridges to
`brwd` end-to-end; the auto-update endpoint serves a valid `updates.xml` + CRX
with the correct content-types.)

### Zero-policy (Chromium)

If you don't want to install any policy, launch Chromium with the extension
already loaded, then run the bridge — there is nothing to click:

```sh
chromium --load-extension=<path-to>/extension --user-data-dir=<path-to>/profile
brwd --bridge
```

`brwd --extension <path-to>/extension` does the same when brwd launches its own
Chromium in direct-CDP mode (it passes `--load-extension` through). This relies
on `--load-extension`, which is reliable on Chromium; Chrome 137+ dropped
reliable support for it, so use one of the Chrome paths below instead.

### Chrome (also works)

Load unpacked (works today):

```sh
task install-extension   # prints the folder + opens chrome://extensions
```

1. Open `chrome://extensions` in the target Chrome profile.
2. Enable Developer mode.
3. Choose Load unpacked.
4. Select the `extension/` directory.
5. In the Options page that opens, review the browser-data disclosure and click
   **Enable local browser control**.
6. Keep the extension enabled.

One-click Chrome Web Store install is being prepared, but is not live. The
current package, accurate disclosure answers, permission justifications, and
reviewer flow are in [`web-store-listing.md`](web-store-listing.md). Verify the
draft item id against the pinned id before publishing; if it differs, update the
profile policy's `bridge_extension_id` before switching users to it.

Set `bridge_extension_id` in the profile policy only when you ship your own
re-signed build with a different id; the default published id is built in.

For a local multi-profile installation, `task install-mac` also refreshes every
existing `~/Library/Application Support/brw/extension-*` payload without
overwriting its private `bridge-defaults.json`. Install the bundled operating
skill into `~/.claude/skills/brw`, `~/.agents/skills/brw` and
`~/.codex/skills/brw` with `task install-agent-skills`; this
copies instructions only, never the private recipe corpus.

## Keep the browser awake (macOS)

macOS App Nap can freeze a backgrounded browser's extension, which drops the
bridge until the browser is focused again. `brwctl setup` disables it for the
browser it configures. To do it by hand, or for a second browser:

```sh
defaults write org.chromium.Chromium NSAppSleepDisabled -bool YES
defaults write com.google.Chrome     NSAppSleepDisabled -bool YES
```

It applies at that browser's next full launch. See
[reliability.md](reliability.md) for the rest of the staying-connected story:
the extension's service-worker keepalive and an optional watchdog.

## macOS Downloads access

Direct-CDP profiles stage deterministic downloads in brw's private cache and do
not need access to the user's Downloads folder. Chrome's extension debugger API
does not expose browser-level download routing, so extension-backed profiles use
Chrome's configured download folder. Capturing those bytes as a brw artifact may
therefore require Files & Folders consent for the installed `brwd` binary under
System Settings > Privacy & Security > Files & Folders. The metadata-only
download event remains available without file access. brw limits an unanswered
OS permission request to one three-second attempt and makes concurrent/repeated
attempts fail fast, so a missing permission cannot create a retry-driven thread
or CPU storm.

## Verify the install

```sh
brwctl doctor
```

With no arguments it reads the policy `brwctl setup` wrote. Pass `--workspace`,
`--profile` and `--profile-policy` to check a specific profile, or `--app-dir`
for a Homebrew or relocated install. `--json` prints the whole report for a
script; `--timeout` bounds the two live probes.

Each check prints `ok`, `warn`, `skip` or `fail`, and every failing check prints
the command that fixes it. `doctor` exits non-zero if any check failed.

| check | what it proves |
| --- | --- |
| `profile_policy` | the policy file is readable and parses |
| `profile_resolved` | this workspace still resolves to a profile the policy defines |
| `app_files` | `brwd`, `brwcheck`, `brw-devtools-mcp` and the extension payload are installed |
| `browser_binary` | the browser is installed, and which version |
| `browser_profile_dir` | the browser has actually created the bound profile directory |
| `bridge_extension` | the brw extension is installed in that browser profile |
| `daemon` | the daemon answers on its loopback port — and serves *this* workspace, not another one |
| `bridge_connected` | an extension has completed the handshake, not merely been installed |
| `bridge_config` | the bridge endpoint the extension *reports* it is using answers, and which config layer supplied it — falling back to the installed `bridge-defaults.json` when nothing has reported |
| `extension_version` | the loaded build matches the installed payload, and no per-profile copy has fallen behind |
| `mcp_registration` | the agent client launches this install's `brwd`, not a path left by an older one |
| `claude_in_chrome` | Claude Code's own Chrome integration is not competing for the browser |
| `transport` | which lane is live, and therefore which tools exist |

## Upgrade

```sh
brwctl upgrade --check   # report the published version, change nothing
brwctl upgrade           # replace this install with it
```

`upgrade` resolves the latest release, verifies the archive's SHA256 against the
published checksum and its GitHub build provenance with `gh attestation verify`
when `gh` is installed, then replaces the binaries and the extension payload,
refreshes every per-profile extension copy, and restarts the per-user daemons.
The verification rules are the ones `install.sh` uses: an archive with no
published checksum, a checksum that does not match, or a provenance check that
runs and fails all abort before anything on disk is replaced. Unpacking also
refuses any entry whose name climbs out of the unpack directory, and any
symbolic- or hard-link entry at all: the release archive carries no links, so a
link entry only ever exists to have a later entry written through it and land
outside the app directory.

It refuses while a daemon reports work in flight rather than pulling the binary
out from under a running agent; `--force` overrides that. `--refresh-extensions`
re-syncs the per-profile extension copies from the installed payload without
downloading anything, which is what `doctor` sends you to when one has fallen
behind. Reload the extension in the browser afterwards (`chrome://extensions`,
Reload) so it runs the payload the upgrade wrote.

Every refusal above happens before the swap, so the install is left exactly as
it was. Once the swap has begun, each payload directory is removed and then
rewritten, so a failure part-way through (a full disk, a permission change)
leaves the app directory partly replaced — as `install.sh` does. Re-run
`brwctl upgrade` or the one-line installer to finish it; the daemon keeps
running the binary it started with until it is restarted either way.

A profile's own `bridge-defaults.json` is install state, not payload: the
upgrade carries each per-profile extension copy's own file across and never lets
one profile's endpoint reach another's copy. The file holds an endpoint and
nothing secret — the handshake token is minted per daemon launch and never
written there.

The cost of carrying it across is that a machine which once had one keeps it,
including after the daemon's port moved. No release archive contains the file, so
nothing ever corrects it. `doctor`'s `bridge_config` check is what names that: it
asks the extension which endpoint it is using — a connected extension reports it
in its hello, and so does one whose handshake was refused for having no token,
which is what a dead *status* URL produces — and falls back to reading the file
only when no extension has reported anything. The file alone is never the
verdict: the extension's `chrome.storage.local` config silently overrides it, so
a stale file next to a working stored config is drift worth printing, not a
fault.

Drift that moves the *websocket* URL (a stored `bridgeUrl` or `bridgePort`, which
the status URL is derived from) reaches no daemon at all, so no handshake is
recorded and `bridge_config` has nothing to name. `bridge_connected` is the check
that goes red on that machine, and the `bridge_config` skip says so.

Both endpoints `bridge_config` reads come from outside `brwctl` — a handshake
report arrives on the bridge's unauthenticated websocket, and
`bridge-defaults.json` is a file no release rewrites — so both must be `http://`
on a loopback port before the check fetches or prints them. One that is not is
its own red line, naming the fault without repeating the value.
