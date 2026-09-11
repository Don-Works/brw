# Install

## One line, no sudo

```sh
curl -fsSL https://brw.donworks.co.uk/install.sh | sh
```

The installer writes nothing outside `$HOME` and never asks for a privileged
step. It resolves the latest release, downloads the archive for your OS and
architecture, verifies its SHA256, verifies the GitHub build provenance
attestation when `gh` is installed and authenticated, unpacks it, and then runs
`brwctl setup` (below). Read it first if you would rather not pipe a URL into a
shell: <https://brw.donworks.co.uk/install.sh>.

| Variable | Effect |
|---|---|
| `BRW_VERSION` | Release to install, with or without the leading `v`. Default: the latest release tag. |
| `BRW_INSTALL_DIR` | Payload directory. Must be absolute. |
| `BRW_BIN_DIR` | Where the command symlinks go. Defaults to `~/.local/bin`, or to `<install dir>/bin` when `BRW_INSTALL_DIR` is set, so a relocated install stays self-contained. |
| `BRW_BASE_URL` | Where to fetch the archive from. For mirrors and testing. |
| `BRW_NO_SETUP` | Stop after unpacking; do not run `brwctl setup`. |
| `BRW_SKIP_ATTESTATION` | Skip the provenance check. The SHA256 check still runs. |

A checksum mismatch aborts before anything is written. An explicit attestation
failure aborts; an unauthenticated `gh` warns and continues. Re-running replaces
`bin extension tests skills doc` and preserves `config/` and
`extension/bridge-defaults.json`.

## Homebrew

```sh
brew install don-works/tap/brw
```

Homebrew puts the whole tree in the formula prefix, so that prefix is the app
directory:

```sh
brwctl doctor --app-dir "$(brew --prefix brw)"
```

## Native installers

For managed and multi-user machines, GitHub releases ship platform packages:

- macOS: `brw_<version>_macos_universal.pkg`
- Windows: `brw_<version>_windows_amd64.msi` and `..._arm64.msi`
- Debian/Ubuntu: `brw_<version>_linux_amd64.deb` and `..._arm64.deb`
- Fedora/RHEL: `brw_<version>_linux_amd64.rpm` and `..._arm64.rpm`

These install to the system PATH and need sudo or an administrator. They put the
extension, tests, bundled agent skill, README and license in the platform share
directory: `/usr/local/share/brw/` on macOS, `C:\Program Files\brw\share\` on
Windows, `/usr/share/brw/` on Linux.

The packages are not yet code-signed. macOS Gatekeeper reports an unidentified
developer: open the `.pkg` with right-click -> Open rather than stripping the
quarantine attribute. Windows SmartScreen shows "Windows protected your PC".
The `.deb` and `.rpm` are unsigned and there is no distribution GPG key.
[`release-signing.md`](release-signing.md) tracks what is needed to change that.

## Verify what you downloaded

Every release artifact carries a SHA256 in `SHA256SUMS.txt` and a GitHub build
provenance attestation:

```sh
gh attestation verify brw_<version>_macos_universal.pkg --repo Don-Works/brw
```

The checksum proves the file matches the release page. The attestation proves
this repository's release workflow built it from that tag, which is the part a
checksum cannot tell you. It needs `gh` 2.49+ and `gh auth login`.

## brwctl setup

```sh
brwctl setup
```

Takes a machine with brw binaries on it to a connected bridge. Every step is
idempotent and reports what it did or why it was already satisfied. `--dry-run`
prints the whole plan and performs none of it.

1. **Profile policy.** Writes the policy into the platform user config
   directory (0600) — `~/Library/Application Support/brw/browser-profiles.json`
   on macOS, `~/.config/brw/browser-profiles.json` on Linux — reusing a policy
   already present in any of the standard locations and backing it up to
   `<path>.bak.<UTC timestamp>` before merging. It binds to a
   browser profile directory that exists, so the browser must have been run at
   least once; `--browser` and `--profile-directory` override the choice. No
   package ships a policy.
2. **App Nap.** Sets `NSAppSleepDisabled` for the chosen browser's bundle id on
   macOS. It applies at that browser's next full launch — Cmd-Q, not just
   closing the window.
3. **Background service.** A per-user service running
   `brwd --bridge`, bound to `127.0.0.1:17310` (control) and `127.0.0.1:17311`
   (extension WebSocket); `--http-port N` moves both, the bridge to N+1.
   macOS: `~/Library/LaunchAgents/co.donworks.brwd.<profile>.plist`, logging to
   `~/Library/Logs/brw/brwd-<profile>.log`. Linux:
   `~/.config/systemd/user/brwd-<profile>.service`, logging to
   `~/.local/state/brw/brwd-<profile>.log`; with no user systemd instance, setup
   prints a `nohup` line instead. Windows: a logon scheduled task.
   Setup refuses to replace a pre-existing service that drives the same profile
   or binds the same ports, and prints the path, label and reason.
4. **MCP client registration.** `claude mcp add -s user brw -- …`, derived from
   the same code as `brwctl mcp-config`. brw never edits `~/.claude.json`
   directly, because Claude Code rewrites that file while it runs. Codex with
   `--mcp-client codex|both`; `--mcp-client none` prints the `mcpServers` block.
5. **Agent skill.** Copies `skills/brw` into `~/.claude/skills/brw`,
   `~/.agents/skills/brw` and `~/.codex/skills/brw`.
6. **Verify.** Runs the doctor checks and prints what is left to do by hand.

Flags: `--profile` `--workspace` `--browser` `--profile-directory`
`--user-data-dir` `--transport` `--mcp-client` `--http-port` `--profile-policy`
`--app-dir` `--skills-dir` `--no-service` `--dry-run` `--yes` `--help`.

Default names come from the browser and lane: workspace `brw-chrome-profile`
with profile `chrome-profile`, `brw-chromium-profile` with `chromium-profile`,
`brw-chrome-agent` with `chrome-agent` for direct CDP.

### Which browsers

Every Chromium-based browser works over both lanes: the daemon speaks the same
CDP to all of them and the extension loads in all of them. `--browser` takes:

| `--browser` | | Profile directory |
|---|---|---|
| `chrome` | Google Chrome | `~/Library/Application Support/Google/Chrome`, `~/.config/google-chrome` |
| `chromium` | Chromium | `~/Library/Application Support/Chromium`, `~/.config/chromium` |
| `edge` | Microsoft Edge | `~/Library/Application Support/Microsoft Edge`, `~/.config/microsoft-edge` |
| `brave` | Brave Browser | `~/Library/Application Support/BraveSoftware/Brave-Browser`, `~/.config/BraveSoftware/Brave-Browser` |
| `vivaldi` | Vivaldi | `~/Library/Application Support/Vivaldi`, `~/.config/vivaldi` |
| `opera` | Opera | `~/Library/Application Support/com.operasoftware.Opera`, `~/.config/opera` |
| `arc` | Arc | `~/Library/Application Support/Arc/User Data` (macOS only) |

With no `--browser`, setup picks the one that has actually been run — a browser
that is merely installed has no profile directory to bind to — and Chrome breaks
the tie.

A Chromium build that is not in that table still works; it just has to say where
its profiles live:

```sh
brwctl setup --browser comet --user-data-dir ~/Library/Application\ Support/Comet
```

Firefox and Safari are not supported. Neither speaks CDP, and Safari's
automation route hands you a clean window rather than your signed-in session,
which is the opposite of what brw is for.

### What setup cannot do for you

Loading an unpacked extension is a browser-UI action with no command-line
equivalent, so it stays manual until the Chrome Web Store listing is live:

1. Open `chrome://extensions`, turn on Developer mode (top right), click Load
   unpacked, and select `<app-dir>/extension`. The id must read
   `amocjcgddnoakjijfggdpnefdnboilpe`.
2. Open the extension's Options, read the browser-data disclosure, and click
   **Enable local browser control**. Nothing connects before you do.
3. Quit the browser completely and reopen it, so the App Nap default applies.
4. Restart your MCP client, or run `/mcp` in Claude Code, so it picks up the new
   server.

Between setup and step 1, `brwctl doctor` reports that the extension is not
installed and exits non-zero. That is the expected state, not a broken install.

### Claude Code's own Chrome integration

Claude Code ships a separate Chrome integration, enabled in `~/.claude.json`.
With both switched on the agent sees two browser tool sets and may drive the
wrong one. `brwctl doctor` emits the `claude_in_chrome_enabled` warning when it
finds it; run `/chrome` in Claude Code and turn it off.

## Two transports, different capabilities

`brw_identity` reports which one a namespace resolved to, and `brwctl doctor`
names it with the capabilities it implies.

| | Extension bridge | Direct CDP |
|---|---|---|
| Browser | The real signed-in Chromium browser you already use | A separate brw-owned instance |
| Existing logins | Yes | No, unless you point it at a cloned profile |
| Chrome tab groups | Yes | No |
| Incognito contexts (`brw_open_incognito`) | No | Yes |
| Cookies incl. HttpOnly (`brw_cookies`) | No | Yes |
| Deterministic download capture | No, uses the browser's download folder | Yes, staged in brw's cache |
| Headless | No | Yes |

`brwctl setup --transport direct-cdp` configures the second lane. Running both
against different profiles is supported: one `brwd` per profile, one MCP server
per daemon. Testing several signed-in roles at once wants a second browser
profile rather than a second transport.

## Runtime layout

macOS:

```text
~/Library/Application Support/brw/
  bin/
  config/browser-profiles.json
  extension/
  skills/brw/
  tests/
```

Linux:

```text
~/.local/share/brw/{bin,extension,skills,tests}
~/.local/bin/{brwd,brwctl,brwcheck,brw-devtools-mcp}   # symlinks
```

If `~/.local/bin` is not on your PATH, the installer prints the line to add.

## Build from source

```sh
make test
make build
```

Built binaries: `bin/brwd`, `bin/brwctl`, `bin/brwcheck`,
`bin/brw-devtools-mcp`. `make install` puts them in the user-local layout above.
`make package-tarballs` builds the archives the one-line installer consumes.

## Remote install

Copy the built binaries, `extension/`, `skills/`, `tests/`, and a profile policy
to the browser machine. Then generate MCP client config from the policy:

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

The extension is open source (AGPL-3.0). Its public key in
`extension/manifest.json` gives load-unpacked and self-hosted builds the stable
id `amocjcgddnoakjijfggdpnefdnboilpe`. That id is the daemon's
`profilepolicy.DefaultBridgeExtensionID`, so an unconfigured bridge already
trusts the real extension; you only set `bridge_extension_id` for a different
(re-signed) build. Verify that the draft Chrome Web Store item resolves to the
same id before publishing it.

The extension bridges the brw daemon to your real, signed-in browser over
`ws://127.0.0.1` after you review its disclosure and explicitly enable browser
control. It drives visible tabs via the Chrome debugger protocol, blocks
HttpOnly-cookie and bulk-storage CDP access, and does not access Chrome's
password store, passkey store, or profile files. Page-visible values can still
be handled when you ask your configured agent to do so. It is a normal visible
browser, with no stealth / CAPTCHA / MFA bypass.

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
5. In the Options page that opens, review the browser-data disclosure and click
   **Enable local browser control**.
6. Keep the extension enabled.

One-click Chrome Web Store install is being prepared, but is not live. The
current package, accurate disclosure answers, permission justifications, and
reviewer flow are in [`web-store-listing.md`](web-store-listing.md). Verify the
draft item id against the pinned id before publishing; if it differs, update the
profile policy's `bridge_extension_id` before switching users to it.

Set `bridge_extension_id` in the profile policy only when you ship your own
re-signed build with a different id; the default published id is built in.

For a local multi-profile installation, `make install-mac` also refreshes every
existing `~/Library/Application Support/brw/extension-*` payload without
overwriting its private `bridge-defaults.json`. Install the bundled operating
skill into `~/.claude/skills/brw`, `~/.agents/skills/brw` and
`~/.codex/skills/brw` with `make install-agent-skills`; this
copies instructions only, never the private recipe corpus.

## Keep the browser awake (macOS)

macOS App Nap can freeze a backgrounded browser's extension, which drops the
bridge until the browser is focused again. `brwctl setup` disables it for the
browser it configures. To do it by hand, or for a second browser:

```sh
defaults write org.chromium.Chromium NSAppSleepDisabled -bool YES
defaults write com.google.Chrome     NSAppSleepDisabled -bool YES
```

It applies at that browser's next full launch. See
[reliability.md](reliability.md) for the rest of the staying-connected story:
the extension's service-worker keepalive and an optional watchdog.

## macOS Downloads access

Direct-CDP profiles stage deterministic downloads in brw's private cache and do
not need access to the user's Downloads folder. Chrome's extension debugger API
does not expose browser-level download routing, so extension-backed profiles use
Chrome's configured download folder. Capturing those bytes as a brw artifact may
therefore require Files & Folders consent for the installed `brwd` binary under
System Settings > Privacy & Security > Files & Folders. The metadata-only
download event remains available without file access. brw limits an unanswered
OS permission request to one three-second attempt and makes concurrent/repeated
attempts fail fast, so a missing permission cannot create a retry-driven thread
or CPU storm.

## Verify the install

```sh
brwctl doctor
```

With no arguments it reads the policy `brwctl setup` wrote. Pass `--workspace`,
`--profile` and `--profile-policy` to check a specific profile, or `--app-dir`
for a Homebrew or relocated install.

`doctor` fails when app files are missing, the profile is not allowed by policy,
or the brw extension is not installed in the target browser profile. It reports
the resolved transport and its capabilities, and warns about a Claude Code
Chrome integration left switched on.
