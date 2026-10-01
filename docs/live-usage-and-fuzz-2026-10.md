# Live usage and the first post-install stress campaign

The instrumentation is collecting real workflow traffic. At 14:24:30 UTC on
2026-10-01, the work browser profile had recorded 230 MCP tool calls since the
instrumented installation, plus two catalogue reads. This cohort excludes our
headless smoke tests and reader fixtures. The source builds are
`0.19.0-usage-e26bd41` and `0.19.0-usage-189d8f4`; this is an observational
workload, not an A/B comparison between builds.

[Aggregate measurements](measurements/live-usage-2026-10-01.json) contain counts
and timings only. No page text, URLs, arguments, or user input are retained in
this report. The profile name is replaced with `example-profile`; substitute
your installed profile name to reproduce a current aggregate:

```sh
brw usage --since 4h --limit 0 --layer mcp --profile example-profile --json
```

## Where the time and output go

| Operation | Calls | p50 ms | p95 ms | Sum ms |
|---|---:|---:|---:|---:|
| Batch | 16 | 1,380.897 | 5,748.889 | 31,466.928 |
| Open | 26 | 427.644 | 2,783.925 | 23,955.858 |
| Wait | 10 | 876.651 | 1,964.338 | 8,209.829 |
| Evaluate | 83 | 4.011 | 70.001 | 3,019.964 |
| Snapshot | 26 | 15.640 | 66.803 | 636.416 |
| Read | 14 | 7.453 | 39.770 | 160.295 |
| List tabs | 5 | 20.208 | 24.491 | 92.956 |

All tool calls together account for 73.239 seconds of summed MCP handling time.
Open, batch, and wait contribute 86.9%. These totals can include concurrent
calls; they are not elapsed session time. Metadata alone cannot separate
necessary network/readiness delays from avoidable settling.

Tool arguments total 53,470 serialized bytes; tool responses total 696,741
serialized bytes. Five tab listings account for 155,775 response bytes (22.4%).
This is a clear candidate for projection at the gateway: retain tab identity,
title, and ownership needed for the decision, and avoid repeating unused fields.
The 127,737 output-token estimate counts text characters divided by four; it is
not provider-billed context. The gateway may filter output before the model
receives it. Binary output and structured/text duplication also make transport
bytes different from useful model context.

The parent model's inference, scheduling, and decision time are not measured.
No full page→model→action latency or human-speed claim follows from these data.
The next useful experiment is a correlated host trace linking observation
delivery, model start/finish, and the following action, with provider token usage
where available.

Six HTTP failures appeared in the earlier 14:12 UTC work-profile sample: five
`foreign_extension_frame` snapshot errors and one generic read error. HTTP and
MCP rows overlap and must not be added. The older MCP logger also missed typed
batch/plan failures with `OK:false`; the follow-up fix records those as errors
without interpreting arbitrary page data such as `{ok:false}` as a tool failure.
Historical zero-error MCP counts are therefore not proof of workflow success.

## Local testing and resulting fixes

Three parallel Sol lanes exercised local parsers, log handling, mocked reader
providers, and disposable browser profiles. Their independent fixes are
integrated into the same release branch. This campaign does not probe external
sites or alter signed-in browser sessions.

Seven coverage-guided parser/logging campaigns completed 20,711,894 executions
in 489.461 seconds of summed package duration. This count measures fuzz inputs,
not browser workflows. The properties cover token-count arithmetic, JSON
measurement, aggregation, line boundaries, bounded file reads, and recipe
parsing/round trips. Focused race tests and 25 repetitions of concurrent recorder
rotation/close and obstruction recovery passed. Oversized log lines deliberately
mark coverage bounded and stop that file; records after them are not counted as
successfully inspected.

- Readiness deadlines: the timeout was armed only after the initial async
  predicate completed. A never-resolving predicate requested for 40 ms instead
  consumed a 400 ms outer deadline. The timer now starts before evaluation, and
  late async results cannot extend it.
- Usage logging: extreme integer character counts could overflow token
  estimates; division/remainder avoids the overflow. A failed log rotation
  could leave recording permanently closed; subsequent records now retry
  opening the file after the obstruction is removed.
- MCP outcomes: typed batch and plan failures now count as failed operations,
  including cancellation. Response payloads retain their existing contract.
- Reader lifecycle: a descendant inheriting stderr could delay a completed
  worker and hold its capacity slot. Owned process-group cleanup and bounded
  stderr draining close that gap. Extract mode now respects smaller answer
  budgets; interrupted HTTP error reads close their descriptors; non-finite
  provider JSON and invalid source counters/ranges are rejected or omitted.

The reader suite passes 58 tests, including more than 16,000 seeded/iterative
local cases and 192 burst requests, with fake providers and isolated log files.
The descendant cleanup regression was exercised on macOS/POSIX; it does not
establish equivalent descendant control on Windows.

The browser lane compared 6,000 role-filtered views under seeded DOM mutations,
including Unicode, changing input types, shadow roots, frames, and shrinking
documents. It also ran 144 seeded readiness workflows and 40 matched complete
flows across headed/headless and direct/extension transports. All four arms
produced the same normalized initial observation and matching final state for
each seed.

| Browser mode | Transport | n | Whole-flow p50 ms | p95 ms |
|---|---|---:|---:|---:|
| Headless | Direct | 10 | 595.158 | 645.594 |
| Headed | Direct | 10 | 596.723 | 637.883 |
| Headless | Extension | 10 | 256.965 | 266.660 |
| Headed | Extension | 10 | 277.036 | 350.810 |

These flows use Chromium 152, viewport 1280×1000, DPR1, pacing off, explicit
fixture readiness, and cyclic transport order. They include open, observation,
read, fill/select/click, submit, and final-state verification; browser startup,
tab cleanup, model inference, and gateway overhead are excluded. p50 here is
the average of the central pair, while the usage viewer uses nearest rank.
At ten samples p95 is the maximum. Headless parity holds in this fixture;
broader site parity remains to be measured.

## Next experiments

1. Correlate host model timing and actual token usage with these request IDs.
2. Compare bounded batches and explicit readiness against the same verified
   workflow using single actions; record success and total wall time together.
3. Measure model-visible tab-list projections after gateway filtering.
4. Minimize the nested extension-frame failures into a local fixture before
   proposing a remedy. Existing same-origin parity does not cover that case.
5. Evaluate a rendered-page fallback hint for thin public-reader results. A
   Maix peer reported an HTTP-successful search page with only a short shell;
   this is a hypothesis to reproduce, not a confirmed fix or permission to
   bypass a site's access controls.
