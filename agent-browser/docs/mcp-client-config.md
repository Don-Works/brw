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
        "off",
        "--profile",
        "agent-revitt",
        "--profile-policy",
        "/Users/max/github/revitt/brw/.mcplexer/config/browser-profiles.json"
      ]
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
        "maxrevitt@max-air",
        "\"$HOME/Library/Application Support/agent-browser/bin/agent-browserd\" --bridge --mcp --http off --profile max-gmail --bridge-addr 127.0.0.1:17311 --profile-policy \"$HOME/Library/Application Support/agent-browser/config/browser-profiles.json\""
      ]
    }
  }
}
```

This controls the browser profile declared as `max-gmail` in the workspace
policy. The transport name is `max-air`; the profile name is `max-gmail`.
Do not merge those concepts.

## Installed-Profile Bridge Requirement

For installed Chrome profiles, direct CDP is not the auth-preserving path.
Use bridge mode only after the bridge extension is installed in that Chrome
profile.

The bridge extension connects locally on `max-air` to
`ws://127.0.0.1:17311/extension`. Browser-control traffic does not need a
network listener.

## Chrome DevTools MCP Companion

Chrome DevTools MCP can sit beside `agent-browser`, but should be launched
through a profile-correlating wrapper. The wrapper must read the same workspace
profile policy and either:

- pass the exact direct-CDP endpoint for an `agent-browserd` direct-CDP session
- use Chrome's approved auto-connect flow for the same installed profile
- fail closed when it cannot prove the DevTools session belongs to the requested profile

Do not configure Chrome DevTools MCP manually against an arbitrary open Chrome.
That breaks the workspace profile boundary.
