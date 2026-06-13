# MCP Client Config

`agent-browser` is a stdio MCP server. Remote control should still use stdio;
SSH is the transport that places the process on the browser machine.

## Local Non-Default Profile

Use this for direct CDP development profiles.

```json
{
  "mcpServers": {
    "agent-browser": {
      "command": "/Users/max/github/revitt/brw/agent-browser/bin/agent-browserd",
      "args": [
        "--mcp",
        "--http",
        "off"
      ],
      "env": {
        "AGENT_BROWSER_WORKSPACE": "agent-browser",
        "AGENT_BROWSER_PROFILE": "agent-revitt",
        "AGENT_BROWSER_PROFILE_POLICY": "/Users/max/github/revitt/brw/.mcplexer/config/browser-profiles.json"
      }
    }
  }
}
```

## max-air Installed Profile Over Tailscale SSH

Use the Tailscale DNS name for the host, but keep MCP as stdio over SSH.
For `max-gmail`, use bridge mode because it is an installed Chrome profile.

```json
{
  "mcpServers": {
    "agent-browser-max-air": {
      "command": "ssh",
      "args": [
        "-o",
        "UserKnownHostsFile=/Users/max/Library/Application Support/agent-browser/ssh/known_hosts",
        "-o",
        "StrictHostKeyChecking=accept-new",
        "maxrevitt@max-air",
        "AGENT_BROWSER_WORKSPACE='agent-browser' AGENT_BROWSER_PROFILE='max-gmail' AGENT_BROWSER_PROFILE_POLICY=\"$HOME/Library/Application Support/agent-browser/config/browser-profiles.json\" \"$HOME/Library/Application Support/agent-browser/bin/agent-browserd\" '--bridge' '--mcp' '--http' 'off' '--bridge-addr' '127.0.0.1:17311'"
      ]
    }
  }
}
```

This controls the browser profile declared as `max-gmail` in the workspace
policy. The transport name is `max-air`; the profile name is `max-gmail`.
Do not merge those concepts.

The workspace binding `agent-browser` supplies the default profile/transport
and also restricts which explicit profile names are accepted. Stdio MCP has no
HTTP headers, so the daemon accepts the MCP launcher equivalent:
`AGENT_BROWSER_WORKSPACE`, `AGENT_BROWSER_PROFILE`, and
`AGENT_BROWSER_PROFILE_POLICY`. A mcplexer credential or downstream config can
set those env vars to pin the selected Chrome profile while the daemon still
enforces the workspace allow-list.

## Installed-Profile Bridge Requirement

For installed Chrome profiles, direct CDP is not the auth-preserving path.
Use bridge mode only after the bridge extension is installed in that Chrome
profile.

The bridge extension connects locally on `max-air` to
`ws://127.0.0.1:17311/extension`. Browser-control traffic does not need a
network listener.

If another long-lived profile daemon already owns the extension bridge port,
run stdio MCP as an upstream wrapper instead of starting a second bridge owner:

```bash
agent-browserctl mcp-config \
  --workspace agent-browser \
  --profile max-gmail \
  --transport max-air \
  --mode upstream-http \
  --profile-policy ../.mcplexer/config/browser-profiles.json
```

That generated MCP server still speaks stdio to the client; internally it
forwards to the profile daemon's local HTTP API on the browser machine.

## Chrome DevTools MCP Companion

Chrome DevTools MCP can sit beside `agent-browser`, but should be launched
through a profile-correlating wrapper. The wrapper must read the same workspace
profile policy and either:

- pass the exact direct-CDP endpoint for an `agent-browserd` direct-CDP session
- use Chrome's approved auto-connect flow for the same installed profile
- fail closed when it cannot prove the DevTools session belongs to the requested profile

Do not configure Chrome DevTools MCP manually against an arbitrary open Chrome.
That breaks the workspace profile boundary.
