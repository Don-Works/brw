# Auth Model

Carrying the user's existing Chrome auth is the hardest requirement.

## Chrome 136+ Constraint

Chrome changed remote debugging behavior in version 136. `--remote-debugging-port` and `--remote-debugging-pipe` are not honored when Chrome is pointed at the default Chrome data directory. Chrome now requires a non-default `--user-data-dir` for remote debugging.

That means a daemon cannot safely launch the user's ordinary default Chrome profile with CDP and inherit the cookies, sessions, extensions, and passkeys already present there.

## Supported Modes

### 1. Launch Mode

`agent-browserd` launches real installed Chrome/Chromium with:

- headed mode only
- `--remote-debugging-port`
- persistent non-default profile
- no `--disable-extensions`

Default profile path:

```text
~/.agent-browser/chrome-profile
```

The user signs in once in that visible profile. After that, OAuth sessions, cookies, extensions, downloads, and passkeys are reused by the daemon.

### 2. Attach Mode

`agent-browserd` can attach to an already-running CDP endpoint:

```sh
agent-browserd --remote http://127.0.0.1:9222
```

This is useful when another wrapper starts Chrome/Chromium with a non-default profile and remote debugging enabled.

### 3. Future Installed-Profile Bridge

For true carry-over from the user's already-authenticated default Chrome profile, the viable architecture is a Chrome extension installed in that profile. Chrome's `chrome.debugger` extension API is an alternate CDP transport. A native host or local WebSocket bridge can let the Go daemon request CDP commands through the extension.

This keeps:

- existing default-profile cookies and sessions
- existing passkeys and OAuth state
- existing financial-site auth
- visible, user-owned browser tabs

Tradeoffs:

- requires explicit extension install and debugger permission
- cannot silently bypass Chrome or website security controls
- some Chrome internal pages and restricted URLs remain inaccessible
- implementation must be auditable because `chrome.debugger` is powerful

## Non-Goals

- No cookie extraction from the default Chrome profile.
- No copying encrypted Chrome profile data.
- No CAPTCHA, MFA, fraud, or bot-check bypass logic.
- No headless-only fallback for authenticated flows.
