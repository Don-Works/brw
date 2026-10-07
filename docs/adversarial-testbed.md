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
