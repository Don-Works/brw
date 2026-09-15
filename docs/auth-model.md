# Auth Model

`brw` never copies browser profile data. It controls a visible browser.

## Launch Mode

`brwd` launches the chosen Chromium browser with a persistent non-default
profile:

```text
~/.brw/chrome-profile
```

The user signs in once. OAuth sessions, cookies, extensions, downloads, and
passkeys are reused by that profile.

## Attach Mode

`brwd` can attach to a Chrome instance that another wrapper started with remote
debugging enabled:

```sh
brwd --remote http://127.0.0.1:9222
```

## Installed Chrome Profile

Chrome 136+ does not allow remote debugging against the default Chrome data
directory. To use an already-authenticated installed profile, load the `brw`
Chrome extension in that profile.

The `brw` extension uses `chrome.debugger` as the CDP transport and connects to the
local daemon. The daemon exposes MCP/HTTP; the browser profile stays where it is.

## Headless on a profile brw owns

`brwd --headless` applies to direct-CDP profiles only, and it changes nothing
about where sessions come from. brw still never reads the user's Chrome data.

The sequence for an authenticated headless lane:

```sh
brwd --workspace brw-agent --login     # headed; sign in; Ctrl-C
brwd --workspace brw-agent             # headless from the profile setting
```

Chrome persists cookies, localStorage and IndexedDB to the profile's own
`--user-data-dir`, so the second run is still signed in. Nothing is copied out
of a browser and nothing is decrypted: brw launched the browser that holds the
session, and the profile is one brw created.

**Headed and headless are sequential on one profile, never concurrent.** Chrome
holds a `SingletonLock` on a live user-data-dir, and `EnsureSafeUserDataDir`
refuses the second launch rather than corrupting the profile. Stop the headed
daemon before starting the headless one. A lane that needs both at once needs
two profiles.

The extension bridge is unaffected — it drives a browser the user is already
running, so `--headless` with `--bridge` is refused rather than ignored.

What the headless lane does not get is Chrome tab groups: `chrome.tabGroups` is
an extension API with no DevTools Protocol equivalent, and a direct-CDP daemon
runs no bridge listener for an extension to connect back through. Loading the
extension with `--extension` does not change that on its own. File-chooser
interception, the bridge's other exclusive, is plain CDP
(`Page.setInterceptFileChooserDialog`) and works on this transport already.

## The extension bridge boundary, and where it stops

**Decision: the boundary is the browser and the network. A process running as
your user is inside it, and `brw` does not claim otherwise.**

### What actually excludes whom

Two guards, and it is worth being exact about which one does the work.

**The websocket Origin pin.** `/extension` accepts only a handshake whose
`Origin` is `chrome-extension://<configured id>`. The browser sets that header
and neither a page nor an extension can change it, so this is what excludes a
**web page** (it cannot present any extension origin) and **another extension
installed in the same browser** (it presents its own id). Pinned by
`TestConfiguredExtensionOriginAcceptedAndOthersRejected`.

**The per-launch token.** The daemon mints it each start, serves it on the
loopback `/status` endpoint, and refuses any hello that does not present it.
Against the two attackers above it adds nothing the Origin pin had not already
stopped. What it does add is *identity*: a token binds a connection to **this**
daemon launch, so an extension pointed at the wrong port is refused instead of
silently driving somebody else's browser. That is why the refusal now carries the
endpoint it tried, and why `brwctl doctor` can name it.

`/status` serves the token to a request with a loopback Host, an `Origin` that is
absent or exactly the configured extension's, and a `Sec-Fetch-Site` that is
absent or `none`. The Origin comparison was a **prefix** match on
`chrome-extension://` until it was made exact; that is hygiene — a prefix match on
attacker-supplied input, removed — and not a new boundary, because no reachable
caller was blocked by it.

### The measurement the empty-Origin case rests on

Requiring `Origin` would refuse the only client this endpoint exists for, so the
question is what a real browser puts on the wire. Measured on Chromium
152.0.7977.82 on 2026-09-15 by
`TestMV3ServiceWorkerAndWebPageStatusHeadersAreMeasured`, which loads an unpacked
MV3 extension into that browser and records what arrives:

| caller | `Origin` | `Sec-Fetch-Site` | `Sec-Fetch-Mode` | `Sec-Fetch-Dest` |
| --- | --- | --- | --- | --- |
| MV3 service worker `fetch()` | absent | `none` | `cors` | `empty` |
| extension page `fetch()` | absent | `none` | `cors` | `empty` |
| page on another site, `fetch(…, {mode:"no-cors"})` | absent | `cross-site` | `no-cors` | `empty` |
| page on another site, `<script src>` | absent | `cross-site` | `no-cors` | `script` |
| local process (curl-equivalent) | absent | absent | absent | absent |

Google Chrome 153.0.8010.37 is installed on the same machine. Tried by hand
during development with the same unpacked extension, it ran no service worker at
all — the `--load-extension` removal `docs/install.md` records for branded
Chrome 137+. That sentence is not the committed test's output: the test tries
unbranded builds first and stops at the first that answers, so with Chromium
installed it never launches branded Chrome. It does not, on purpose — branded
Chrome starts Google's separate updater, which inherits the test binary's stderr
and can outlive it, and `go test` fails the whole package on that. The test logs
every installed build it found and which one it measured, so the table above can
be checked against the run that produced it.

Two things follow. **No `Origin` distinguishes anything** — every caller sends
none, so the header cannot be required and cannot be used to tell callers apart.
**`Sec-Fetch-Site` distinguishes the browser callers**: a page on another site
reaches a loopback daemon with no `Origin` (a no-cors GET carries none) and
arrives as `cross-site`. The browser sets `Sec-Fetch-*` itself and forbids page
script from overriding it, so `none` is a property a web page cannot present.
That case is now refused. The page could never read the reply — an opaque
response, and the JSON body is not a parseable script — but a page on the internet
causing a request that answers with the token is not left in place on the strength
of the reply being unreadable.

**It distinguishes nothing else.** A local process sends no `Sec-Fetch-*` at all
and may send any value it likes; `brwctl doctor` sends none, so an absent header
is still served. The header is a boundary against browsers, not against the
machine.

Re-check the measurement with
`go test ./internal/extensionbridge -run TestMV3ServiceWorkerAndWebPageStatusHeadersAreMeasured -v`.
It prints the table above for the browser it found and fails if the worker starts
sending an `Origin`, at which point the empty-`Origin` case can be closed.

### What it does not do

Anything ON THIS MACHINE that can send a loopback GET can have the token: a
process running as your user, and any other extension holding
`http://127.0.0.1/*`. There is no request property that separates them from the
extension, because a local process chooses every property of the request it
sends, `Sec-Fetch-Site` included. The guard would be something the attacker
already controls. What the `Sec-Fetch-Site` check buys is the browser half: a
page on the internet cannot choose that header, so it can no longer cause a
request the daemon answers with the token.

For another extension the token is inert — it still cannot open the websocket, and
a token alone drives nothing. For a local process it is not: that process forges
the Origin the browser would have set, presents the token, and takes the bridge.

That is the argument, not severity. The comparable Claude-in-Chrome finding was
closed by HackerOne as requiring local code execution, and this one has the same
prerequisite. The reason to write it down anyway is that a guard an attacker
supplies is not a boundary, and a product that describes one as a boundary is
where the next person's threat model goes wrong.

### Why the alternatives do not move it

- **A unix socket with a peer-credential check.** Chrome extensions cannot open a
  unix socket, so this needs a native-messaging host in front — which is where
  Claude-in-Chrome's Windows named-pipe bug was. The host itself is a binary any
  local process can execute directly, with the same argv Chrome passes it, so
  `LOCAL_PEERCRED` on that channel identifies the attacker's own process. The
  only credential that would actually separate them is the peer's **code
  signature**, which is macOS-specific, needs cgo, and still admits an attacker
  who launches their own Chrome with their own extension.
- **Binding the token to the extension id plus a per-launch nonce.** The
  extension has no private state to prove: its id is public, its directory is
  readable by the same uid, and anything the daemon plants there is readable
  before the extension ever reads it. Every secret in this scheme is stored on
  the same disk, as the same user, as the attacker it is meant to exclude.

Both of those describe the same wall. On a single-user desktop with no per-process
isolation, there is no credential the extension can hold that a process running
as that user cannot also hold. Moving this boundary requires OS-level isolation
(an app sandbox, a keychain ACL bound to a code signature), not a better protocol.

### What did change

`/status` compares the `Origin` exactly instead of by prefix — hygiene, as above,
with no path closed that was open.

`/status` and the consent surface now also refuse a request whose
`Sec-Fetch-Site` is anything other than `none`. That one did close a path that
was open: a page on another site could cause a GET to the loopback daemon with
no `Origin` at all, and the daemon answered it with the token in the body. The
page could not read that body, so nothing leaked — but the request was served,
and it need not have been. The measurement above is what says `none` is a value
a page cannot present and the real extension always does.

The token is no longer written to `~/.brw/bridge-token`. That file existed "for
operator inspection"; nothing in the tree ever read it. What it did was keep a
second copy of the secret at rest, outliving the daemon that minted it and
readable even while `brwd` is not running — which `/status` is not. Every launch
sweeps `~/.brw` for `bridge-token` and `bridge-token-<workspace>` and removes
them all, not only the one this launch would have written, so an upgrade cleans
up after whichever workspace wrote them. The sweep runs on the path every launch
takes rather than inside the extension-bridge branch: only that mode mints a
token, but a machine that upgrades and then runs direct-CDP or upstream-proxy is
exactly the one that would otherwise keep the last bridge launch's file forever.
`BRW_BRIDGE_TOKEN_FILE=<path>` asks for one back, and re-creates that exposure;
the rest are still swept, and so is that one on a launch that minted no token to
put in it.

`brwctl doctor` reads two endpoints that come from outside it — the one a refused
handshake reported, which arrives on the unauthenticated websocket above, and the
one in an installed `bridge-defaults.json` — and it both GETs and prints them.
Both now pass the same gate the extension applies to its own config: `http://`, a
loopback host that is an address rather than a name to resolve, an explicit port,
the `/status` path. Without it, a local process that can forge one handshake
chooses a host this machine resolves and a URL it fetches, which is the network
egress this boundary says brw holds and the local process does not.

Gating a URL gates one request, so doctor follows no redirect. The same process
that forges a handshake also binds a loopback port, and a gate applied once would
let it report an endpoint that passes and then answer with `302 Location:
http://anywhere` — Go's default client takes ten such hops without re-checking
anything. Nothing reached that way is contacted, and nothing named by a failure
of a request doctor did not make is printed: a `url.Error` carries the URL of the
hop that failed, not the one that was gated, so the report is the other half of
the same channel.

### The extension's consent record has no MAC, and cannot have one

Browser control is gated on `brwBrowserControlConsent` in `chrome.storage.local`,
and `isGrantedConsent` validates its shape only. Writing the profile's LevelDB
while Chrome is down forges it.

There is no key to sign it with. Any key the extension could check the record
against would sit in the same `chrome.storage.local`, in the same profile
directory, readable and writable by the same uid as the record it authenticates —
so a forger writes both. A daemon-held key is no better: the daemon serves it to
anything on loopback, which is the same attacker. This is the same wall as the
section above, and it moves for the same reason: OS-level isolation, not a better
record format.

The consent record defends against the case it was built for — the extension
driving a browser before its owner has agreed — which is a question about the
person at the keyboard, not about a local attacker who has already won.

### If you need the stronger boundary today

Run the bridge daemon as a **separate uid** from the one your untrusted work runs
as, or on a machine you do not run untrusted code on. Both put a real kernel
boundary where this section says there is none.

## Non-Goals

- No cookie extraction.
- No encrypted profile database copying.
- No stealth automation.
- No CAPTCHA, MFA, fraud-check, or consent bypass.
- No headless-only fallback for authenticated flows.
- No headless lane that borrows the user's installed-profile session.

## Scoped session snapshots (`brw_state`)

A throwaway direct-CDP or incognito context starts signed out. Every run pays
the login again, which is slow, trips rate limits, and burns MFA codes.
`brw_state` seals the cookies one brw-created browser context already holds for
an explicitly named set of origins, and can put them back into another context
on the same browser host.

### What it is

- **Save and restore, not export.** `brw_state` has four actions: `save`,
  `restore`, `list`, `delete`. None of them returns a stored value. `save` and
  `list` answer with the snapshot id, the origins the caller named, and counts;
  `restore` answers with how many cookies it applied. There is no action that
  reads a snapshot back out, so the material never enters a model context.
- **Browser-host storage.** The snapshot file is written by the daemon that
  owns the browser, under an operator-configured root, `0700` directory and
  `0600` files. In `--upstream-http` mode the remote MCP server forwards the
  command and receives the same metadata; the payload never crosses the
  transport.
- **Encrypted at rest, or refused.** The store needs an operator-supplied key
  file (`brwd --state-key-file`, at least 32 bytes, unreadable by group and
  other, and not inside the state root). Without one, `save` fails by name
  rather than writing a plaintext snapshot. Each file is sealed with AES-256-GCM
  under a per-file key derived from the store key and a random salt, with the
  snapshot id as additional data, so a file cannot be renamed onto another id.
- **Per-origin allowlist.** `origins` is required on `save` **and** on
  `restore`, and must be a non-empty list of exact `scheme://host[:port]`
  origins. A cookie is sealed only if its domain matches one of them by the
  ordinary cookie domain-match rule. The same filter runs again on `restore`,
  against the origins the *restoring* caller named — so a snapshot file cannot
  inject a cookie for an origin the restore did not ask for, even if the file
  contains one. A restore that names no origins is refused rather than
  defaulting to the origins recorded inside the snapshot: that default would
  check the file against itself and the second pass would guarantee nothing.
  What is applied is the intersection of the restoring caller's origins and the
  snapshot's own, so the origins `list` reports for a snapshot bound what any
  restore of it can install.
- **Minimised by default.** A snapshot carries cookies and nothing else. It does
  not capture `localStorage`, `sessionStorage`, IndexedDB, cache, or the
  profile's password or passkey stores. An application that keeps its session in
  `localStorage` will not be signed in by a restore; that is a limit of the
  feature, not an oversight to be worked around later. `redact` narrows further
  by dropping cookie names matching a glob.
- **Expiring.** Every snapshot carries a TTL (default 12 hours, and a per-save
  `ttl_seconds` may only shorten it). A restore after expiry fails by name and
  the file is deleted.

### What it is not

- Not available on any transport that drives the browser the user is personally
  signed into — the extension bridge, and the Chrome opt-in lane. Sealing that
  browser's cookies is exactly the thing brw does not do. `brw_state` is not
  advertised on either and returns a named error if called anyway:
  `ErrSessionStateUnsupported` on the bridge, `ErrSessionStateSignedIn` on the
  opt-in lane. The opt-in lane is the one where the refusal has to be in the
  controller rather than only in `tools/list`: it is full browser-target CDP, so
  the capability to seal those cookies is right there.
- Not a credential vault. It holds session cookies for origins the caller named,
  for hours, in a file only the daemon's user can read, with no path back out.
  It does not hold, and cannot be made to hold, passwords, API keys or OAuth
  refresh tokens that a site did not put in a cookie.
- Not a way to move a session between machines. There is no transfer action, and
  the file is unreadable without the operator's key, which lives on the browser
  host.

### Why this is not the secret store brw refuses to be

The non-goal above is "no cookie extraction": brw must not become the thing an
agent asks for the user's credentials. The threat is brw as an **oracle** — a
surface that turns access to the daemon into possession of the user's session
material.

`brw_state` adds no oracle. The model sees an opaque id and a count. The bytes
travel from a browser context brw itself created, into a sealed file on the same
host, and back into a browser context on that host. An attacker who can call
`brw_state` can already call `brw_open` and drive the authenticated browser
directly, which is strictly more capability than replaying its cookies.

The material is also not the user's. Direct-CDP profiles and incognito contexts
hold sessions that brw's own browser established, and a persistent
`--workspace` profile already stores those same cookies on disk **unencrypted**,
because that is what Chrome does with a profile. A snapshot of them is a smaller,
shorter-lived, encrypted subset of a file that already exists. The profile whose
cookies are genuinely the user's — the installed Chrome the extension bridge
drives — is refused for that reason.

`brw_state` is refused on the `off-host-cdp` transport too, and that one is the
"on that host" clause above holding rather than a second policy. The store lives
on the machine brwd runs on and holds sessions established there; a browser a
`browser.provider` plugin lent brw is on somebody else's machine. All four
actions are refused, not `restore` alone: `restore` would put those cookies into
the provider's browser, `list` and `delete` would let a cloud-backed run
enumerate and destroy this host's snapshots, and `save` would file a snapshot
sealed from the provider's browser alongside the local ones. A provider-backed
daemon also resolves to its own store scope, so it cannot reach the local
default one even if a guard were missed. `remote-cdp` is not on this list:
`--remote` reaches a browser on this machine, so the store and the browser are
on the same host and the clause does not bite.

What would contradict the non-goal is a read action. A `brw_state` that returned
cookie values, or a snapshot format the daemon would decrypt for a caller, would
make brw an extraction tool no matter how the tool was described. That action
does not exist, and its absence is what the security argument rests on.
