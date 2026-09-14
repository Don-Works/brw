# Benchmarks

Private pre-release head-to-head runs compared `brw` with Claude-in-Chrome on
semantic browser tasks. The raw transcripts are not published, so these results
are directional rather than independently reproducible from this repository.

Observed signal:

- `brw` needed fewer turns.
- `brw` used fewer tokens.
- `brw` took less wall time.
- `brw` had lower estimated cost.
- `brw` needed fewer screenshots because actions return semantic observations.
- Claude-in-Chrome retained an auth advantage when it could use an already-open
  installed Chrome profile.

Interpretation:

- For normal DOM-heavy web tasks, refs plus action observations beat repeated
  screenshot interpretation.
- For auth-heavy tasks, installed-profile access matters. `brw` addresses that
  with the Chrome extension bridge and SSH runtime.
- The full MCP tool surface is intentionally broad. Use `--mcp-tools core` or
  `--mcp-tools minimal` for a lean advertised tool set; the catalogue is re-sent
  on every request, so its size is a per-turn cost rather than a one-off.

Raw transcripts are not shipped. They can contain prompts, paths, local machine
metadata, and third-party page state.

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

These are machine-local samples, not universal latency promises. Reproduce them
with:

```sh
go test -count=1 -v ./internal/browser -run TestPrearmedSettleIsMateriallyFaster
go test -count=1 -v ./internal/browser -run 'TestEventWaitLatencyBeatsThePollingFallback|TestLoadWaitLatencyBeatsTheInPageProbe'
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

The measured MCP catalogues are 86 tools / ~23.6k tokens for `all`, 26 / ~7.9k
for `core`, 13 / ~4.3k for `minimal`, and 14 / ~4.5k initially for the default
`auto` profile — the same figures README.md and docs/agent-guide.md quote, from
`scripts/measure-tool-catalogue.py`. Thus the default starts about 81% smaller
than advertising every tool, while every tool remains directly callable and
discoverable through `brw_tools`.

A daemon whose identity names a transport advertises fewer: the three
extension-only tab-group tools drop on direct CDP, and the incognito, context
and cookie tools drop on the extension bridge. The numbers above are the
unfiltered catalogue, which is the ceiling.

The figures in the rest of this paragraph are historical: they measure the
release that introduced the `auto` default, not this tree, and the catalogue has
grown since. At that point the CLI default was `all`: 55 tools and about
12,109 tokens. The new default `auto` started at 13 tools and about 4,060 tokens,
which was **66.5% less catalogue context than the old default**. The opt-in full
surface grew by seven tools and about 11.4% (12,109 to 13,488 tokens) because it
now describes the recipe and artifact APIs; MCPlexer installations continue to
index that full surface once and expose individual tools through semantic tool
search. Keep MCPlexer downstream routes on `all`: its pinned catalogue does not
adopt brw's session-local `list_changed` growth after `brw_tools`, while its own
six-tool code-mode façade already keeps the downstream definitions out of model
context. `auto` is for direct MCP clients that honor dynamic tool-list changes.
