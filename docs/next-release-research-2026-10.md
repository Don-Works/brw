# Next release research after 0.19

Research date: 1 October 2026. Baseline: released `v0.19.0`, commit
`ad5d6a9fbb7468d10fff5474793fd9d30b8a2694`. Three parallel Sol research lanes
tested snapshots, reader evidence and lifecycle behavior, and recipe/protocol
contracts. The parent reviewed their experiments and measured the existing
browser benchmark and event plumbing. This document distinguishes the original
research from the implementation follow-up below.

## Implementation follow-up

The context-usage-and-latency branch implements scoped metadata counters for
HTTP, MCP, CLI and the optional reader, with `brw usage` for bounded local review.
The first telemetry build was installed on all three local daemons during this
work; live HTTP and MCP rows were verified. See [usage logs](usage-logs.md) for
measurement boundaries, unknown values, retention and privacy.

Snapshot extraction now reuses three per-element scalar results and rejects
non-form roles earlier. The implementation's controlled paired measurement
reported 5,000-control frontier extraction at 31.2 → 24.6 ms and sparse form
extraction at 8.1 → 4.8 ms. These are extraction measurements on one machine,
not full-task gains. The permanent 1,800-view differential test and the opt-in
headless/headed × direct/extension harness check semantic equivalence.

Direct batches can use an immediately following, bounded `fn:` readiness wait
whose predicate was false before the action and whose document identity remains
current. The measured property-only fill fixture fell from 122.202 → 27.387 ms;
ordinary actions retain their existing settling behavior. Delayed updates,
overlays, redirects, replacement documents, cancellation and timeouts have
regression coverage. Predicates should be pure observations of page state.

The reader preserves short facts, packs multiple candidate passages and exposes
source/evidence completeness and ranges. Cancellation interrupts the child and
retains its capacity until cleanup. Provider usage, collection phases and model
request timing are captured separately. Packaging tests execute both Python
entrypoints from the extracted archive to verify the new helper ships.

Model-facing examples now start with compact frontier snapshots and require
reader qualification before making a helper the default. Current gateway
schemas still need the gateway's authenticated acceptance workflow after a
release; a daemon version check alone does not prove the model sees new flags.

The session event feed, a production host-model timing integration, and a timed
human baseline remain research work. This implementation does not establish
human-equivalent speed or complete host-model token accounting.

Reproduce the opt-in local browser measurements serially:

```sh
BRW_MEASURE_SNAPSHOT_EXTRACTION=1 go test ./internal/snapshot -run TestSnapshotExtractionMeasurement -count=1 -v
BRW_MEASURE_BATCH_READINESS=1 go test ./internal/browser -run TestBatchReadinessMeasurement -count=1 -v
BRW_MEASURE_TRANSPORT_PARITY=1 go test ./internal/extensionbridge -run TestControlledTransportParity -count=1 -v
```

The next release should optimize **page ready → useful observation → model
decision → action → verified result**. Headless is a first-class execution mode:
common workflows must produce equivalent outcomes and competitive latency in
headless and Chrome profiles. Direct brw remains usable without an intermediary
model or classifier. Human-equivalent navigation speed is the product objective;
this round did not measure a human baseline or a complete live-model browser loop.

## Recommended order

| Priority | Deliverable | Evidence and acceptance |
| --- | --- | --- |
| 1 | A repeatable full-loop latency and transport-conformance harness | Extend the controlled four-cell experiment below. Stamp readiness, extraction, serialization, request start, first visible model delta, complete decision, validation, dispatch and postcondition. Count retries, expansions and model turns. Require identical intended outcomes; report matched p50/p95 and cold startup separately. |
| 2 | Target-specific completion that removes redundant settle delays | The direct form flow took about 600 ms versus 265 ms through the extension. Direct fill/select and checkbox settle paths were materially slower. Test explicit postconditions and pre-armed navigation tracking; do not shorten generic waits without delayed-update, animation, overlay and navigation coverage. |
| 3 | Compact decision packets and deterministic continuation | Six local calls showed substantially lower latency with compact packets, but both packet sizes failed an ambiguous choice. Use fresh semantic candidates, necessary values, document identity and exact uniqueness checks. Execute already-supported recipes or unambiguous steps without another inference call; request fresh evidence on ambiguity. Gate on complete task success and total elapsed time, including recovery. |
| 4 | Reader evidence coverage and completeness in the returned packet | Preserve short facts, allow multiple complementary passages, page through bounded source windows, and expose omitted evidence to the parent. Use the seeded late-fact/distractor/multi-fact cases below as regression fixtures. A smaller packet alone is insufficient. |
| 5 | Two small snapshot optimizations | Prototype per-element scalar reuse and earlier form-role filtering. Require unchanged semantics across headless/profile transports and independent paired flow measurements. Avoid treating the two savings as additive. |
| 6 | A session-scoped page event feed with resynchronization | Delayed redirects and oversized menu snapshots are recorded dogfood problems. Deliver bounded page changes to the host, then request only the necessary new observation. Test reconnect, dropped events, document replacement, same-document navigation, lease expiry and background tabs. |

Proposed experiment gates, not measured guarantees: zero wrong-target or outcome
mismatches across 100 seeded runs per shared workflow; at least 30 independent
page runs per performance arm; no material p95 headless regression under matched
conditions. Set the numerical regression budget from a stable baseline. A
human-speed claim requires timed humans completing the same tasks to the same
end-state standard, including errors and corrections.

## What the current browser baseline costs

Three parent-run headless fixture benchmarks each passed all 35 commands. They
used Chrome 154.0.8037.93 on Apple M4 Max, Go 1.26.6, disposable profiles and
loopback fixtures. The binary was built from the pinned commit without release
linker flags, so its embedded version reads `dev`.

| Measurement | Result |
| --- | ---: |
| Sum of command durations, three runs | 3051.374 / 3000.023 / 2996.506 ms |
| CDP commands per run | 237 |
| Observation bytes per run | 52,954 |
| Structured-page read, median | 806.065 ms |
| Form flow, median | 640.032 ms |
| Shopping flow, median | 486.997 ms |

These are deterministic browser operations with no model inference. Total
process/run duration also includes startup, warmup and measurement overhead;
it is not the command-duration sum. The sparse structured read exercises the
existing 800 ms default. The dynamic fixture deliberately waits 800 ms for a
control. Those costs dwarf an 8 ms snapshot improvement. `settle_ms:0` already
exists for a caller that has explicitly verified readiness; it is not a new
feature or a safe universal replacement for settling.

The existing end-state evaluator passed four honest tasks and detected all four
sabotaged variants. It checks actual page state; it does not establish autonomous
model success. Observation token figures in the existing benchmark are
characters/4 estimates, not tokenizer or billing measurements.

## Controlled headless and profile comparison

The research fixture runs the same form workflow through direct CDP and the
extension bridge, in both headless and headed disposable Chromium profiles.
It uses Chromium 152.0.7977.82, measured viewport 1280 × 1000 at DPR 1, pacing
off, a readiness predicate, fresh tabs and cyclic transport order. This browser
version differs from the standalone benchmark above; compare within each cohort.

| Mode | Direct CDP median | Extension median |
| --- | ---: | ---: |
| Headless | 599.632 ms | 266.620 ms |
| Headed | 597.598 ms | 263.405 ms |

All 12 workflows reached the expected submitted state. Initial normalized
semantics matched between transports within each browser mode. A separate
parent rerun passed another 12 workflows and verified one identical semantic
SHA-256, final state and measured viewport across all four cells. The headless
difference was about 0.34% for direct and 1.22% for extension. Three repetitions
per cell do not support statistical non-inferiority or broad parity claims.
These are clean fixture profiles, not authenticated production sites.

The larger difference was direct versus extension: name fill/select took around
121–123 ms and checkbox click around 173 ms on direct CDP, versus around
40–43 ms each through the extension. Source inspection identifies different
settling strategies. Faster return is not automatically better readiness;
unify observable postconditions before changing timing policy.

The corrected measurement takes the first explicit snapshot before `Read`,
because direct `Read` itself snapshots. Cold explicit snapshot medians were
roughly 4.9–6.4 ms; warm snapshots roughly 0.9–1.7 ms. Model inference and tab
cleanup are outside the reported workflow timing; readiness, viewport setup
and verification are included. Preserve normal human pacing as a separately
measured configuration instead of confusing its deliberate delay with overhead.

## The model boundary matters more than packet size alone

Six serial streaming requests used the locally available `qwen3.5-4b-mlx` with
three synthetic tool-choice cases. Each case had a compact arm and an arm with
20 repeated irrelevant-context blocks, rotating order. No proposed action was
executed. The requested and returned model identities matched.

| Arm | Correct | Median first visible delta | Median complete response | Actual prompt tokens per call |
| --- | ---: | ---: | ---: | --- |
| Compact | 2/3 | 624.183 ms | 939.127 ms | 705 / 696 / 649 |
| Expanded | 2/3 | 1738.070 ms | 2255.532 ms | 2230 / 2122 / 1615 |

This supports a larger paired context-budget experiment. It does not qualify
the model as an autonomous controller: both arms clicked an ambiguous Details
target instead of requesting another observation. Exact target validation and
escalation remain necessary. There was no discarded warmup, only three pairs,
and some other local work was active. First visible streamed delta is a client
measurement, not server queue, prefill or first generated token telemetry.

The current worker metrics cannot reconstruct the whole loop. Collection timing
excludes health checking and final tab close; nonstreaming HTTP header time is
not TTFT; worker timing excludes final report/journal output. Host delivery,
parent processing, validation, action dispatch and verified completion need a
shared trace ID and explicit spans. Do not add medians from independent cohorts
and present the sum as measured full-loop latency.

## Snapshot hypotheses tested

The prototypes changed the walker only inside an isolated test harness. Thirty
timed samples per arm followed three warmup rounds, with rotating arm order,
on one page/browser instance per fixture. Geometry/style instrumentation was
the same in every arm.

| Prototype | Fixture | Baseline → prototype median | Meaning |
| --- | --- | --- | --- |
| Reuse visible, viewport and disabled scalars after assigning the ref | 5000 buttons plus two inputs, frontier limit 40 | 33.8 → 25.7 ms | 24.0% less in-page extraction time; geometry/style accessor counts halved from 10,004 to 5,002. |
| Check form-role membership before extracting names | 5000 irrelevant links plus two inputs, form lens | 9.4 → 5.6 ms | 40.4% less in-page extraction time; the same two controls remain. |

The prototype differential checks covered 1800 views; the existing role
regression covered another 600. Expanded operations included display, opacity,
ARIA state, transform, focus, width, values, replacement, reordering, shadow
roots and same-origin frames. Exact element output matched for these fixtures.
Synchronous custom-element reactions and volatile getters still need explicit
prototype-specific gates. Persistent cross-call caching is a different and
substantially harder correctness problem.

A crowded 154-candidate frontier fixture kept focused and invalid controls in
its ten results, but omitted an ordinary control that later changed. Its delta
was empty. This is documented limited-view behavior, with existing candidate
count/truncation metadata, not a newly found delta bug. Test coverage-aware
selection and scoped expansion on actual task outcomes before changing ranking.

## Reader hypotheses tested

The 24 existing Python tests passed. Seven additional local probes passed and
were independently rerun by the parent. They use fake providers or controlled
subprocesses, not model factuality judgments.

- A fact placed at 60 seeded positions in approximately 94.7k characters reached
  an oracle in 4/60 cases at an 8k prefix budget, 20/60 at 32k, and 60/60 at
  100k. This measures fact availability under truncation, not answer accuracy.
- In 200 crafted distractor cases, keyword overlap excluded the required fact
  from all eight candidates. A query with exact distinguishing clues recovered
  it in all 200. Standalone facts shorter than the passage threshold were lost.
- Selecting one passage discarded a second independent required fact. A
  classifier cannot recover evidence that was excluded before classification.
- The private report records source truncation and evidence narrowing, but the
  MCP parent packet does not forward those completeness fields.
- Fifteen cancellation schedules preserved request correlation and suppressed
  cancelled replies. Cancellation still occupies worker capacity until work
  finishes or times out, matching the documented current contract. Cooperative
  stop and prompt capacity reclamation are useful follow-up work.

Prefer bounded multi-passage evidence with offsets, source identity and explicit
incompleteness over blindly increasing every prompt. Evaluate total tokens and
latency including follow-up reads. Keep no-model, classifier-off, shadow and
selection configurations independently usable.

## Page events and exploratory browser APIs

brw already has internal event-driven waits and a session action stream.
`internal/http/stream.go` streams recorded actions on direct CDP; it is not a
complete page-navigation feed, and bridge/proxy controllers explicitly refuse
that stream. `manager_stream.go` has a bounded drop-oldest queue; connection-local
sequence numbers are assigned after delivery, so they are not resumable source
sequence numbers. A new feed needs source sequence, document generation,
ownership scoping, gap reporting and explicit resynchronization.

Navigation commit and application completion must remain distinct. CDP exposes
frame navigation and same-document navigation separately. Use these signals to
trigger a targeted readiness check, not to claim that a business operation
finished. [Chromium Page protocol](https://raw.githubusercontent.com/ChromeDevTools/devtools-protocol/master/pdl/domains/Page.pdl).

Two bounded research spikes are worthwhile after the latency/conformance work:

- Capability-probe experimental `Page.getAnnotatedPageContent` as a semantic
  extraction oracle. Pin its protobuf, compare correctness, mapping cost and
  end-to-end time, and preserve existing refs/fallbacks. Its declaration does
  not establish support in installed browser builds or every transport.
- Evaluate site-authorized cross-origin WebMCP composition using native
  exposure and permission contracts on controlled origins. Existing modern
  object-input and cancellation compatibility is already implemented.
  [Chrome imperative WebMCP API](https://developer.chrome.com/docs/ai/webmcp/imperative-api).

Asynchronous reader completion is a separate host integration. The reader pins
MCP 2025-11-25; the 2026-07-28 Tasks extension has different negotiation and task
methods. Do not mix those contracts or make task augmentation a dependency of
direct brw. [Versioned Tasks extension](https://modelcontextprotocol.github.io/ext-tasks/specification/2026-07-28/tasks.html).

## Backlog disposition and verification scope

GitHub had no open issues at review time. The Maix workspace had six nonterminal
rows: three relevant blocked distribution rows and three unrelated project
rows. The relevant work is Developer ID/notarization provisioning, Chrome Web
Store publication, and their clean-Mac installation parent. Engineering
packaging exists; the account-holder prerequisites remain unresolved. The
unrelated rows were excluded from this release plan, not closed or changed.

Much of the old parity roadmap is already shipped: trace-to-recipe drafts,
failure bundles, provider receipts, bridge event waits, artifact deduplication,
regression comparison and developer introspection. Preserve those investments.
Do not promote Firefox, hosted scheduling, arbitrary extraction, persistent DOM
caching or another intermediary service ahead of the measured latency work.

Bounded recipe parser fuzzing completed **567,599 executions in 26.386 seconds**
without panic. It checks panic robustness, not complete semantic correctness.
Selected protocol units passed 34 top-level tests; five properties repeated ten
times passed 50 executions; readiness mocks passed four top-level tests.
The parent additionally passed 17 event/stream tests three times under the race
detector, 291 extension service-worker checks and four surface guards.

The parent independently reran the three prototype differential checks
(1800 comparisons), the crowding probe and the four-cell transport fixture.
These repeatability checks do not add new seed families or independent
performance samples to the original medians. The portable reader fixture also
passed after being saved into the repository.

This was targeted research, not a full release gate or whole-system security
review. No third-party fuzzing, production profile modification, deployment,
external-provider spend or runtime patch was part of the work. See the
[raw measurements](measurements/next-release-2026-10-01.json) and
[portable reproduction fixtures](experiments/next-release-2026-10-01/README.md).
