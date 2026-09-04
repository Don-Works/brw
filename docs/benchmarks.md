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
| In-memory 100,000-entry catalogue, rare intent | 740 ns indexed vs 5.64 ms linear control; **~7,631x faster** |
| Local 100,000-entry catalogue, common intent top 50 | 5.78 ms, 14,240 B and 109 allocations |

These are machine-local samples, not universal latency promises. Reproduce them
with:

```sh
go test -count=1 -v ./internal/browser -run TestPrearmedSettleIsMateriallyFaster
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

The measured MCP catalogues are 62 tools / ~13.5k tokens for `all`, 24 / ~7.0k
for `core`, 12 / ~3.8k for `minimal`, and 13 / ~4.1k initially for the default
`auto` profile. Thus the default starts about 69.9% smaller than advertising every
tool, while every tool remains directly callable and discoverable through
`brw_tools`.

For the exact pre-change commit, the CLI default was `all`: 55 tools and about
12,109 tokens. The new default `auto` starts at 13 tools and about 4,060 tokens,
which is **66.5% less catalogue context than the old default**. The opt-in full
surface grew by seven tools and about 11.4% (12,109 to 13,488 tokens) because it
now describes the recipe and artifact APIs; MCPlexer installations continue to
index that full surface once and expose individual tools through semantic tool
search. Keep MCPlexer downstream routes on `all`: its pinned catalogue does not
adopt brw's session-local `list_changed` growth after `brw_tools`, while its own
six-tool code-mode façade already keeps the downstream definitions out of model
context. `auto` is for direct MCP clients that honor dynamic tool-list changes.
