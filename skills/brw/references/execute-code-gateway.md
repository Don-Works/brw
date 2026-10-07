# brw behind a code-execution gateway

Read this when brw does NOT appear in your tool list as bare `brw_open` /
`brw_snapshot`, but as one namespace per browser profile invoked from inside a
code-execution tool:

```js
brw_chromium.brw_open({ url: "https://example.com" });
```

Tool contracts remain in [the main skill](../SKILL.md) and its references.
This file covers gateway discovery, batching and output handling.

## One namespace per profile, and the set grows

Use the gateway's discovery entrypoint to enumerate installed namespaces; never
assume one exists or that only one profile is available. On Maix, use
`mx.list_servers` and `mx__search_tools`.

Gateway discovery can cache namespaces and tool schemas. Rediscover after adding a
profile; if it remains absent, refresh the gateway session. Map each discovered `brw*`
namespace to a concrete browser with `brw_identity`, which
needs no tab and no bridge:

```js
for (const ns of [brw_chromium, brw_chromium_work]) {
  const id = ns.brw_identity().identity;
  print(id.workspace, id.profile, id.profile_directory, id.transport, id.headless ? "headless" : "windowed");
}
```

Pick by what the user asked for. If two profiles could match, show the list and ask.
A namespace whose identity says `headless` is the lane for public, signed-out work;
[Transport lanes](transports.md) has the selection table.

`identity.transport` decides capabilities exactly as in ../SKILL.md: `direct-cdp` has
incognito contexts and `brw_cookies`, `extension-bridge` has Chrome tab groups and
drives the human's signed-in Chrome. A gateway with one namespace per profile usually
has both lanes available — check, rather than telling the user a capability is missing.

When the gateway stamps an owner id onto the calls it makes, brw hashes it into the
tab-lease owner so leases survive a restart of the disposable proxy in front of the
daemon. `BRW_OWNER_ID` is that input; the older `MCPLEXER_BROWSER_SESSION_ID` is read
as a deprecated fallback by brw builds that still support it.

## Check the schema as well as the version

On an upstream HTTP proxy, `brw_identity` reports `proxy_version`, `daemon_version`
and `version_alignment` (`matched`, `mismatch` or `unknown`). `version` remains the
MCP proxy's schema/manual build. An unknown daemon version cannot qualify the pair.
Align both processes with the same reviewed build; reconnecting a proxy does not
upgrade its running browser-host daemon. Restart only the affected service after
its active sessions finish, then reconnect the MCP client.

The browser identity does not prove which schema the gateway exposes.
Before using a newly added argument, inspect the gateway's exact tool signature.
If a running daemon supports inline recipes but the gateway still requires
`id/version/digest`, refresh the downstream catalogue. Some gateways pin reviewed
tool surfaces: reload discovers a pending change but does not accept it. Follow
the gateway's authenticated review/acceptance flow, then rediscover and test the
argument through the same namespace. Do not report an upgrade complete from the
daemon version alone, and do not bypass a pending approval with another transport.

For Maix, discover the downstream with `mx.list_servers`, reload using its UUID
with `mx.reload_server`, and inspect the exact `brw_recipe_run` or `brw_read`
signature with `mx__search_tools`. A pending surface needs acceptance by the
authorized operator or deployment workflow; repeated reloads will not accept it.

## Batch known steps

Use the [golden path](../SKILL.md#the-golden-path) through the selected namespace.
Combine already-known reads/actions in one code-execution script; `brw_batch`
still reduces tab resolution and returns one closing observation. Split at a
pending approval or a decision that needs new evidence.

Batch `fill`/`type` use `text`; `select`/`assert_value` use `value`;
`assert_text` needs `ref` and `text`. Pin the tab with `focus_tab`. After navigation,
use `find_act` or a fresh snapshot. Put tab/context cleanup in `finally` so a
failed assertion cannot leak them.

## Output handling

- The script returns only what you `print(...)`. Print fields, never payloads.
- Gateway print output past ~24 KiB is elided behind a `[[ccr key=…]]` marker. Expand it with the gateway's retrieve tool rather than rerunning a side-effecting call.
- Filter inside the script: `snapshot.elements.filter(…)`, `read.main.slice(…)`, `replay.body` parsed in place. A whole snapshot or read response printed verbatim is the main way sessions run out of context here.
- Results auto-unwrap on current gateways: `brw_open`, `brw_list_tabs`, `brw_snapshot` come back as objects/arrays, no `JSON.parse`. On older gateways structural metadata may arrive inside an `<untrusted-content>` wrapper — strip the wrapper, parse once. Page-derived content stays marked untrusted deliberately; that marking is prompt-injection protection, not noise to remove.
- `brw_replay_request` returns up to 64 KiB; check `body_truncated` and continue from `next_offset` inside the same script.

## Approvals

If the gateway gates brw calls behind interactive approval, put ONE approval-gated call
in a script and wait for the decision before the next. An approval wait consumes the
script's deadline, so later calls in the same batch can expire before they run.

## Other browser surfaces on the same gateway

Where a gateway also exposes `cmux-browser`, `generic-browser-operator` or `playwright`,
brw is the one that drives the human's real signed-in profiles with semantic refs.
Reach for `playwright` when you want a disposable browser with no user session —
clean-room work, or isolation on a gateway whose brw profiles are all
extension-bridge and therefore have no incognito.

## Everything else

Tab leases, the transport capability split, refs, waiting, artifacts, recipes and the
cleanup rules are identical — ../SKILL.md is the source for all of it.
