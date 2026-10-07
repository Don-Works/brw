# Local adversarial browser testbed

Run from the repository root:

```sh
go run ./cmd/brw-testbed -seed 7 -chaos 2 -max-events 256
```

The command prints JSON containing `url` and `frame_url`. Both listeners bind
literal loopback IPs on independent ports. Open `url` in a disposable browser
profile or an explicitly owned fixture tab. Stop the server with Ctrl-C.
`-http` and `-frame-http` accept fixed loopback addresses when a harness needs
stable ports. There are no external resources, accounts, model calls, or
package downloads at runtime.

Installed releases also provide `brw-testbed`; a checkout provides `task testbed`.
Use `?frames=none` for isolated permission scenarios; the default page retains all
three adversarial frames. Dedicated frame scenarios exercise that full page.

The page offers manual seeded steps and a bounded automatic stream. Changing
the delivery cadence does not change the logical sequence for the same seed,
chaos level, and explicit step sequence. Chaos 0 changes text; 1 also replaces
nodes and virtualized rows; 2 adds overlays, focus, frame reloads, article
revisions, and disconnects; 3 also emits dialog notifications. Native dialogs
open through the separately labeled alert, confirm, and prompt controls.

The hard reading fixture combines a corrected report, an incorrect sidebar,
hidden decoys, collapsed supplementary content, repeated headings, tables,
multiple columns, Unicode, and a revised report. Canvas and SVG targets share
similar colors and labels. The oracle supplies their geometry and the correct
target for a later vision evaluation; this testbed does not run a model.

The payment, password, card, and deletion forms change only synthetic fixture
state. The browser companion must obtain and verify **real brw approval-gate
decisions before performing these writes**. The fixture has no replacement
approval mechanism. A refused browser action should leave `form_state` and
`action_counts` unchanged. Sensitive inputs produce presence booleans only;
their values are never sent to the fixture server or included in the oracle.

## Machine contract

Every run has a new `run_id`. Mutating requests require the current run, and
form actions also require the current `document_epoch`. A stale captured node
cannot commit an action after a hydration step. Unknown JSON fields, trailing
JSON values, nonobject requests, oversized requests, and invalid cursors fail
before changing state. Seeds stay within JavaScript's exact integer range;
each run permits at most 64 simultaneous stream connections.

| Endpoint | Contract |
| --- | --- |
| `GET /api/state` | Run, DOM, reading, visual, form, upload, connection, cursor, and response-size oracle. |
| `POST /api/reset` | `{seed, chaos, max_events}` clears the run and returns its new oracle. |
| `POST /api/step` | `{run_id, count:1..64, kind?}` returns ordered events. The budget is 1–4096 events; exceeding it returns 409 without a partial step. |
| `POST /api/ack` | `{run_id, cursor, applied_cursor}` accepts only emitted cursors and `applied_cursor <= cursor`. Acknowledgements advance monotonically. |
| `POST /api/action` | `{run_id, document_epoch, kind, note?, sensitive_supplied?, target?}` records a synthetic effect. |
| `GET /events?run_id=…&cursor=…` | SSE hello, observable heartbeat, ordered events, and replay. `Last-Event-ID` overrides the query cursor. |
| `GET /ws?run_id=…&cursor=…` | WebSocket hello, heartbeat, ordered events, and replay. Send `{type:"ack", run_id, cursor, applied_cursor}` or `{type:"ping"}`. |
| `GET /frame?version=…` | Owned same-origin or second-origin frame with a labeled action. |
| `GET /download?name=fixture.txt` | Fixed body and oracle `download_sha256`. Names are bounded plain filenames. |
| `POST /upload` | One multipart `file` plus one `run_id`; maximum file size 1 MiB. Records filename, bytes, and SHA256, then discards the file. |
| `GET /fixture/status/{code}` | Synthetic status from 200 to 599. |
| `GET /fixture/redirect?to=…` | Relative URL or either owned fixture origin. Other destinations, including unrelated loopback services, are refused. |
| `GET /fixture/basic-auth` | Fixed public synthetic credentials `fixture` / `fixture`; credentials are not logged or returned. |
| `GET /api/cookies` | Sets a public Lax cookie and a HttpOnly Strict cookie with synthetic values. |

Explicit step kinds are `mutation`, `hydrate`, `virtualize`, `overlay`, `focus`,
`frame`, `reading`, `disconnect`, and `dialog`. Each event is
`{id, run_id, kind, view, replay?}`. A live `disconnect` event is delivered
before closing both streams. Replaying that event does not disconnect again.
There is no silent history truncation: the finite run retains every event,
and a slow connection closes so its consumer can replay from the last cursor.

The oracle includes `scenario_id`, `document_epoch`, `applied_cursor`,
`pending_actions`, `visible_item_ids`, `focus_name`, `form_state`, `last_upload`,
`emitted_event_ids`, `acknowledged_cursor`, `ws_connection_count`, and
`sse_connection_count`. `view` names the expected DOM state; the client’s
applied cursor and observed browser DOM establish whether delivery succeeded.
`reading` contains scoring facts and required phrases. `visual_targets`
contains image-relative bounds, shape, color, printed ID, and the correct
target. Stable accessible names and test IDs live in the embedded page.
`Hover fixture target` records `hover`/`hover-exit` counts; the named editable
controls record `focus` counts and the latest tracked `focus_name`. Both child
frames provide `Child fixture note`, whose value appears as `form_state.child_note`.
Frame-version checks reject effects from a replaced child document.

When the browser provides native WebMCP or brw's explicitly enabled runtime,
the page registers `fixture_read_report` (read-only verified facts) and
`fixture_delete_account` (destructive, approval-gated synthetic mutation).
If the runtime is enabled after load, use `Register available page tools`.
The mutation increments `action_counts.webmcp-mutation`; the companion must
prove a refused call leaves that counter unchanged before approving a write.

`measurements` counts successfully produced JSON/static response body bytes,
Unicode characters, and the ceiling of characters divided by four. The stream
counter sums SSE text blocks (including `id`, `data`, `event`, and `retry`
fields) and WebSocket JSON payloads. It excludes HTTP chunk encoding and
WebSocket frame headers. Stream-message counts include hello and heartbeat
writes; they are separate from logical event IDs. Individual body
sizes also appear in `X-Testbed-Body-Bytes` and
`X-Testbed-Chars4-Estimate` response headers. These are **size estimates, not
billed tokens**, and do not measure provider tokenization.
Counters and connection timing are observational; they are not part of the
seeded logical-event ground truth. An oracle response reports counters from
before its own body is produced.

## Validation

```sh
go test -race -p 1 ./internal/testbed ./cmd/brw-testbed
node --check internal/testbed/web/app.js
BRW_TESTBED_LIVE=1 go test -race -p 1 ./internal/testbed -run TestBrowserFixtureReconnectsAndReadsGroundTruth
task feature-check
```

The protocol tests compare seeds, enforce the finite budget, reject stale
runs/documents and malformed resets, verify live SSE and WebSocket disconnects,
replay and reconnect both streams, assert cursor acknowledgements, observe an
SSE heartbeat, prove sensitive values are absent, and verify download/upload,
cookies, auth, redirect containment, and byte/character measurements.
The opt-in browser test launches one disposable headless profile, reads the
report's ground truth, verifies hydration and both stream reconnections, and
compares rendered virtual rows with acknowledged cursors. It performs no form
writes. `BRW_TESTBED_SCREENSHOT=/absolute/path.png` optionally saves its page.

The feature companion starts a temporary headless browser profile and invokes the
actual MCP server over stdio pipes. It uses real site consent and an authenticated
operator approval inbox. Synthetic password, card, payment and deletion cases
exercise refusals before approval and compare approved effects with the oracle.
It creates no account, calls no model and sends no operating-system notification.

Its inventory comes from `mcp.ToolNames()` plus deferred `brw_skill` and `brw_tools`
discovery. Every tool must have exactly one scenario owner, an executed call and
an observable assertion; adding a tool without a scenario fails the catalogue
check. HTTP/CLI aliases use the same browser operations and retain their separate
transport tests. The JSON report records each tool, scenario, call/assertion counts
and outcome. Unsupported CDP tab grouping and deliberately unavailable page
notifications remain explicit outcomes rather than successful delivery claims.
With action confirmations enabled, a whole read-only recipe currently returns
`approval_split_required`; its MCP row is `policy_refused`. The companion also
checks that refusal causes no effect and verifies the same recipe's captured
evidence through the shared recipe service. That service check is not an MCP
success or an approval bypass.

| Scenario group | Observable checks |
| --- | --- |
| Navigation and reading | Owned tab identities, committed history/SPA destinations, corrected report facts, structured data, semantic refs and bounded snapshots. |
| Forms and assertions | Exact input values, trusted keys/clicks, selections, focus, sensitive withholding, pending approvals and failed assertions. |
| Pointer, frames and dialogs | Actual pointer/drag effects, same-origin and cross-origin child controls, replaced documents and native dialog outcomes. |
| Events and page tools | SSE/WebSocket reconnect and acknowledged cursors, page-event channels, WebMCP schema and approval-gated synthetic mutation. |
| Browser capabilities | Isolated contexts/auth, cookies/storage, emulation and reset, origin-scoped headers, init scripts, touch, profiles, routes and replay. |
| Artifacts and services | Actual image/PDF/profile/download contents and hashes, upload results, artifact lifecycle, recipes, plans, watchers, cancellation and discovery. |

`task feature-check` writes `dist/testbed/features.json` and always runs the whole
catalogue without cached results. For development only, `BRW_FEATURE_CASE` filters
the direct Go command by scenario substring; its output is partial coverage.
`BRW_FEATURE_REPORT=/absolute/path.json` selects a private report path for a direct
run. The companion runs in ordinary non-short Go tests as a correctness check;
latency measurements and provider billing remain separate.
