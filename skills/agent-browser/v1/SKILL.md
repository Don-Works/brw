---
name: agent-browser
description: "Use when Codex needs to operate, test, configure, or debug the Go agent-browser daemon: a headed Chrome/Chromium CDP browser controller with HTTP and MCP tools, semantic DOM/accessibility snapshots, stable element refs, workspace-governed Chrome profile policy, and remote SSH/Tailscale operation."
---

# Agent Browser

Use this skill for the `agent-browser` repository and for workspaces that expose its MCP tools.

## Operating Rules

- Treat the browser as user-visible production UI. Prefer semantic `snapshot()` / `read()` state over screenshots.
- Use screenshots only for image-heavy content, charts, canvas, maps, or debugging.
- Do not bypass CAPTCHA, MFA, fraud checks, consent gates, or site controls.
- For private apps such as Gmail, ad platforms, banking, or finance pages, verify coarse auth state only unless the user explicitly asks for content. Ask the human to take over on login, passkey, MFA, CAPTCHA, or unexpected risk prompts.
- Keep profile access governed by `.mcplexer/config/browser-profiles.json`; do not improvise a Chrome profile path.

## Local Workflow

From the repo root:

```sh
cd agent-browser
make test
make build
AGENT_BROWSER_WORKSPACE=agent-browser \
AGENT_BROWSER_PROFILE=agent-revitt \
AGENT_BROWSER_PROFILE_POLICY=../.mcplexer/config/browser-profiles.json \
./bin/agent-browserd --http 127.0.0.1:17310
```

Smoke test:

```sh
curl -sS -X POST http://127.0.0.1:17310/api/browser/open \
  -H 'content-type: application/json' \
  -d '{"url":"https://example.com"}'
curl -sS http://127.0.0.1:17310/api/page/snapshot
```

Expected signal: a visible Chrome window opens, `listTabs()` shows the tab, and `snapshot()` returns semantic elements with refs like `e1` plus accessibility summary where available.

## Remote Workflow

Prefer stdio MCP over SSH so the browser and profile stay on the machine that owns them. Use Tailscale DNS for the host identity:

```sh
cd agent-browser
./bin/agent-browserctl mcp-config \
  --workspace agent-browser \
  --profile max-gmail \
  --transport max-air \
  --profile-policy ../.mcplexer/config/browser-profiles.json \
  --mode bridge
```

For max-air, the installed runtime lives at:

```text
~/Library/Application Support/agent-browser/
```

## Profile Policy

The workspace binding `agent-browser` defaults to `max-gmail` over `max-air`
and restricts this workspace to the profiles/transports declared in
`.mcplexer/config/browser-profiles.json`. Runtime MCP profile pinning should use
launcher env (`AGENT_BROWSER_WORKSPACE`, `AGENT_BROWSER_PROFILE`, and
`AGENT_BROWSER_PROFILE_POLICY`) so mcplexer can register the server as a normal,
visible stdio MCP downstream and later move those values into credential/env
injection without changing the daemon.

`agent-revitt` is the direct-CDP persistent non-default profile.

`max-gmail` points at Chrome `Profile 1` / `max.revitt@gmail.com` and is extension-bridge only.

`revitt-work` points at Chrome `Default` / `max@revitt.co` and is extension-bridge only.

Chrome 136+ blocks direct remote debugging against the default Chrome data dir. Chrome 137+ branded builds do not reliably support `--load-extension` for installed profiles. Do not edit Chrome profile JSON by hand. Use the stable bridge extension `hkomepfdcddgepbdalomhabiphokllkd`, installed once through Developer Mode or repeatably through a private Chrome Web Store channel or managed Chrome policy.

Run `AGENT_BROWSER_WORKSPACE=agent-browser AGENT_BROWSER_PROFILE=max-gmail agent-browserctl doctor` on the browser machine before authenticated tests.

## MCP Tools

The daemon exposes:

- `browser_open`
- `browser_list_tabs`
- `browser_focus_tab`
- `browser_close_tab`
- `browser_read`
- `browser_snapshot`
- `browser_click`
- `browser_type`
- `browser_select`
- `browser_press`
- `browser_scroll`
- `browser_screenshot`
- `browser_screenshot_element`
- `browser_wait_for`

Operate on refs returned by `browser_snapshot`, not CSS selectors. Example: `browser_type({ref:"e2", text:"..."})`, then `browser_click({ref:"e8"})`.

## Known Constraints

- SSH-launched Chrome on macOS may lack interactive Keychain/Bluetooth access. Passkeys or stored-password flows may require launching from the user session or the extension bridge.
- Full-fat installed Chrome is required. Do not switch to headless Chrome, Chrome-for-Testing-only workflows, or custom renderers unless explicitly requested for a separate test.
- The current optional UI is not implemented; rely on visible Chrome plus logs.
- Chrome DevTools MCP may be used only through `agent-browser-devtools-mcp`, which checks the same profile policy and fails closed when it cannot correlate the DevTools session to the requested workspace profile.
- The installed-profile extension bridge returns DOM semantic snapshots and readable content; direct CDP profiles are the path for full accessibility-tree enrichment.
