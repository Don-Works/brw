# Install

## Build

```sh
make test
make build
make package-darwin-arm64
```

Built binaries:

- `bin/brwd`
- `bin/brwctl`
- `bin/brwcheck`
- `bin/brw-devtools-mcp`

## Runtime Layout

macOS:

```text
~/Library/Application Support/brw/
  bin/
  config/browser-profiles.json
  extension/
  tests/
```

Linux:

```text
~/.local/bin/
~/.local/share/brw/
```

## Remote Install

Copy the built binaries, `extension/`, `tests/`, and a profile policy to the
browser machine. Then generate MCP client config from the policy:

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
  --host max-air \
  --user maxrevitt \
  --remote-brwd ~/.local/bin/brwd \
  --output ~/.local/bin/brw-max-air-mcp
```

The generated wrapper is what MCP clients should run. It keeps browser-control
HTTP bound to loopback on the browser machine and relies on SSH for transport
security.

## brw Chrome Extension

The extension pins a public key in `extension/manifest.json`, so it always loads
with the same stable id — `amocjcgddnoakjijfggdpnefdnboilpe` — whether loaded
unpacked or installed from the Chrome Web Store. That id is the daemon's
`profilepolicy.DefaultBridgeExtensionID`, so an unconfigured bridge already
trusts it; you only set `bridge_extension_id` for a different (re-signed) build.

Development install (works today):

```sh
make install-extension   # prints the folder + opens chrome://extensions
```

1. Open `chrome://extensions` in the target Chrome profile.
2. Enable Developer mode.
3. Choose Load unpacked.
4. Select the `extension/` directory.
5. Keep the extension enabled.

Chrome Web Store (one-click, coming soon): an unlisted listing is planned for
one-click install and auto-updates, sharing the same id — no policy change when
you switch from the unpacked build.

Managed install:

```sh
brwctl pack-extension --key /path/to/chrome-extension.pem
brwctl update-xml \
  --workspace brw \
  --profile work-profile \
  --profile-policy ~/.config/brw/browser-profiles.json \
  --crx-url <crx-url> \
  --output dist/extension/updates.xml
brwctl macos-policy \
  --workspace brw \
  --profile work-profile \
  --profile-policy ~/.config/brw/browser-profiles.json \
  --update-url <updates-url> \
  --output dist/brw-chrome.mobileconfig
```

Set `bridge_extension_id` in the profile policy only when you ship your own
re-signed build with a different id; the default published id is built in.

## Verify

```sh
brwctl doctor \
  --workspace brw \
  --profile work-profile \
  --profile-policy ~/.config/brw/browser-profiles.json
```

`doctor` fails if app files are missing, the profile is not allowed, or the
expected `brw` extension is not installed.
