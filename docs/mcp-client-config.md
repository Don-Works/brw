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

For profile isolation, generate config from `--workspace` and let policy resolve
the profile and bridge addresses. The workspace route should expose one resolved
profile, not a user-selectable profile argument. When the generated command uses
`--upstream-http`, `brwd` checks `/health.identity` on the upstream daemon and
refuses to start if it is not the workspace/profile resolved from policy.

## Upstream Wrapper

If a long-lived daemon already owns the `brw` extension port on the browser
machine, generate an SSH stdio wrapper:

```sh
brwctl remote-mcp-wrapper \
  --host browser-host \
  --user browser-user \
  --remote-brwd ~/.local/bin/brwd \
  --output ~/.local/bin/brw-browser-mcp
```

Then configure the MCP client with the generated command:

```json
{
  "mcpServers": {
    "brw": {
      "command": "/path/to/brw-browser-mcp",
      "args": []
    }
  }
}
```

The wrapper speaks stdio MCP to the client and forwards to the local HTTP API on
the browser machine over SSH. It is a generated per-install artifact; source
control should contain the `brwctl` generator and docs, not host-specific
wrappers.

## DevTools MCP Companion

Use `brw-devtools-mcp` only when it can prove it is attached to the same
workspace-approved browser profile. If it cannot correlate the profile, it fails
closed.
