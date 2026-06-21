# brw Chrome Extension

This is the Chrome extension transport for installed-profile auth.

The manifest pins a public `key`, so the extension always loads with the same
stable id — `amocjcgddnoakjijfggdpnefdnboilpe` — for both unpacked and Chrome
Web Store installs. That id is baked into the daemon as
`DefaultBridgeExtensionID`, so an unconfigured bridge trusts it with no policy
edit. Only set `bridge_extension_id` if you re-sign the extension with your own
key (which produces a different id).

## What It Does

- Connects to `ws://127.0.0.1:17311/extension`.
- Uses `chrome.debugger` as a CDP transport for visible tabs.
- Sends tab summaries and CDP results to `brwd --bridge`.
- Raises desktop notifications (via `chrome.notifications`, requires the
  `notifications` permission) when the daemon sends a `notify` command so the
  user is pulled back at human-handoff points (MFA/CAPTCHA/purchase
  confirmation), on completion, or on error — even when the tab is backgrounded.
- Never reads or exports Chrome cookies, passwords, passkeys, or profile files.

## Install Modes

For development, run `make install-extension` from the repo root (it prints the
folder and opens `chrome://extensions`), then Developer mode → Load unpacked →
select this directory.

A one-click, unlisted Chrome Web Store listing is on the way — same id, plus
auto-updates. For managed fleets, package a CRX for managed Chrome policy.
Self-hosted CRX installs on macOS require managed Chrome / MDM / Chrome
Enterprise; unmanaged personal Chrome should use the Developer Mode install or
the Web Store.

```sh
brwctl pack-extension --key /path/to/chrome-extension.pem
brwctl update-xml --workspace brw --profile work-profile --crx-url <crx-url>
brwctl macos-policy --workspace brw --profile work-profile --update-url <updates-url>
```

Chrome 137+ branded builds do not reliably support `--load-extension` for
installing unpacked extensions. Do not depend on launch flags for installed
Chrome profiles.
