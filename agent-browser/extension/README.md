# agent-browser Profile Bridge Extension

This is the minimal Chrome extension transport for installed-profile auth.

Extension ID:

```text
hkomepfdcddgepbdalomhabiphokllkd
```

The ID is stable because `manifest.json` contains the extension public key. The
matching packaging key lives in `../packaging/agent-browser-bridge.pem` and
should stay in the private repo.

## What It Does

- Connects to `ws://127.0.0.1:17311/extension`.
- Uses `chrome.debugger` as a CDP transport for visible tabs.
- Sends tab summaries and CDP results to `agent-browserd --bridge`.
- Never reads or exports Chrome cookies, passwords, passkeys, or profile files.

## Install Modes

For development, load this directory once through `chrome://extensions` in the
target profile.

For repeatable deployment, publish it through a private Chrome Web Store channel
or package it as a CRX for managed Chrome policy. Self-hosted CRX installs on
macOS require managed Chrome/MDM/Chrome Enterprise; unmanaged personal Chrome
should use the one-time Developer Mode install.

```sh
agent-browserctl pack-extension
agent-browserctl update-xml --profile max-gmail --crx-url https://example/agent-browser-bridge.crx
agent-browserctl macos-policy --profile max-gmail --update-url https://example/updates.xml
```

Chrome 137+ branded builds do not reliably support `--load-extension` for
installing unpacked extensions. Do not depend on launch flags for installed
Chrome profiles.
