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

## Dense-page follow-up: measured findings and algorithm choices

The [October 1 performance round](benchmarks.md#dense-page-snapshot-round-2026-10-01)
found a quadratic recovery-path calculation even when output was capped to 40
controls. A small response does not imply cheap extraction. A lazy per-walk
sibling index now preserves exact paths while eliminating repeated traversal;
the paired 5,000-control frontier measurement improved from 302 to 32 ms.

Live use confirmed that semantic find-and-act, batch assertions and compact
deltas are effective. It also exposed three easy mistakes: reading an empty
viewport frontier as an absent target, guessing capitalization for a text wait,
and missing the reason a compact delta request returned a full snapshot. The
prompt now addresses the first two; the renderer explicitly reports the third.

The research choices below are engineering conclusions, not measured claims
about unimplemented changes:

| Technique | Fit and next experiment |
| --- | --- |
| Per-walk exact index | Implemented. A node-keyed WeakMap and incremental per-parent counts avoid quadratic sibling scans without cross-call staleness. [WeakMap semantics](https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/WeakMap) support object-identity keys without retaining nodes indefinitely. |
| Dirty-subtree cache / Merkle-style fingerprints | Promising for repeated unchanged snapshots. Prototype only with invalidation for mutation records, form properties, focus, viewport, stylesheet/layout changes, shadow roots and frame navigation. [Mutation observers](https://dom.spec.whatwg.org/#mutation-observers) report DOM mutations; they are not a complete UI-state invalidation oracle. Compare exact results to a fresh walker before enabling reuse. |
| Bloom filters | Useful as a negative prefilter before an exact lookup if candidate sets become very large. Do not use them to decide that a change has already been seen or a target is unique: their [false positives](https://tsapps.nist.gov/publication/get_pdf.cfm?pub_id=903775) would suppress information. Current DOM lookup work is better served by an exact index. |
| Read/write phase separation | Profile forced layouts before moving ref writes. [Chrome's guidance](https://web.dev/articles/avoid-large-complex-layouts-and-layout-thrashing) explains why interleaving geometry reads with style-invalidating writes can be expensive. Existing pages may style ref attributes or react synchronously, so prove behavior as well as speed. |
| Install reusable action scripts once | Tested and rejected for this local workload: fill/select caching cut transmitted bytes only 2.46%, added five CDP round trips and showed no latency improvement. A future remote-CDP experiment needs a different installation strategy and its own paired evidence. |
| Bounded top-k selection | A heap can replace a full sort when candidate ranking dominates. Instrument candidate counts and sorting time first: reducing path traversal paid off without changing which controls win. |
| Intent-based waits and scoped observations | Keep explicit postcondition assertions and semantic targets. [Playwright actionability](https://playwright.dev/docs/actionability) is a useful reference for readiness rather than fixed delays. [Agent-browser's scoped/compact diffs](https://github.com/vercel-labs/agent-browser) and [Playwright MCP's file snapshots](https://github.com/microsoft/playwright.dev/blob/main/mcp/snapshots.mdx) support the same principle of sending only useful observations to the model. |
| Gateway result deduplication | Measure whether a client actually feeds both MCP representations to its model. The [MCP contract](https://github.com/modelcontextprotocol/modelcontextprotocol/blob/main/docs/specification/2026-07-28/server/tools.mdx) recommends a text copy for compatibility alongside structured content. Keep brw interoperable; remove redundant model-context copies in clients that understand both. |

The next highest-value experiments are reusable action modules for remote
transport, then conservative dirty-subtree caching for repeat observations.
Neither was enabled in this round. Measure end-to-end task completion and
serialized model context alongside extraction time; a faster walker cannot
remove network waits or the cost of another model turn.

## Remaining priorities

1. Matched, reproducible cross-tool task fixtures measuring end-state success,
   output bytes, round trips and latency on the same browser and hardware.
2. Typed cross-origin frame fill/select/press, tested on each supported transport.
3. Evaluate arbitrary schema extraction beyond the deterministic bounded sources.
4. Compare trace-analysis usefulness on the same deliberately slow local app.

Native Safari/Firefox breadth, hosted agent orchestration and provider-specific
infrastructure remain separate product decisions. Shipping nominal commands
without reliable execution and verification would not establish parity.

## Role pushdown and local executor follow-up

The next confirmed win applies [database predicate pushdown](https://duckdb.org/2024/11/14/optimizers)
to the DOM walker: reject nonmatching roles before computing names, paths and
geometry. Live-page extraction medians improve 3.25–9.46×; the
[benchmark record](benchmarks.md#role-filtered-live-page-extraction-2026-10-01)
separates this in-page CPU result from network and model latency.

A DOM has no general ordering by semantic relevance, so binary search cannot
find an arbitrary named control. Useful structures are exact role/name indexes,
bounded top-k selection and progressively refined subtrees. Persisting an index
requires a complete invalidation model. [DBSP](https://www.vldb.org/pvldb/vol16/p1601-budiu.pdf)
and [self-adjusting computation](https://www.cs.cmu.edu/~guyb/papers/ABBHT09.pdf)
are useful research models for maintaining derived views, but DOM mutation
notifications alone do not capture focus, form properties, layout or viewport
state. This round deliberately uses a per-walk optimization with no retained
cache to invalidate.

For asynchronous browsing, keep one bounded job per leased tab. A deterministic
controller owns navigation, action validation, deadlines, cancellation and
postconditions. It supplies a model only the goal and relevant fresh controls,
with a small action vocabulary. Run exact recipes and unique semantic matches
without inference. Escalate ambiguous or repeatedly unsuccessful decisions to
the parent. Do not let a model's self-reported confidence substitute for checks.

Maix already has delegation result routing to the parent's durable mesh address;
reuse that transport for one bounded result containing outcome, final URL,
evidence and a failure reason. Intermediate snapshots should stay with the
worker. A queued mesh result becomes visible at the parent's next receive/tool
boundary; it is not proof that a busy parent immediately resumed. Avoid creating
a repository worktree for every model-only browser decision.

Two local Maix adapter canaries did not qualify the path: the existing 27B alias
hit a 30-second deadline and finished after 51.67 seconds including failed
worktree cleanup; an older 0.5B baseline returned an invented ref after 9.82
seconds, and its post-hook binding failed. Both results were reviewed and
rejected. Neither is a measurement of the newer candidate models below. The
in-process HTTP benchmark isolates model/serving latency from that adapter
setup. No autonomous small-model browser worker is enabled by this change.

### Model selection method

The shortlist uses current small tool-trained models, not legacy Qwen 2.5 or
Llama 3.2 merely because those files were already present. Primary model cards
for [Qwen 3.5 0.8B](https://huggingface.co/Qwen/Qwen3.5-0.8B),
[Qwen 3.5 2B](https://huggingface.co/Qwen/Qwen3.5-2B),
[Liquid LFM2.5 230M](https://huggingface.co/LiquidAI/LFM2.5-230M) and
[Liquid LFM2.5 1.2B](https://huggingface.co/LiquidAI/LFM2.5-1.2B-Instruct)
explicitly document tool use. Documented capability is not a passing local
qualification. The official Qwen catalog has newer 3.8 models at much larger
sizes; the installed `qwen-local-3.8` alias resolves to 27B, not 3.8B.

[Qwen 3.6 35B-A3B](https://huggingface.co/Qwen/Qwen3.6-35B-A3B) is a useful MoE
candidate: 35B total parameters, 3B active per token. Active parameter count is
not the resident weight size, prefill cost or a latency guarantee. Its installed
8-bit MLX weights occupy about 37.75 GB on disk. The first two local requests
timed out after 30 seconds each while the server reported `processingPrompt`;
the repeated run was stopped. After warmup and an explicit reasoning-off request, the MoE completed the
same cohort at 813 ms median, scoring 15/24. All 15 unambiguous cases passed;
all nine stale/missing/ambiguous cases failed. A tiny-model latency comparison
therefore needs both cold-start and warm execution evidence.

The benchmark supplies four narrowed browser tool schemas through the server's
[tool-calling API](https://lmstudio.ai/docs/developer/openai-compat/tools),
not an instruction to print an arbitrary JSON object. Scoring requires exactly
the right tool and arguments; extra calls, invented refs, prose and incorrect
abstention fail. It compares terse text with explicit control-state objects,
and separately tries two examples. It never executes a model-proposed action.
The real browser trial is separately supervised and checked after each step.

Run `python3 scripts/measure-local-browser-model.py --model MODEL --structured
--out result.json` against an already running local endpoint. Default requests
have a 128-token output cap, temperature zero and `reasoning_effort:none`.
`--reasoning-effort template` reproduces the older template-only request.
On 9B, the template flag left reasoning enabled: 1/24 passed at 3.66 seconds
median. Explicit reasoning-off improved that to 16/24 at 1.31 seconds; it did
not cure all targeting failures. Both input/output usage and reasoning tokens
are retained. The rendered model inputs were checked to verify that the
controller instructions actually reached the models. The
script stops after consecutive transport errors. The eight decision patterns
are each repeated with three seeded ref/order permutations: useful for finding
failures, not an independent 24-task production qualification. The evidence
records warm p50/p95, first-request time, actual calls and reported token usage.

### Observed local configurations

| Configuration | Correct calls | Warm median ms | Warm p95 ms |
| --- | ---: | ---: | ---: |
| LFM2.5 230M Q8 | 3/24 | 63 | 139 |
| Qwen 3.5 0.8B MLX 4-bit | 6/24 | 228 | 286 |
| LFM2.5 1.2B Q8 | 13/24 | 233 | 388 |
| Qwen 3.5 2B MLX 4-bit | 11/24 | 318 | 394 |
| Qwen 3.5 4B MLX 8-bit | 16/24 | 767 | 1031 |
| Qwen 3.5 4B + prose examples | 21/24 | 899 | 1159 |
| Qwen 3.5 9B MLX 8-bit, explicit off | 16/24 | 1305 | 1690 |
| Qwen 3.6 35B-A3B MLX 8-bit, explicit off | 15/24 | 813 | 1181 |
| MoE + prose examples | 1/24 | 575 | 725 |
| MoE + actual tool-call examples | 17/24 | 1347 | 1805 |

These are configuration results, not intrinsic model rankings. Quantization,
format, served templates and prompts differ; CPU browser tests ran alongside
some measurements. The primary shortlist comparison uses structured observations
without examples. All tested configurations still make consequential errors.
The 4B model reaches 21/24 with prose examples, but that same example style
causes the MoE to print calls as plain text and scores only 1/24. Actual tool-call
history examples raise the MoE to 17/24, with more input and latency. A harness
change can dominate a model-size change. None of these runs qualifies an
unattended worker.

The [raw evidence](measurements/local-browser-models-2026-10-01.json) retains
configuration, quantization, actual outputs, per-case timing and token usage.
Earlier drafts that omitted field values or used a noncanonical keypress schema
were corrected before this recorded cohort. A supervised Wikipedia trial let
4B choose the correct fill target, verified the value with brw, then rejected an
incorrect repeated-fill proposal; the parent completed and verified navigation
to Merkle tree. That is a harness-assisted recovery, not autonomous model success.

The practical next experiment is a worker with exact candidate/action validation
and a small, canonical observation packet, evaluated on held-out public-site
jobs. Select the fastest configuration that meets the task-success bar after
counting refreshes, retries, escalation and parent review. Preserve the same
jobs and request contract when comparing hosted models. Keep raw snapshots out
of the parent's context and send one bounded mesh result when the job completes.

## Optional reading workers and shadow classifiers

Direct brw remains the default. The experimental
`scripts/browser-answer-worker.py` is a separate, configurable public-page worker;
it adds no model dependency to the daemon or MCP tools. It accepts local or cloud
OpenAI-compatible answer endpoints, TypeSafe-compatible decision endpoints or
OpenAI-compatible JSON classifiers, model IDs, credential environment-variable
names, request budgets and a JSON configuration file. Either helper can be off.
The worker collects under a unique tab owner, closes its tab, stores the source
outside parent context and returns a bounded answer/source packet. Its supported
wire formats are explicit; arbitrary provider APIs are not interchangeable.

A generic coding-worker harness was a poor fit for these small jobs. An offline
DeepSeek Flash batch answered eight typed cases correctly in 2.956 seconds total,
but a mandatory coding-report wrapper conflicted with the requested JSON-only
format. Two attempted tool-enabled reader delegations failed their cumulative
input/tool budgets without delivering an answer. That is harness evidence, not
proof that the model cannot browse. The direct adapter subsequently completed the
one-sentence job. The cumulative input-spend cap is distinct from context-window
capacity. The local model benchmark now journals request IDs/hashes, tool-schema
size, input/output/reasoning tokens, response bytes, serving identity, phase times
and classified failures. Queue, prefill and first-token times are explicitly
unmeasured by these non-streaming clients.

### Reading results

The Johns Hopkins canary kept a 30,035-character page in a worker artifact and
returned a 236-character answer/source JSON packet: 99.21% fewer characters in
that returned payload. This is not a measured reduction in the main model's total
billed tokens or its response time. The no-generator mode returns an excerpt,
leaving interpretation to the caller. The JHU history URL returned a 403, so its
owned tab was closed and the canary used a different public source, Wikipedia.

A subsequent paired replay used three pages (Johns Hopkins, Bloom filter, Merkle
tree), three repetitions, and alternating local/hosted and full/selected order.
The later deterministic-ranking cohort is separate, not interleaved with that
first cohort. All paths used the same captured source per question. Timings below
exclude fresh collection, process startup, main-model review and delivery. The
first cold-looking local canary took 5.3 seconds; it is not the warm median.

| Answer configuration | Calls | Median worker ms | Reported provider spend for cohort |
| --- | ---: | ---: | ---: |
| Local Qwen 3.5 4B, full evidence | 9 | 710 | Local compute unpriced |
| Local 4B, Jev-selected evidence | 9 | 829 | $0.001506 plus local compute |
| Local 4B, deterministic passage | 9 | 441 | Local compute unpriced |
| Hosted DeepSeek V4.1 Flash, full evidence | 9 | 1345 | $0.004203 |
| Hosted Flash, Jev-selected evidence | 9 | 1553 | $0.002089 |
| Hosted Flash, deterministic passage | 9 | 1264 | $0.000327 |

Jev reduced reported hosted spend here but added median latency. Deterministic
selection was faster and cheaper, but all six shortened-evidence local Merkle
answers incorrectly generalized that Merkle trees are binary. Full-evidence local
answers and the hosted answers did not make that specific error. The raw answers
are retained for inspection; this is a small exploratory correctness review, not
independent expert scoring or a production qualification. The performance gain
therefore does not justify automatically dropping full evidence. Cache effects,
source length and answer length matter, and nine repetitions are not nine distinct
jobs. A source hash and context sizes make each result reproducible without
committing copied full pages.

### Tool decision results

The existing eight synthetic patterns, each with three ref/order permutations,
were evaluated without executing proposed actions:

| Contract/configuration | Correct / cases | Median ms |
| --- | ---: | ---: |
| Native tool calls: local 4B with prose examples | 21/24 | 899 |
| Native tool calls: hosted Flash, no examples | 22/24 | 1124 |
| Filtered candidate choice: local 4B, JSON schema | 16/24 | See per-case evidence |
| Filtered candidate choice: Jev | 24/24 | 271 |
| Narrow exact-match resolver | 24/24 | No inference call |

These contracts differ: the typed classifier receives prefiltered candidates;
free-form native calls must also produce arguments. Neither table is an intrinsic
model ranking. Flash's two native failures filled stale controls. The 4B's three
example-assisted failures were two ambiguous choices and one page-instruction
misdirection. Exact validity/uniqueness checks are required around every model.

Twelve additional semantic cases exposed a Jev ambiguity error with confidence
0.88. Its raw result was 10/12; review found one incorrect expected label: without
ordering evidence, “following page” cannot be mapped uniquely to “Older.” Jev
correctly abstained there. The adjudicated result is 11/12; both original and
corrected labels are retained. Local 4B candidate choice scored 7/12 against the
corrected labels. The exact resolver abstained on all twelve; five abstentions
were required and seven were unresolved answerable cases. This is an exploratory
cohort, not held-out proof. The global `jev-decisions` skill now defines shadow
trials, independent outcome review, context/cost accounting and promotion gates.

The relevant algorithm is often decomposition: code filters impossible targets,
a classifier judges semantic relevance, and code enforces uniqueness. TypeSafe's
[fan-out pattern](https://docs.typesafe.ai/patterns/fan-out) and
[Noul primitive](https://docs.typesafe.ai/primitives/noul) support asking independent
candidate-relevance questions in one request. This is tested separately from
winner-takes-all choice; probability thresholds remain workload-specific.

### What remains worth measuring

- Mutation-scoped reuse of unchanged subtrees, with conservative invalidation for
  visibility, layout, accessible-name dependencies, frames and shadow roots.
  A cache that misses an invalidation is a correctness regression, not a speedup.
- Indexed candidate discovery and role/name predicate pushdown before geometry;
  search a semantic index rather than applying binary search to an unordered DOM.
- Action-plus-postcondition responses that avoid a second whole-page observation;
  existing assertions, batch steps and bounded recipe extraction already help.
- Native structured data and exact category routing before any model/classifier.
- Bounded asynchronous jobs with small result packets and explicit escalation;
  measure completion quality and parent interruption as well as inference time.
- Request-level CDP, extraction, serialization, proxy and model-serving telemetry.
  Aggregate dogfood logs showed no new disconnect/panic/timeout cohort, but no
  substantial peer workload has yet established an end-to-end performance win.

The rejected action-script cache saved only 2.46% of protocol bytes while adding
round trips and slightly increasing fixture elapsed time. It remains uninstalled.
The role-filter pushdown and exact per-walk indexing are installed; classifiers
and reading workers remain optional experiments. See
[worker/decision evidence](measurements/decision-workers-2026-10-01.json) and the
[worker operating guide](../skills/brw/references/decision-workers.md).

The batched relevance experiment resolved 35/36 at median 278 ms (p95 464 ms).
Unlike winner-takes-all choice, it made no wrong-target proposal in this cohort:
the sole miss was an unnecessary refresh because “Documentation” received 0.78,
below the preselected 0.8 yes threshold. Alternatives had to be at or below 0.2;
multiple supported candidates triggered refresh in code. Thresholds were not
retuned after seeing this result. This shifts the observed error from an incorrect
action to abstention; it does not establish those thresholds on new workloads.
The resolver also passed 240 seeded observation-order permutations. All proposed
actions in these classifier/model tests remained unexecuted.
