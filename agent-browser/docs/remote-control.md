# Remote Control Over SSH And Tailscale

Remote browser control is viable and fits the product model well, as long as the browser profile stays on the machine that owns it.

## Default: MCP Stdio Over SSH

The runtime default is stdio MCP over SSH to the browser machine:

```sh
ssh maxrevitt@max-air \
  'AGENT_BROWSER_WORKSPACE=agent-browser AGENT_BROWSER_PROFILE=max-gmail AGENT_BROWSER_PROFILE_POLICY="$HOME/Library/Application Support/agent-browser/config/browser-profiles.json" "$HOME/Library/Application Support/agent-browser/bin/agent-browserd" --bridge --mcp --http off --bridge-addr 127.0.0.1:17311'
```

Advantages:

- MCP remains stdio, so most harnesses can run it as a command.
- The browser opens visibly on the remote machine.
- The remote machine keeps its own Chrome profile, passkeys, downloads, and OAuth sessions.
- SSH handles authentication, encryption, audit logs, and transport security.
- Tailscale DNS (`max-air`) gives a stable private name without exposing browser control on the LAN or public internet.

Tradeoff: the human takeover happens at the remote machine's display, or through screen sharing.

The workspace policy should name both pieces separately:

- `workspace`: for example `agent-browser`
- `profile`: for example `max-gmail`
- `transport`: for example `max-air`

The workspace binding determines the default profile/transport and the allow-list.
The profile determines which Chrome profile may be controlled. The transport only
determines where the MCP process runs.

For stdio clients, profile pinning is passed as launcher environment rather than
an HTTP header: `AGENT_BROWSER_WORKSPACE`, `AGENT_BROWSER_PROFILE`, and
`AGENT_BROWSER_PROFILE_POLICY`. The daemon treats flags and env the same way and
still fails closed if the profile is outside the workspace policy.

## HTTP API Over SSH Tunnel

HTTP remains a development/debug surface, not the primary remote transport.

Run the daemon on the browser machine:

```sh
agent-browserd --http 127.0.0.1:17310
```

Tunnel from the agent machine:

```sh
ssh -L 17310:127.0.0.1:17310 maxrevitt@max-air
```

Then call:

```sh
curl http://127.0.0.1:17310/api/page/snapshot
```

This is good for custom clients and debugging.

## Tailscale Transport

Tailscale should be used first as the SSH host identity:

```sh
ssh maxrevitt@max-air ...
```

For a future streamable-HTTP MCP transport, expose only an authenticated proxy on the tailnet. Do not bind raw browser-control HTTP directly to `100.x.y.z` or `0.0.0.0`.

Required posture:

- Bind to `127.0.0.1` by default.
- Prefer Tailscale SSH or private tailnet access.
- If exposing HTTP on Tailscale, require caller auth before accepting browser actions.
- Log every action with caller identity once auth is added.
- Preserve the same workspace profile authorization as stdio SSH.

Do not expose raw unauthenticated browser control on a public interface.

## Repeatable Install Shape

The repeatable browser-machine install location is:

```text
~/Library/Application Support/agent-browser/
  bin/agent-browserd
  config/browser-profiles.json
  extension/
  tests/
```

Runtime MCP clients should call the installed binary over SSH. Repository helper scripts are development/install conveniences only; they are not the control protocol.

## Installed-Profile Bridge

Current branded Chrome no longer supports reliable unpacked extension auto-loading through `--load-extension`. The bridge is therefore repeatable only when the extension is actually installed in the target Chrome profile:

- development: user enables Developer Mode and loads the unpacked extension once
- managed/private deployment: install a signed/private extension with a stable ID through Chrome Web Store private distribution or managed Chrome policy
- alternative: use a DevTools MCP wrapper when Chrome grants the relevant auto-connect permission

The daemon's `--bridge` mode is the MCP/HTTP surface once that extension is installed. It is not an extension installer.

## Future MCP Over Remote HTTP

For a future remote MCP transport, the daemon can expose MCP over streamable HTTP behind Tailscale. The tool semantics stay the same; only the transport changes.

Security requirements before enabling this:

- caller authentication
- per-tool authorization
- action audit log
- visible remote "agent is controlling this browser" status
- pause/kill switch on the browser machine

## Product Implication

Remote control is not a side feature. It is a strong answer to profile ownership:

> Keep the real browser profile on the trusted machine, then let harnesses connect to that machine through SSH, Tailscale, or MCP transport.
