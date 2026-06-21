# brw Chrome Extension

This is the Chrome extension transport for installed-profile auth.

For development installs, Chrome assigns the extension ID when you load the
unpacked directory. For managed installs, package the extension with your own
Chrome signing material and put the resulting ID in `bridge_extension_id` in
your profile policy.

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

For development, load this directory once through `chrome://extensions` in the
target profile.

For repeatable deployment, publish it through a private Chrome Web Store channel
or package it as a CRX for managed Chrome policy. Self-hosted CRX installs on
macOS require managed Chrome/MDM/Chrome Enterprise; unmanaged personal Chrome
should use the one-time Developer Mode install.

```sh
brwctl pack-extension --key /path/to/chrome-extension.pem
brwctl update-xml --workspace brw --profile work-profile --crx-url <crx-url>
brwctl macos-policy --workspace brw --profile work-profile --update-url <updates-url>
```

Chrome 137+ branded builds do not reliably support `--load-extension` for
installing unpacked extensions. Do not depend on launch flags for installed
Chrome profiles.
