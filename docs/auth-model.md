# Auth Model

`brw` never copies Chrome profile data. It controls a visible browser.

## Launch Mode

`brwd` launches Chrome/Chromium with a persistent non-default profile:

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

## Non-Goals

- No cookie extraction.
- No encrypted profile database copying.
- No stealth automation.
- No CAPTCHA, MFA, fraud-check, or consent bypass.
- No headless-only fallback for authenticated flows.
