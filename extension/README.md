# brw Chrome Extension

This is the Chrome extension transport for installed-profile auth.

The manifest pins a public `key`, so the extension always loads with the same
stable id — `amocjcgddnoakjijfggdpnefdnboilpe` — whether loaded unpacked,
installed from the self-hosted CRX, or installed from the Chrome Web Store. That
id is baked into the daemon as
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

### Chromium recommended (open source)

brw champions Chromium: because it's open source and not gated by the Chrome
Web Store, you can force-install **and** auto-update the extension from a single
policy file. brw self-hosts the distribution on its own site:

- Signed package (CRX): `https://brw.donworks.co.uk/brw.crx`
- Auto-update manifest: `https://brw.donworks.co.uk/updates.xml` (gupdate/Omaha)
- `ExtensionInstallForcelist` entry:
  `amocjcgddnoakjijfggdpnefdnboilpe;https://brw.donworks.co.uk/updates.xml`

Drop the matching policy file for your platform:

- Linux (no MDM needed):
  [`brw-chromium-policy.json`](https://brw.donworks.co.uk/policies/brw-chromium-policy.json)
  → `/etc/chromium/policies/managed/` (Chromium) or
  `/etc/opt/chrome/policies/managed/` (Chrome). Chromium installs from the
  update manifest and auto-updates.
- macOS (requires a managed configuration profile / MDM — force-install is
  **not** settable from user-domain defaults):
  [`brw-chromium.mobileconfig`](https://brw.donworks.co.uk/policies/brw-chromium.mobileconfig),
  installed manually or via MDM.
- Windows:
  [`brw-chromium-policy.reg`](https://brw.donworks.co.uk/policies/brw-chromium-policy.reg),
  or set the equivalent GPO at
  `HKLM\SOFTWARE\Policies\Chromium\ExtensionInstallForcelist`.

`brwctl` generates these artifacts (the private signing key lives outside the
repo):

```sh
brwctl pack-extension --key /path/to/chrome-extension.pem
brwctl update-xml --crx-url https://brw.donworks.co.uk/brw.crx
brwctl macos-policy --update-url https://brw.donworks.co.uk/updates.xml --install-mode force_installed
```

Zero-policy option on Chromium: launch Chromium with the extension loaded, then
run the bridge — nothing to click:

```sh
chromium --load-extension=<path>/extension --user-data-dir=<path>/profile
brwd --bridge
```

(`brwd --extension <path>/extension` does the same when brwd launches its own
Chromium in direct-CDP mode.) `--load-extension` is reliable on Chromium.

Verified: Chromium 151 loads the extension with the correct id and bridges to
`brwd` end-to-end; the auto-update endpoint (`updates.xml` + CRX) is valid and
served with correct content-types.

### Chrome (also works)

For development, run `make install-extension` from the repo root (it prints the
folder and opens `chrome://extensions`), then Developer mode → Load unpacked →
select this directory.

A one-click, unlisted Chrome Web Store build is in review (not live yet) — same
id, plus auto-updates. Chrome 137+ branded builds dropped reliable
`--load-extension`, so use the load-unpacked path (or the Web Store once live)
rather than launch flags on Chrome.
