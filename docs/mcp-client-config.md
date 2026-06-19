# MCP Client Config

`brw` speaks stdio MCP. Local or remote, the client launches a command.

## Local Profile

```json
{
  "mcpServers": {
    "brw": {
      "command": "/path/to/brwd",
      "args": ["--mcp", "--http", "off"],
      "env": {
        "BRW_WORKSPACE": "brw",
        "BRW_PROFILE": "local-profile",
        "BRW_PROFILE_POLICY": "/path/to/browser-profiles.json"
      }
    }
  }
}
```

## Remote Profile Over SSH

Use `brwctl mcp-config` to generate the exact command:

```sh
brwctl mcp-config \
  --workspace brw \
  --profile work-profile \
  --transport remote \
  --profile-policy ~/.config/brw/browser-profiles.json \
  --mode bridge
```

The generated server still speaks stdio MCP to the client. Internally it starts
`brwd` on the browser machine through SSH.

## Upstream Wrapper

If a long-lived daemon already owns the bridge extension port on the browser
machine, generate an upstream wrapper instead:

```sh
brwctl mcp-config \
  --workspace brw \
  --profile work-profile \
  --transport remote \
  --profile-policy ~/.config/brw/browser-profiles.json \
  --mode upstream-http
```

The wrapper speaks stdio MCP to the client and forwards to the local HTTP API on
the browser machine.

## DevTools MCP Companion

Use `brw-devtools-mcp` only when it can prove it is attached to the same
workspace-approved browser profile. If it cannot correlate the profile, it fails
closed.
