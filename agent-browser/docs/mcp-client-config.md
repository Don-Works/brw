# MCP Client Config

## Local Stdio

```json
{
  "mcpServers": {
    "agent-browser": {
      "command": "/Users/max/github/revitt/brw/agent-browser/bin/agent-browserd",
      "args": ["--mcp", "--http", "off", "--profile", "agent-revitt"]
    }
  }
}
```

## Remote Browser On max-air Over SSH

This runs the MCP server on `max-air`, so the visible browser window and Chrome profile are also on `max-air`.

```json
{
  "mcpServers": {
    "agent-browser-max-air": {
      "command": "ssh",
      "args": [
        "max-air",
        "cd ~/agent-browser && ./agent-browserd-darwin-arm64 --mcp --http off --profile agent-revitt"
      ]
    }
  }
}
```

If you want to use the already-authenticated `revitt` default Chrome profile, the profile policy currently requires the extension bridge:

```json
{
  "mcpServers": {
    "agent-browser-max-air": {
      "command": "ssh",
      "args": [
        "max-air",
        "cd ~/agent-browser && ./agent-browserd-darwin-arm64 --mcp --http off --profile revitt"
      ]
    }
  }
}
```

That command will reject direct CDP until the extension bridge transport is implemented and installed, by design.

## SSH Prerequisite

The controlling machine's public key must be trusted on `max-air`.

```sh
ssh-copy-id max-air
```

On macOS without `ssh-copy-id`, append the controlling machine's public key to `~/.ssh/authorized_keys` on `max-air`, or enable Remote Login and add the key in your normal device setup process.
