# Install

## Native Installers

GitHub releases ship platform-native installers:

- Windows: `brw_<version>_windows_amd64.msi` and `brw_<version>_windows_arm64.msi`
- macOS: `brw_<version>_macos_universal.pkg`
- Debian/Ubuntu Linux: `brw_<version>_linux_amd64.deb` and `brw_<version>_linux_arm64.deb`
- Fedora/RHEL Linux: `brw_<version>_linux_amd64.rpm` and `brw_<version>_linux_arm64.rpm`

The installers put the brw commands on the platform PATH and install the
extension, tests, README, and license into the platform share directory:

- Windows: `C:\Program Files\brw\share\`
- macOS: `/usr/local/share/brw/`
- Linux: `/usr/share/brw/`

Download them from <https://github.com/Don-Works/brw/releases>.

## Build From Source

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
  --host browser-host \
  --user browser-user \
  --remote-brwd ~/.local/bin/brwd \
  --output ~/.local/bin/brw-browser-mcp
```

The generated wrapper is what MCP clients should run. It keeps browser-control
HTTP bound to loopback on the browser machine and relies on SSH for transport
security.

## brw Extension

The extension is open source (AGPL-3.0). It pins a public key in
`extension/manifest.json`, so it always loads with the same stable id —
`amocjcgddnoakjijfggdpnefdnboilpe` — whether loaded unpacked, installed from the
self-hosted CRX, or installed from the Chrome Web Store. That id is the daemon's
`profilepolicy.DefaultBridgeExtensionID`, so an unconfigured bridge already
trusts the real extension; you only set `bridge_extension_id` for a different
(re-signed) build.

The extension bridges the brw daemon to your real, signed-in browser over
`ws://127.0.0.1` and drives visible tabs via the Chrome debugger protocol. It
never reads cookies, passwords, or passkeys — it is a normal visible browser, no
stealth / CAPTCHA / MFA bypass.

### Chromium recommended (open source)

Chromium is the browser brw champions. Because Chromium is open source and not
gated by the Chrome Web Store, you can force-install **and** auto-update the
extension from a single policy file — and on Linux you do not need any MDM.

brw self-hosts the distribution on its own site:

- Signed package (CRX): <https://brw.donworks.co.uk/brw.crx>
- Auto-update manifest: <https://brw.donworks.co.uk/updates.xml> (gupdate / Omaha protocol)

The force-install line referenced by every platform's policy is the stable id
joined to the update manifest:

```text
amocjcgddnoakjijfggdpnefdnboilpe;https://brw.donworks.co.uk/updates.xml
```

Once that entry is present, Chromium installs from the update manifest and polls
<https://brw.donworks.co.uk/updates.xml> for new versions automatically.

Ready-made policy files:

- Linux JSON: <https://brw.donworks.co.uk/policies/brw-chromium-policy.json>
- macOS profile: <https://brw.donworks.co.uk/policies/brw-chromium.mobileconfig>
- Windows reg: <https://brw.donworks.co.uk/policies/brw-chromium-policy.reg>

**Linux (no MDM needed).** Drop the policy JSON into the managed-policy
directory; Chromium picks it up on next launch, installs from the update
manifest, and auto-updates:

```sh
# Chromium
sudo cp brw-chromium-policy.json /etc/chromium/policies/managed/
# Chrome
sudo cp brw-chromium-policy.json /etc/opt/chrome/policies/managed/
```

**macOS (profile / MDM required).** Force-install on macOS is only settable
through a managed configuration profile — it is *not* settable from user-domain
defaults. Install the `.mobileconfig` manually or push it via MDM.

**Windows.** Import the `.reg`, or set the equivalent GPO at
`HKLM\SOFTWARE\Policies\Chromium\ExtensionInstallForcelist`.

brw generates these artifacts for you. The private signing key lives outside the
repo:

```sh
brwctl pack-extension --key /path/to/chrome-extension.pem   # builds brw.crx
brwctl update-xml \
  --workspace brw \
  --profile work-profile \
  --profile-policy ~/.config/brw/browser-profiles.json \
  --crx-url https://brw.donworks.co.uk/brw.crx \
  --output dist/extension/updates.xml                        # builds updates.xml
brwctl macos-policy \
  --workspace brw \
  --profile work-profile \
  --profile-policy ~/.config/brw/browser-profiles.json \
  --update-url https://brw.donworks.co.uk/updates.xml \
  --install-mode force_installed \
  --output dist/brw-chromium.mobileconfig                    # builds the .mobileconfig
```

(Tested: Chromium 151 loads the extension with the correct id and bridges to
`brwd` end-to-end; the auto-update endpoint serves a valid `updates.xml` + CRX
with the correct content-types.)

### Zero-policy (Chromium)

If you don't want to install any policy, launch Chromium with the extension
already loaded, then run the bridge — there is nothing to click:

```sh
chromium --load-extension=<path-to>/extension --user-data-dir=<path-to>/profile
brwd --bridge
```

`brwd --extension <path-to>/extension` does the same when brwd launches its own
Chromium in direct-CDP mode (it passes `--load-extension` through). This relies
on `--load-extension`, which is reliable on Chromium; Chrome 137+ dropped
reliable support for it, so use one of the Chrome paths below instead.

### Chrome (also works)

Load unpacked (works today):

```sh
make install-extension   # prints the folder + opens chrome://extensions
```

1. Open `chrome://extensions` in the target Chrome profile.
2. Enable Developer mode.
3. Choose Load unpacked.
4. Select the `extension/` directory.
5. Keep the extension enabled.

One-click Chrome Web Store install: an unlisted listing is **in review** (not
live yet). It shares the same id, so switching to it later needs no policy
change.

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
