# Remote Control

`brw` is designed to run where the browser profile lives. The agent connects to
that machine over SSH; Chrome stays visible there.

## Default Shape

```text
agent harness -> stdio MCP -> ssh -> brwd -> visible Chrome
```

Why this works:

- No browser-control port needs to be exposed publicly.
- SSH provides authentication, encryption, logging, and host policy.
- Cookies, passkeys, downloads, and human takeover stay on the browser machine.
- The same profile policy gates local and remote use.

## Generate MCP Config

```sh
brwctl mcp-config \
  --workspace brw \
  --profile work-profile \
  --transport remote \
  --profile-policy ~/.config/brw/browser-profiles.json \
  --mode bridge
```

Policy separates the concerns:

- `workspace`: caller boundary and allow-list
- `profile`: Chrome profile allowed for that workspace
- `transport`: where `brwd` runs

Chrome tab groups can be used as visible run workspaces on the extension-bridge
transport. An agent can inspect `brw_list_tab_groups`, choose the next
client-side run name such as `brw-1`, pass that unique `group` to
`brw_open`, and then pass `group_id` on later opens or regrouping calls so
each automation run keeps its tabs together. This is only tab strip
organization: ungrouped tabs remain readable/targetable, and tab groups do not
isolate cookies, storage, downloads, or authorization. Use separate profiles or
incognito contexts for isolation.

## Installed Chrome Profile

For an already-authenticated Chrome profile, run a long-lived bridge daemon on
the browser machine:

```sh
brwd --bridge --http 127.0.0.1:17310 --bridge-addr 127.0.0.1:17311
```

The `brw` extension connects locally to that daemon. On the agent machine,
generate a stdio MCP wrapper that reaches the browser machine over SSH and
talks to the daemon's loopback HTTP API:

```sh
brwctl remote-mcp-wrapper \
  --host max-air \
  --user maxrevitt \
  --remote-brwd ~/.local/bin/brwd \
  --output ~/.local/bin/brw-max-air-mcp
```

Point the MCP client at the generated wrapper. The wrapper uses a dedicated
`known_hosts` file under the local `brw` app directory and defaults to
`StrictHostKeyChecking=accept-new`; set `--strict-host-key-checking yes` when
pre-pinning host keys.

## HTTP Tunnel

HTTP is useful for custom clients and debugging:

```sh
brwd --http 127.0.0.1:17310
ssh -L 17310:127.0.0.1:17310 <ssh-target>
curl http://127.0.0.1:17310/api/page/snapshot
```

Do not expose unauthenticated browser-control HTTP on a public interface.

## Required Posture

- Bind browser-control HTTP to loopback by default.
- Prefer stdio MCP over SSH for remote control.
- Use a private network or authenticated proxy before exposing HTTP.
- Preserve workspace profile authorization in every transport.
- Keep a visible browser and human takeover path on the browser machine.
