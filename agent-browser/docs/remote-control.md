# Remote Control Over SSH or Tailscale

Remote browser control is viable and fits the product model well, as long as the browser profile stays on the machine that owns it.

## Best Default: MCP Over SSH

The simplest secure path is running `agent-browserd --mcp` on the browser machine through SSH:

```sh
ssh mac-mini.local /path/to/agent-browserd --mcp --http off
```

Advantages:

- MCP remains stdio, so most harnesses can run it as a command.
- The browser opens visibly on the remote machine.
- The remote machine keeps its own Chrome profile, passkeys, downloads, and OAuth sessions.
- SSH handles authentication, encryption, audit logs, and port exposure.

Tradeoff: the human takeover happens at the remote machine's display, or through screen sharing.

## HTTP API Over SSH Tunnel

Run the daemon on the browser machine:

```sh
agent-browserd --http 127.0.0.1:17310
```

Tunnel from the agent machine:

```sh
ssh -L 17310:127.0.0.1:17310 mac-mini.local
```

Then call:

```sh
curl http://127.0.0.1:17310/api/page/snapshot
```

This is good for custom clients and debugging.

## Tailscale

Tailscale is viable if the daemon binds deliberately to the Tailscale interface or a locked-down localhost proxy on the browser machine.

Recommended posture:

- Bind to `127.0.0.1` by default.
- If exposing on Tailscale, add an auth layer before accepting remote HTTP.
- Prefer Tailscale SSH or Funnel-free private tailnet access.
- Log every action with caller identity once auth is added.

Do not expose raw unauthenticated browser control on a public interface.

## Repository Helper

From this repository, the helper script builds a macOS ARM binary, copies it and `.mcplexer` policy to the remote machine, and starts a visible browser there:

```sh
cd agent-browser
./scripts/remote-ssh.sh max-air
```

This requires SSH auth to `max-air` first.

## MCP Over Remote HTTP

For a future remote MCP transport, the daemon can expose MCP over streamable HTTP/SSE behind Tailscale. The tool semantics stay the same; only the transport changes.

Security requirements before enabling this:

- caller authentication
- per-tool authorization
- action audit log
- visible remote "agent is controlling this browser" status
- pause/kill switch on the browser machine

## Product Implication

Remote control is not a side feature. It is a strong answer to profile ownership:

> Keep the real browser profile on the trusted machine, then let harnesses connect to that machine through SSH, Tailscale, or MCP transport.
