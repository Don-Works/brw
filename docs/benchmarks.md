# Benchmarks

Every number on this page is produced by something in this repository, against
fixtures in this repository, on a machine whose identity is printed next to the
result.

A head-to-head against Claude-in-Chrome used to be described here. It was run
before release, its transcripts were never published, and nothing in this
repository reproduces it. It has been removed rather than restated: see
[removed claims](#removed-claims) for exactly what went and why.

What remains about the design, with no measurement attached to it: `brw` returns
semantic observations from actions, so an agent acting from refs does not
re-interpret a screenshot per step; an installed browser profile is what carries
a signed-in session, which is what the extension bridge and the SSH runtime
exist for; and the MCP catalogue is re-sent on every request, so its size is a
per-turn cost rather than a one-off — `--mcp-tools core` and `--mcp-tools
minimal` trade surface for it.

## The fixture benchmark harness

`task bench` drives five flows against `tests/fixtures`, served over a loopback
HTTP origin it starts itself, in a headless Chrome on a throwaway profile. It
needs no daemon, no network and no account. Per command it records wall time,
the CDP messages sent and received, the bytes those cost on the transport, and
the size of the MCP tool result an agent gets back; per run it records the
harness process's and the browser tree's CPU and peak RSS.

"No network" is enforced rather than asked for. Chrome launches with
`--host-resolver-rules="MAP * ~NOTFOUND, EXCLUDE 127.0.0.1"`, so the fixture
origin's own address is the only thing that resolves at all and everything else
fails by construction. Chrome's `--disable-background-networking` family is set
too and is not sufficient on its own: with all of it set, Chrome 153 still
completed GCM registration round trips to Google on every run, inside the
window being measured.

```sh
task bench                                   # summary plus dist/bench/record.json
go run ./cmd/brwcheck --bench --repo-root .  # the same run, no record written
go run ./cmd/brwcheck --bench --bench-only forms --bench-json --repo-root .
```

It is not part of `go test ./...`, `task test` or `task check`, on purpose: a
timing that fails because CI was busy is a gate nobody can act on.

The CDP counts and byte counts are measured, not estimated. A counting relay
sits between brw and Chrome's debugging port and walks the websocket frames, so
a row reading 60 sent is 60 CDP messages that crossed the socket. The counters
are sampled around each call, so an event arriving while no call is in flight is
attributed to the next command rather than to the one that caused it.

### First recorded run

```
darwin/arm64 Apple M4 Max x16 | Chrome/153.0.8010.37 | brw 0.13.5-73-g4601537 | go1.26.6 | fixtures c93446d33c8a
captured 2026-09-15T10:05:39Z, 32 commands, 7978 ms wall
```

| Flow | Commands | Wall ms | CDP sent | CDP received | Bytes sent | Bytes received | Observation bytes | ~tokens |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| forms | 11 | 1,214.2 | 60 | 116 | 362,407 | 83,198 | 20,582 | 5,142 |
| shop | 11 | 1,749.7 | 59 | 109 | 334,710 | 58,529 | 13,486 | 3,370 |
| dynamic | 6 | 1,525.9 | 40 | 88 | 216,201 | 35,552 | 6,278 | 1,568 |
| structured | 4 | 1,377.3 | 28 | 75 | 97,709 | 24,243 | 3,778 | 944 |
| **all flows** | **32** | **5,867.2** | **187** | **388** | **1,011,027** | **201,522** | **44,124** | **11,024** |

System cost of that run: the harness process peaked at 24.4 MB RSS and used
67 ms user + 79 ms sys; the browser tree's largest process peaked at 260.1 MB
and the tree used 2,027 ms user + 1,192 ms sys. The browser figure is a
high-water mark for the largest single browser process, which is what the kernel
records — not a sum across Chrome's processes.

Read the wall column as an upper bound. This machine was compiling and driving
other browsers throughout, at a load average around 65 on its 16 cores. The
counted columns do not move with that, and are the ones to compare.

`dynamic/wait_controls` is the page waiting rather than brw working: the
fixture's `setTimeout` is 800 ms and the row cannot be faster than it.

`structured/read` is brw working. `structured-product.html` has no timer; its
visible body text is 38 characters, under `readMinMainLen` 50, so `brw_read`
treats the page as an unpopulated shell and waits out `readSettleCapMS` — the
800 ms content-settle cap in `internal/readability/scripts.go` — before giving
up. Read that row as the cost of brw's own settle cap on a page with almost no
text, not as the page being slow. The flow totals include both.

Bytes sent exceeds bytes received on every flow because brw sends in-page
scripts and receives semantic results: a `brw_fill` carries roughly 44 KB of
script to the browser and gets back roughly 6 KB. That is the shape of the
design, not a measurement of a page.

What moves between runs and what does not, across 6 runs on this machine while
it was busy with other work: total wall time ranged 5,867–9,518 ms. Every one of
them sent 187 commands and 1,011,027 bytes, to the byte. Messages received
ranged 380–388 — Chrome's event stream is asynchronous, so an event that
arrives between two calls is attributed to the later one. Observation bytes
spanned 10 bytes in all, because each result carries its own `duration_ms`. Read
the send counts as stable, the receive counts as approximate, and the wall times
as a machine with other work on it. Stable is not invariant: in an earlier set of
eight runs the slowest sent 186 and 1,010,910.

Compare two records by their `environment` block first: `os`, `arch`,
`cpu_model`, `cpus`, `browser`, `headless` and `fixture_digest` all have to
match, and the record carries all seven so a mismatch is visible rather than
assumed.

The observation columns are the MCP tool result an agent receives, envelope
included: MCP carries the payload twice, once as a JSON string inside
`content[0].text` with every quote escaped and again as `structuredContent`, and
both are counted. An earlier version of this table weighed the internal Go value
once, which was roughly half of what a turn costs under a heading that said
otherwise; records from it carry schema `brw.bench/v1` and must not be compared
with these.

Token figures use the same 4-chars-per-token estimator as
`scripts/measure-tool-catalogue.py`. It compares arms; it is not a tokenizer.

### Reading a page with and without a tab

The `read_paths` flow reads `content.html` both ways: `brw_open` then
`brw_read` in a tab, and `brw_read_url`, which fetches and extracts in the
daemon with no browser. The first recorded table above predates the flow and
does not include it.

```
darwin/arm64 Apple M4 Max x16 | Chrome/154.0.8037.58 | brw dev | go1.26.6 | fixtures 9ca97ef688ad
go run ./cmd/brwcheck --bench --bench-only read_paths --repo-root .   (3 runs, 2026-09-25)
```

| Path | Commands | Wall ms | CDP sent | CDP received | Bytes sent | Observation bytes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `brw_open` + `brw_read` | 2 | 28.4–49.0 | 30 | 70–71 | 94,549 | 4,248 |
| `brw_read_url` | 1 | 0.9–1.6 | 0 | 0 | 0 | 3,394 |

The `brw_read_url` row includes its discovery probes (`/llms.txt`, the page's
`.md` variant and three `/.well-known/` documents), which the loopback fixture
origin answers with 404. Against a remote origin both paths add network time;
the tab path also waits for the page's own subresources and settle, which the
fixture does not exercise. Both rows exclude the MCP layer: the `brw_open` row
does not include the one evaluate the MCP handler adds to report `page_tools`
and `agent_surfaces`.

## The agent evaluations

`task agent-eval` runs four tasks against the same fixtures and grades each one
on the state the harness reads out of the page after the run stops, not on what
the run says it did. The four shapes: submit a form with the right values,
extract a fact that only exists after opening something, complete a four-step
flow, and report a failure instead of doing something adjacent and calling it
done.

```sh
task agent-eval          # four tasks, deterministic end-state grading
task agent-eval-verify   # the same four run honestly AND sabotaged
```

It runs with no API key, and on the same resolver-blocked browser as the
benchmark. `--eval-judge` adds an LLM judge over the deterministic check, shown
the task, the criteria and the observed end state — never what the run claimed. The judge can fail a run the end-state check passed; it cannot pass
one the check failed.

`agent-eval-verify` is how the evaluation is shown to be capable of failing at
all: it runs every task a second time with the decisive act removed and the
success claim left in, and exits non-zero unless every one of those is caught.
On the machine fingerprinted above, 8 of 8 graded as expected — four honest
passes, four sabotaged failures, each naming what the page did not show:

| Task | Sabotage | Caught by |
| --- | --- | --- |
| form-submit | fills everything, never submits | status region empty |
| extract-price | right price, product never opened | product panel closed |
| basket-flow | never chooses a size | basket count 0, basket empty |
| report-missing-control | submits the form and claims the deletion | claimed success, and the status region shows a submission |

## Removed claims

These were on this page and are gone. None of them is reproducible from this
repository, and each was a claim about `brw` that a reader had no way to check.

- "`brw` needed fewer turns" than Claude-in-Chrome.
- "`brw` used fewer tokens" than Claude-in-Chrome.
- "`brw` took less wall time" than Claude-in-Chrome.
- "`brw` had lower estimated cost" than Claude-in-Chrome.
- "`brw` needed fewer screenshots because actions return semantic observations."
- "Claude-in-Chrome retained an auth advantage when it could use an already-open
  installed Chrome profile."

The harness above measures `brw` against fixtures. It does not compare `brw`
with any other tool, and nothing here should be read as one.

## Reproducible local measurements

The repository now includes synthetic and real-browser regression probes that
contain no private site data. On an Apple M4 Max in September 2026:

| Probe | Result |
| --- | --- |
| Event settling around a synchronous DOM reaction | pre-armed median 66.3 ms vs post-armed 152.9 ms; **2.31x faster** |
| Settle when the page produces no reaction | pre-armed median 102.4 ms vs post-armed 103.6 ms; **no measured worst-case penalty** |
| Browser-host bounded read across the HTTP proxy | 20,226 bytes vs 1,048,733 bytes; **51.9x less transfer** |
| 1,310,720-byte artifact result | 276-byte metadata vs 1,747,628-byte inline base64; **6,332.0x smaller** |
| Extension logical-response ceiling | 64 MiB in bounded chunks vs the former 4 MiB single-frame ceiling; **16x more serialized capacity without raising the per-frame cap** |
| Extension close of a `beforeunload`-guarded page | 43 ms vs the previous 20,000+ ms timeout; **at least 465x faster, and now succeeds** |
| Maximum 120 s element-event wait | about 483 semantic checks vs 1,200 fixed-100 ms checks; **59.8% fewer browser scans** |
| Download-complete wait, event subscription vs the 50 ms registry poll it replaced | median 13.5 µs vs 32.2 ms from the registry recording completion to the wait returning; **~2,400x lower wait latency** |
| Repeat readiness check on an already-loaded tab, event subscription vs the in-page readyState probe | median 459 ns vs 551 µs; **~1,200x lower wait latency, and no CDP round trip** |
| In-memory 100,000-entry catalogue, rare intent | 740 ns indexed vs 5.64 ms linear control; **~7,631x faster** |
| Local 100,000-entry catalogue, common intent top 50 | 5.78 ms, 14,240 B and 109 allocations |
| Post-action observation over ten separate action-tool calls, `observe:"minimal"` vs the `full` default | ~2,676 B vs ~4,951 B of result JSON; **46% fewer bytes** |
| The same ten calls at `observe:"none"` | ~1,184 B vs ~4,951 B; **76% fewer bytes**, and every call still reports its outcome |
| The same ten steps as one `brw_batch` (one closing observation), against those ten calls | ~743 B vs ~4,951 B; **85% fewer bytes**, and ~622 B at `observe:"none"` |
| One compositor frame against one screenshot capture of the same page, over a 20s capture at 5fps | 2,386 B vs 2,771 B per frame; **14% fewer bytes per frame** |

The two wait rows measure the same question asked two ways against the same
state, so each ratio is the cost of asking rather than the cost of the answer.
Read them for exactly what they time. The download row starts when the registry
records the completion, not when Chrome finishes writing the file: the
completion is injected in-process, so the Chrome to websocket to chromedp
dispatch hop that a real `Browser.downloadProgress` pays is outside both arms.
The readiness row is a repeat check against a tab that has already loaded — a
mutex-guarded map read against a CDP round trip — not the delivery of a load
event. The in-page arm varies between roughly 0.5 ms and 3 ms run to run, and
the figure above is from the fastest run of four, so the ratio is the
conservative end. The polled control is not a strawman: it is the cadence the
extension transport still runs, because it holds no debugger attachment to
subscribe with. See [waiting.md](waiting.md) for which mechanism answers which
condition on which transport.

The screencast row is per frame because the window total is not a property of
the transport: the total is bytes per frame times the frame rate each side
managed, and the loop's rate is set by how fast the host can capture. The same
20s window fits 100 captures on this machine and 42 on a Linux CI runner, which
puts the total saving anywhere between 5.8x and 15% while the per-frame figure
stays put — 2,386 B against 2,771 B here, 2,578 B against 2,966 B there. Round
trips are not the measure either: `Page.screencastFrameAck` costs one per frame
exactly as the loop costs one capture call per frame, plus start and stop, so
that count reads 22 against 100 here and 43 against 42 on the runner.

These are machine-local samples, not universal latency promises. Reproduce them
with:

```sh
go test -count=1 -v ./internal/browser -run TestObserveLevelsShrinkATenStepFlow
go test -count=1 -v ./internal/browser -run TestPrearmedSettleIsMateriallyFaster
go test -count=1 -v ./internal/browser -run 'TestEventWaitLatencyBeatsThePollingFallback|TestLoadWaitLatencyBeatsTheInPageProbe'
go test -count=1 -v ./internal/browser -run TestScreencastMovesFewerBytesPerFrameThanTheScreenshotLoop
go test -count=1 -v ./internal/httpclient -run TestReadWindowIsAppliedOnBrowserHost
go test -count=1 -v ./internal/artifact -run TestArtifactMetadataSizeDoesNotScaleWithPayload
go test -run '^$' -bench '^BenchmarkCatalogSearch100K$' -benchtime=100x -benchmem ./internal/recipe
python3 scripts/measure-tool-catalogue.py
```

The 100,000-entry probe measures `Catalog`, not the filesystem-backed
`DirectoryProvider`. The latter performs an O(N) metadata fingerprint before
each search/fetch so local installs become visible immediately. Use the local
provider for a modest private collection and the HTTPS provider for a large
database/vector-backed bank; do not extrapolate the nanosecond catalogue lookup
to a 100,000-file directory.

Adaptive event polling checks again at 25, 50, 100, and 200 ms, then caps at
250 ms. That improves the first recheck from 100 ms to 25 ms and sharply reduces
idle DOM scans on large inbox/message pages; the explicit tradeoff is up to
250 ms steady-state detection latency instead of 100 ms.

## Observation size

`observe` on the action tools, `brw_batch` and `brw_plan` chooses how much of the
post-action observation is reported. The measurement fills five fields and
clicks five buttons on the same real headless Chrome page (a twenty-control
form) two ways: as ten separate action-tool calls (five `brw_fill`, five
`brw_click_text`), which observe once per call, and as one ten-step
`brw_batch` of `find_act` steps, which observes once for the whole call. Each
arm's result JSON is serialized and counted.

```
ten separate calls, observe=full (the default on every call)   4951 bytes  ~1238 tokens  100.0%
  sequence default (minimal for calls 1-9, full for call 10)   2907 bytes  ~ 727 tokens   58.7%
  observe=minimal on every call                                2676 bytes  ~ 669 tokens   54.0%
  observe=none on every call                                    1184 bytes  ~ 296 tokens   23.9%
the same ten steps as one brw_batch, observe=full               743 bytes  ~ 186 tokens   15.0%
  observe=none                                                  622 bytes  ~ 156 tokens   12.6%
```

The two arms answer different questions. Batching is the larger saving by far,
because nine of the ten observations stop existing rather than getting smaller;
`observe` then trims what is left. On `brw_batch` there is only the one closing
observation to trim, and it carries no element list, so `minimal` there is the
same as `full` — that is why `brw_batch` advertises its own wording for the
parameter instead of the shared one.

Token figures use the same 4-chars-per-token estimator as
`scripts/measure-tool-catalogue.py`; they compare arms, they are not a
tokenizer. Byte counts jitter by a few bytes between runs because each result
carries its own `duration_ms`. The absolute numbers are a property of this
fixture — a denser page has a larger frontier element list and a larger saving
— so read the ratios.

`none` is not free of meaning: every level still reports `ok`, `message`,
`warning` and `changed_state`, so a flow can always tell whether a step worked.
A navigation also keeps `url` at every level — `brw_navigate`, `brw_navigate_to`
and a `navigate_to` step inside `brw_plan` — because the message names the url
that was REQUESTED and the caller would otherwise be left holding a destination
brw never verified. And no level skips the observation itself. The post-action
read is where the navigation policy re-checks the committed destination, so
`observe` buys tokens and never latency; a level that skipped the read would be
an opt-out from a guard.

A recipe run is already at the floor: `recipe.StepResult` reports `{id, status,
attempts, duration_ms}` and no observation at all, so there is nothing for a
level to trim.

## Tool catalogue

The measured MCP catalogues are 94 tools / ~34.9k tokens for `all`, 26 / ~10.5k
for `core`, 13 / ~5.8k for `minimal`, and 14 / ~6.0k initially for the default
`auto` profile — the same figures README.md and docs/agent-guide.md quote, from
`scripts/measure-tool-catalogue.py`, which measures a direct-CDP daemon. Thus
the default starts about 80% smaller than advertising every tool, while every
tool remains directly callable and discoverable through `brw_tools`.

The `observe` parameter and the locate-and-act half of `brw_find` cost ~2.6k
tokens of `all` and ~1.3k of `minimal`, measured against the catalogue that
introduced them, because a parameter repeated across seventeen tools is paid for
on every turn whether or not it is used. Those two are deltas, not a second pair
of totals: subtracting them from the figures above to quote a catalogue size
without them is what left this section naming two different sizes for `all`.
That is the trade: a fixed per-turn catalogue cost against a per-action saving
that scales with the length of the flow.

A daemon whose identity names a transport advertises fewer than the 97 tools
the catalogue holds. The three extension-only tab-group tools drop on every CDP
lane, leaving the 94 above on direct CDP. `--remote` drops
`brw_set_download_path` as well (93): brw did not start that browser, so it will
not retarget its downloads. The Chrome opt-in lane drops `brw_state` on top of
that (92), because it drives the browser its user is signed into. A
plugin-supplied off-host browser drops `brw_downloads`, `brw_upload_file` and
`brw_clipboard` as well as `brw_state` (89), because all four name something on
this machine and that browser is on another: three resolve a path or the
clipboard on the host the browser runs on, and the fourth reads this host's
session-snapshot store. Twelve more drop on the extension
bridge — incognito, contexts and cookies, the seven page-environment overrides,
clipboard, the two held-key tools, pushState and session snapshots — leaving 78.
Only `all` is narrowed this way; `core`, `minimal` and `auto` advertise the same
tools on every transport.

The figures in the rest of this paragraph are historical: they measure the
release that introduced the `auto` default, not this tree, and the catalogue has
grown since. At that point the CLI default was `all`: 55 tools and about
12,109 tokens. The new default `auto` started at 13 tools and about 4,060 tokens,
which was **66.5% less catalogue context than the old default**. The opt-in full
surface grew by seven tools and about 11.4% (12,109 to 13,488 tokens) because it
now describes the recipe and artifact APIs; gateway installations continue to
index that full surface once and expose individual tools through semantic tool
search. Keep gateway downstream routes on `all`: a pinned catalogue does not
adopt brw's session-local `list_changed` growth after `brw_tools`, while its own
six-tool code-mode façade already keeps the downstream definitions out of model
context. `auto` is for direct MCP clients that honor dynamic tool-list changes.
