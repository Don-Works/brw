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

- Not available on the extension-bridge transport. That transport drives the
  browser the user is personally signed into, and sealing its cookies is exactly
  the thing brw does not do. `brw_state` is not advertised there and returns
  `ErrSessionStateUnsupported` by name if called.
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
shorter-lived, encrypted subset of a file that already exists. The one profile
whose cookies are genuinely the user's — the installed Chrome the extension
bridge drives — is the one transport where `brw_state` is refused.

What would contradict the non-goal is a read action. A `brw_state` that returned
cookie values, or a snapshot format the daemon would decrypt for a caller, would
make brw an extraction tool no matter how the tool was described. That action
does not exist, and its absence is what the security argument rests on.
