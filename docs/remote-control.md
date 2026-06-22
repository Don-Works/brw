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

For installed browser profiles, treat the workspace binding as the authority.
Do not expose profile selection as an agent/tool argument. A long-lived daemon
should start with `--workspace` and its resolved policy profile; its HTTP
`/health` response includes that identity. Upstream MCP wrappers launched for a
workspace verify the upstream daemon identity before exposing tools, and fail
closed if the daemon is unlabelled or reports a different workspace/profile.

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

The `brw` extension connects locally to that daemon. Each Chrome profile can pin
its own loopback bridge URL and expected workspace/profile in the extension's
options page. On the agent machine,
generate a stdio MCP wrapper that reaches the browser machine over SSH and
talks to the daemon's loopback HTTP API:

```sh
brwctl remote-mcp-wrapper \
  --host browser-host \
  --user browser-user \
  --remote-brwd ~/.local/bin/brwd \
  --output ~/.local/bin/brw-browser-mcp
```

Point the MCP client at the generated wrapper.

The generated wrapper is hardened and resilient by default:

- **Auth/host trust**: `BatchMode=yes` (never blocks on a prompt that would wedge
  the MCP client), a dedicated `known_hosts` under the local `brw` app directory,
  and `StrictHostKeyChecking=accept-new`. Set `--strict-host-key-checking yes`
  when pre-pinning host keys. Pass `--identity-file ~/.ssh/id_brw` to offer only
  that key (`IdentitiesOnly=yes`), avoiding agent key churn / server lockout.
- **Clean stdio**: `RequestTTY=no` keeps the binary MCP stream intact even if the
  operator's `ssh_config` forces a TTY.
- **Connectivity resilience**: SSH keepalives
  (`--server-alive-interval 30 --server-alive-count-max 3`) drop a silently dead
  link (laptop sleep, NAT rebind, wifi switch) promptly instead of hanging the
  MCP client; `0` disables them. `--connection-attempts` retries the initial
  connect on flaky links.
- **Log hygiene**: SSH/remote stderr is appended to `--log`, rotated once it
  passes `--log-max-bytes` (default 5 MiB; `0` disables) so an unattended
  reconnect loop cannot fill the disk.
- **Performance**: `--compression` enables SSH compression — worth it for
  text-heavy payloads (snapshots) on slow links, skip it on fast links and for
  already-compressed screenshot payloads.

Every baked-in value is overridable at runtime via the matching `BRW_*` env var
(for example `BRW_SERVER_ALIVE_INTERVAL`), and `--ssh-option` appends any extra
`ssh -o` setting.

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
