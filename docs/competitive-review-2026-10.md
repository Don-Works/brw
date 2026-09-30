# Browser automation review, October 2026

This review combines current primary-source documentation with hands-on brw
tests. Competitor capabilities below are documented capabilities, not results
from running their agents. No matched competitor benchmark was performed, so
this report makes no overall speed, success-rate or superiority claim.

The [wider landscape](browser-landscape-2026-10.md) adds eight infrastructure,
extraction, computer-use and orchestration ecosystems, plus a concrete research
design for smaller executors working under larger planners.

## Useful ideas and brw's position

| Product | Documented strength | brw comparison and decision |
| --- | --- | --- |
| [agent-browser](https://agent-browser.dev/commands) | Compact refs, snapshot deltas, semantic actions, iframe-aware refs and application-specific waits. | brw already has compact/delta snapshots, semantic find-and-act and assertions. Its cross-origin frame support is narrower: direct-CDP clicks work, while other ref actions and batches refuse those frame refs. Expand typed frame actions only with transport-specific conformance tests. |
| [Playwright MCP](https://github.com/microsoft/playwright-mcp) | Accessibility snapshots; its maintainers recommend CLI plus skills for some coding-agent workloads to reduce tool/context overhead. | brw offers CLI, a selectively disclosed MCP catalogue and an embedded skill. Improve the accuracy of that skill and gateway deployment checks; do not add another overlapping interaction surface. |
| [Stagehand extract](https://docs.stagehand.dev/v4/basics/extract) | Natural-language extraction validated against a caller-supplied output schema. | brw extracts tables, forms and embedded structured data, but does not provide equivalent arbitrary typed semantic extraction. A possible adapter needs source attribution, validation failures and bounded output; returning invented values to satisfy a schema would be a regression. |
| [Stagehand](https://docs.stagehand.dev/v4/first-steps/introduction) | Combines model-assisted actions with deterministic browser operations. | brw's stored and caller-supplied recipes already separate workflow ownership from deterministic execution. Make that choice visible in the main skill and preserve input hashes and business-operation identity across repairs. |
| [Browser Use](https://docs.browser-use.com/cloud/quickstart) | Separates hosted agents from CDP browser infrastructure and explicitly stops managed sessions after use. | brw's provider interface and profile identities cover browser attachment; brw does not itself provide a hosted agent service. Keep lifecycle ownership explicit and close only tabs/contexts created by the caller. |
| [Chrome DevTools MCP](https://github.com/ChromeDevTools/chrome-devtools-mcp) | Performance traces, network inspection and source-mapped debugging. | brw has debugging and evidence surfaces, but this review did not establish parity in performance insight quality. A matched trace-analysis evaluation is needed before making that claim. |

## Changes driven by the tests

1. **Batch fill correctness.** On the released direct-CDP lane, a batch fill
   using the advertised `value` alias reported success but cleared the field.
   The same batch with `text` passed. Direct batches now use the same effective
   value selection as standalone fills and the extension bridge. A live Chrome
   regression checks the alias, text precedence and intentional clearing.
2. **Readable documentation extraction.** Reading agent-browser's command
   documentation through `brw_read_url` joined command tokens and flattened
   code. The HTML reader now preserves inline word boundaries and fenced
   preformatted code, including indentation. Synthetic regression fixtures
   cover both separated words and intentionally adjacent inline fragments.
3. **Explicit read settling.** The existing fixture benchmark documents an
   800 ms wait on short static pages. `brw_read({settle_ms:0})` and
   `brw read --settle-ms 0` allow an immediate read after the caller has checked
   readiness. Omission retains 800 ms; values above 5000 or below zero are
   rejected. This is a text-availability heuristic, not a readiness assertion.
   Tests cover delayed content, zero, boundaries, MCP, the actual HTTP proxy
   hop and the extension's outgoing evaluation.
4. **Skill correctness.** The main skill now explains both recipe sources,
   accepts comma-separated `include`, documents settle budgets, batches its
   example and avoids asserting against refs after a possible navigation.
   Gateway guidance distinguishes a new daemon from an accepted tool schema.
   Registry publication carries the full reference bundle.

## Live checks

The pre-change baseline used brw 0.17.0 through the gateway's headless,
direct-CDP namespace. Only newly created review tabs were used and closed.

| Check | Observation |
| --- | --- |
| Public docs, no browser | Stagehand and Browser Use advertised llms indexes were readable. An unadvertised llms path returned 404; reading the ordinary URL worked. |
| Add/remove controls | Clicking Add Element exposed a new Delete button. An initial test incorrectly omitted `ref` from `assert_text`; that was a test error. |
| Dropdown | Select option 2 and assert its value in one batch passed. |
| Numeric input | Fill using `text`, assert 41, focus, ArrowUp, assert 42 passed. The advertised `value` alias exposed the direct-CDP bug above. |
| Snapshot delta | An unchanged snapshot returned `delta:true`, no elements and empty change lists. An intervening observation discarded the old single baseline in v0.17; the candidate retains bounded history and tests that sequence. |
| Tables | Both example tables were returned as structured headers and rows. |
| Dynamic loading | Start followed by a bounded text wait reached Hello World. |

These public-site checks are a small regression sample, not the complete
browser gauntlet. Repository fixture tests provide the repeatable coverage.

## Reproducing the read comparison

Start a test daemon, then run:

```sh
python3 scripts/measure-read-settle.py --daemon http://127.0.0.1:17710
```

The script serves a short static fixture on loopback, creates one leased tab,
compares five reads each at the default, zero and 125 ms, verifies identical
text and the batch fill alias, then closes its tab and server in `finally`.
Use an authorized test profile. It performs no login or external write and
has no timing threshold: machine load changes latency. The output records the
running version and every sample. This comparison isolates one optional wait;
it is not a claim about overall browsing speed.

## LLM usability and context cost

An LLM needs a short decision path, precise arguments and small observations.
The live sample exposed two avoidable mistakes: assuming an llms index exists,
and omitting the target ref from a text assertion. The main skill now teaches
those contracts alongside `main` versus `text`, `text` versus `value`, tab ID
strings, cleanup and gateway schema freshness. Detailed tool and transport
contracts moved into references loaded only when needed.

The previous main skill was 62,620 bytes. The candidate is approximately 13 KiB,
about 79% smaller by bytes; its complete reference bundle remains available.
This is a measured file-size reduction, not a tokenizer or task-success score.
The initial catalogue remains substantial: a freshly built candidate measured:

| Catalogue | Tools | tools/list UTF-8 bytes | Approximate description/schema tokens |
| --- | ---: | ---: | ---: |
| auto, initially | 14 | 26,534 | 6,533 |
| minimal | 13 | 25,642 | 6,323 |
| core | 26 | 44,684 | 11,018 |
| all | 95 | 147,586 | 36,201 |

Token estimates use characters divided by four; they are not model-tokenizer
counts. Bytes include the JSON-RPC result envelope. Clients differ in how much
schema/result material reaches the model. JSON object results can include both
text and structured content; do not assume every client charges both to context.
Repeat with `python3 scripts/measure-tool-catalogue.py`; it now uses disposable
headless profiles rather than an existing browser profile.

Repeated discovery now spends its four-tool budget on new matches. Compact frame
rows retain click centers. Snapshot history retains up to eight generations and
2 MiB serialized payload per document, with normalized option keys and
process-lifetime numeric versions. Intermediate find/observe calls can therefore
preserve an explicit baseline. Eviction, changed options, navigation, enriched
AX/frame snapshots and restart can still require a full response. Retained
payload bytes exclude transient buffers and JavaScript object overhead.

## Candidate measurements

On 1 October 2026 local time, the candidate (`0.17.0-dirty`, before release) ran
on Apple M4 Max, macOS arm64, Go 1.26.6, headless Chrome 154.0.8037.93. Five reads
per setting on the short static loopback fixture produced:

| Settling budget | Median wall time | Individual samples, ms |
| --- | ---: | --- |
| Default, 800 ms | 833.81 ms | 1024.64, 827.31, 846.78, 825.95, 833.81 |
| 0 ms | 7.17 ms | 17.50, 18.04, 7.17, 2.40, 2.58 |
| 125 ms | 133.34 ms | 132.94, 138.95, 133.34, 138.60, 130.39 |

All reads returned identical `Ready` text. The batch `value` fill alias also
passed. Settings ran in fixed order, the first default read includes cold costs,
and five samples do not establish confidence intervals. The approximately 99%
reduction applies to this deliberately sparse, already-ready page and optional
wait. It cannot be generalized to arbitrary websites or total agent task time.

`task bench` also completed all 35 commands across forms, shop, dynamic content,
structured data and read-path fixtures: 4,650.871 ms summed command time,
52,612 observation bytes, 237 CDP commands and 473 CDP messages. The harness
recorded 235.2 MiB peak RSS for the largest browser process, not total browser
heap. The local JSON record is `dist/bench/record.json`; rerunning replaces it.
Its approximate observation-token figure is derived from characters, not a model
measurement. These are one-run diagnostics with no matched competitor baseline.

`task agent-eval-verify` graded all eight runs as expected: four ordinary fixture
flows succeeded and four intentionally sabotaged flows failed. This checks that
the evaluator can distinguish success from a plausible but false report. It is
not a comparison of LLM policies or a measured small-model success rate.

## Architecture decisions

Repository-owned recipes use `.brw/recipes/` and the existing inline execution
path. Optional HTTP registries can wrap Maix, Notion or Postgres without coupling
browser execution to any of them. See [repository recipe guidance](repository-recipes.md).
Named bounded extraction adds deterministic section, table and normalized-data
selection to captures, with metadata handles rather than large inline results.
It does not claim Stagehand's arbitrary model-assisted schema extraction.

UCP profile discovery now summarizes bounded declarations using the existing
probe. Native WebMCP and payment authorization remain distinct contracts; see
[commerce interoperability](commerce-interoperability.md). No purchase or
processor settlement was exercised.

The [algorithm research](algorithm-research-2026-10.md) evaluates dirty-region
semantic maintenance, in-page readiness predicates, token-budgeted coverage,
conditional Merkle summaries and isolated-world installation. It gives complexity
bounds, correctness hazards and proposed acceptance experiments. Those five
experiments are not shipped optimizations and have no measured gain here.

## Remaining priorities

1. Matched, reproducible cross-tool task fixtures measuring end-state success,
   output bytes, round trips and latency on the same browser and hardware.
2. Typed cross-origin frame fill/select/press, tested on each supported transport.
3. Evaluate arbitrary schema extraction beyond the deterministic bounded sources.
4. Compare trace-analysis usefulness on the same deliberately slow local app.

Native Safari/Firefox breadth, hosted agent orchestration and provider-specific
infrastructure remain separate product decisions. Shipping nominal commands
without reliable execution and verification would not establish parity.
