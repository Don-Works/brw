# Install

The runtime control surface is `agent-browserd --mcp` over stdio. For remote
machines, stdio is carried by SSH. Helper scripts are not part of the runtime
contract.

## Installed Layout

macOS browser machines use:

```text
~/Library/Application Support/agent-browser/
  bin/agent-browserd
  bin/agent-browserctl
  bin/browsercheck
  bin/agent-browser-devtools-mcp
  config/browser-profiles.json
  extension/
  tests/
```

Linux installs use:

```text
~/.local/bin/
~/.local/share/agent-browser/
```

## Build

```sh
cd agent-browser
make test
make build
make package-darwin-arm64
```

## Remote macOS Install Over Tailscale SSH

Use the Tailscale DNS name as the SSH host identity:

```sh
ssh maxrevitt@max-air 'mkdir -p "$HOME/Library/Application Support/agent-browser/bin" "$HOME/Library/Application Support/agent-browser/config" "$HOME/Library/Application Support/agent-browser/extension" "$HOME/Library/Application Support/agent-browser/tests"'

scp bin/agent-browserd-darwin-arm64 'maxrevitt@max-air:Library/Application Support/agent-browser/bin/agent-browserd'
scp bin/agent-browserctl-darwin-arm64 'maxrevitt@max-air:Library/Application Support/agent-browser/bin/agent-browserctl'
scp bin/browsercheck-darwin-arm64 'maxrevitt@max-air:Library/Application Support/agent-browser/bin/browsercheck'
scp bin/agent-browser-devtools-mcp-darwin-arm64 'maxrevitt@max-air:Library/Application Support/agent-browser/bin/agent-browser-devtools-mcp'
scp ../.mcplexer/config/browser-profiles.json 'maxrevitt@max-air:Library/Application Support/agent-browser/config/browser-profiles.json'
scp -r extension/* 'maxrevitt@max-air:Library/Application Support/agent-browser/extension/'
scp -r tests/* 'maxrevitt@max-air:Library/Application Support/agent-browser/tests/'
ssh maxrevitt@max-air 'chmod +x "$HOME/Library/Application Support/agent-browser/bin/"*'
```

These commands install files only. They do not control Chrome.

## MCP Runtime

Generate the MCP config from the workspace policy:

```sh
./bin/agent-browserctl mcp-config \
  --profile max-gmail \
  --transport max-air \
  --profile-policy ../.mcplexer/config/browser-profiles.json \
  --mode bridge
```

The generated command runs `agent-browserd` on `max-air` through SSH.
The Chrome profile remains on max-air.

## Bridge Extension

Chrome 137+ branded builds do not reliably accept `--load-extension` for
unpacked extension injection. Do not depend on that flag for installed-profile
auth.

The bridge extension has a stable ID:

```text
hkomepfdcddgepbdalomhabiphokllkd
```

### Development Install

For one machine, install once in the visible Chrome profile:

1. Open `chrome://extensions` in the target Chrome profile.
2. Enable Developer mode.
3. Choose Load unpacked.
4. Select `~/Library/Application Support/agent-browser/extension`.
5. Keep the extension enabled.

After that, use `agent-browserd --bridge --mcp` over SSH. The extension connects
to `ws://127.0.0.1:17311/extension` on the same machine.

### Managed/Repeatable Install

For repeatable installs, package the extension and install it through a private
Chrome Web Store listing or managed Chrome policy.

On unmanaged macOS Chrome, external CRX installs are restricted. A self-hosted
CRX policy is viable only when the Chrome instance is managed through MDM,
Chrome Enterprise Core, or equivalent management. For a personal unmanaged Mac,
use the one-time Developer Mode install until the extension is published through
a private Web Store channel.

Create the CRX and update XML:

```sh
./bin/agent-browserctl pack-extension
./bin/agent-browserctl update-xml \
  --profile max-gmail \
  --profile-policy ../.mcplexer/config/browser-profiles.json \
  --crx-url https://max-air/agent-browser/agent-browser-bridge.crx \
  --output dist/extension/updates.xml
```

Generate a macOS Chrome configuration profile:

```sh
./bin/agent-browserctl macos-policy \
  --profile max-gmail \
  --profile-policy ../.mcplexer/config/browser-profiles.json \
  --update-url https://max-air/agent-browser/updates.xml \
  --output dist/agent-browser-chrome.mobileconfig
```

Install the `.mobileconfig` with an MDM/profile manager or managed Chrome setup.
For managed policy, Chrome supports `ExtensionSettings` with `normal_installed`
or `force_installed`.

## Doctor

After install, verify the app and Chrome profile:

```sh
ssh maxrevitt@max-air \
  '"$HOME/Library/Application Support/agent-browser/bin/agent-browserctl" doctor --profile max-gmail --profile-policy "$HOME/Library/Application Support/agent-browser/config/browser-profiles.json"'
```

`doctor` fails if the requested profile is not allowed, app files are missing,
or the expected bridge extension ID is not installed in the Chrome profile.

## DevTools MCP Companion

Use `agent-browser-devtools-mcp` only through policy:

```sh
agent-browser-devtools-mcp --profile agent-revitt --cdp-endpoint http://127.0.0.1:9222
```

For installed-profile auth profiles, the wrapper fails closed until Chrome
DevTools MCP can be correlated to the same workspace profile through an approved
Chrome permission flow or managed extension install.
