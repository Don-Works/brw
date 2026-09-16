# brw behind a code-execution gateway

Read this when brw does NOT appear in your tool list as bare `brw_open` /
`brw_snapshot`, but as one namespace per browser profile invoked from inside a
code-execution tool:

```js
brw_chromium.brw_open({ url: "https://example.com" });
```

Tool names, arguments and return shapes are the ones in [../SKILL.md](../SKILL.md);
that file is the reference for what each call does. This file covers only what the
gateway changes: discovery, batching, and output handling.

Nothing here is specific to one gateway. Any harness that multiplexes MCP servers
into namespaces and runs JavaScript through a code-execution tool produces this
situation; brw only needs the calling convention below to be the same.

## Which situation am I in

| you see | read |
|---|---|
| `brw_open`, `brw_snapshot`, `brw_identity` in your tool list | ../SKILL.md, call them directly |
| a code-execution tool (`execute_code` / `call_tool`) and no `brw_*` tools | this file |

## One namespace per profile, and the set grows

Enumerate first; never assume a namespace exists or that there is only one:

```js
help();   // prints every namespace, e.g. brw_chromium (63 tools), brw_chromium_work (63 tools)
```

Namespaces bind at session start — a profile added later is invisible until the session
restarts. Map each `brw*` namespace to a concrete browser with `brw_identity`, which
needs no tab and no bridge:

```js
for (const ns of [brw_chromium, brw_chromium_work]) {
  const id = ns.brw_identity().identity;
  print(id.workspace, id.profile, id.profile_directory, id.transport, id.headless ? "headless" : "windowed");
}
```

Pick by what the user asked for. If two profiles could match, show the list and ask.

`identity.transport` decides capabilities exactly as in ../SKILL.md: `direct-cdp` has
incognito contexts and `brw_cookies`, `extension-bridge` has Chrome tab groups and
drives the human's signed-in Chrome. A gateway with one namespace per profile usually
has both lanes available — check, rather than telling the user a capability is missing.

When the gateway stamps an owner id onto the calls it makes, brw hashes it into the
tab-lease owner so leases survive a restart of the disposable proxy in front of the
daemon. `BRW_OWNER_ID` is that input; the older `MCPLEXER_BROWSER_SESSION_ID` is read
as a deprecated fallback by brw builds that still support it.

## Batch the flow, not the call

The round trip is the expensive part here. Put the whole flow in ONE `execute_code`
script; do not spend one gateway call per browser action.

```js
const ns = brw_chromium;
const r = ns.brw_open({ url: "https://app.example.test" });
const tab = String((r.tab || r).id);                        // ids may arrive numeric — stringify
const s = ns.brw_snapshot({ mode: "all", tab_id: tab });
const email = s.elements.find(e => e.role === "textbox" && /email/i.test(e.name)).ref;
const submit = s.elements.find(e => e.role === "button" && /continue|sign in/i.test(e.name)).ref;
ns.brw_fill({ ref: email, text: "a@example.com", tab_id: tab });
ns.brw_batch({ steps: [
  { action: "focus_tab", id: tab },
  { action: "click", ref: submit },
  { action: "wait", condition: "text:Signed in", timeout_ms: 8000 },
]});
const rd = ns.brw_read({ tab_id: tab, include: ["main"], max_chars: 2000 });
print(rd.title, rd.main.slice(0, 200));
ns.brw_close_tab({ tab_id: tab });
```

`brw_batch` still wins inside a script: one tab resolution, one observation at the end.
Two levels of batching compose — script for the flow, `brw_batch` for the steps.

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
