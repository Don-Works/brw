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
