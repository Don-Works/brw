# Algorithms worth testing for brw

Research date: 1 October 2026. The five algorithm proposals below are unimplemented experiments. No performance measurements were collected for them; numerical gains are hypotheses and acceptance targets, not results.

## Baseline and ranking

brw already installs a snapshot walker once per document, computes option-keyed deltas, and ranks frontier elements before applying a count limit (`internal/snapshot/scripts.go`). The October candidate build implements bounded retained delta history; that correctness change is separate from the incremental-recomputation proposal below. Extension settling already polls adaptively with a minimum floor and watchdog (`internal/extensionbridge/bridge_actions.go`). Existing compact output, semantic refs, batching, recipes, read_url and WebMCP are not new recommendations.

Highest priorities: (1) conservative dirty-region semantic recomputation; (2) readiness predicates evaluated inside the page; (3) token-budgeted diversity selection with lossless output. Merkle trees and isolated worlds are conditional experiments after profiling.

Notation: N is observed DOM size, E semantic candidates, M queued mutations, D affected nodes including dependency expansion, K returned candidates, B serialized bytes. Bounds describe proposed application algorithms, not browser implementation guarantees.

## 1. Dirty-region semantic recomputation with safe invalidation

Established foundation: MutationObserver queues child-list, attribute and character-data records; `takeRecords()` drains pending records. This enables incremental maintenance rather than repeatedly rediscovering every candidate. It does not establish that a page's visual or interactive state changes only through those records. [WHATWG DOM specification](https://dom.spec.whatwg.org/#mutation-observers).

Proposal: initialize existing full-walk semantics and maintain candidate membership plus dependencies. Observers mark dirty regions; snapshot calls drain records synchronously, deduplicate affected nodes and recompute only those regions. Keep geometry/visibility in a separate invalidation class, and periodically run the existing walker as an oracle. If observer coverage, frame generation, stylesheet changes or dependency expansion are uncertain, perform a full walk. Suppress only brw's exact internal ref-attribute writes; do not ignore all attributes.

Complexity hypothesis: initial O(N), mutation collection O(M), later work O(M + D + E) if global ranking still examines E candidates; reduce selection to O(E log K) with a bounded heap. Worst case remains O(N) plus sorting. Savings come from avoiding expensive name, visibility and layout computations for unchanged candidates, not from magically eliminating global selection costs.

Correctness hazards: `aria-labelledby` and labels can depend on nodes outside a changed subtree; ancestor hidden/inert/disabled state affects descendants; input `.value`, `.checked`, validity and focus can change without attribute records; CSSOM, pseudo-classes, fonts, viewport, scrolling and animation affect geometry; observers do not automatically cover separate shadow roots or documents. New roots and removed frames require explicit lifecycle handling. Recompute volatile state for requested candidates; maintain reverse name dependencies or conservatively dirty the relevant document.

AX supplement: CDP `nodesUpdated` describes changes to previously requested AX nodes, not a complete discovery stream. `Accessibility.enable` stabilizes IDs across calls but explicitly may affect performance. AX caching therefore needs initial enumeration, child discovery, deletion reconciliation and frame epochs, with DOM fallback. [Chromium Accessibility protocol source](https://raw.githubusercontent.com/ChromeDevTools/devtools-protocol/master/pdl/domains/Accessibility.pdl).

Experiment: synthetic local fixtures with 1k/10k/50k nodes, unchanged reads, one-cell updates, subtree replacements and adversarial dependency cases. Compare incremental and full output after each of 10,000 seeded operations. Ship only with zero semantic mismatches, no stale refs, ≥40% lower median renderer CPU at 10k nodes/≤1% dirty nodes, and ≤10% p95 regression for full invalidations. Record observer callback CPU and retained heap separately. These thresholds are proposed gates.

## 2. Event-triggered, target-specific readiness

Existing settle fingerprints contain node count and body text length. Equal-length text replacement and disabled-state changes can leave that fingerprint unchanged; reading `innerText` can also require layout work. Replace repeated cross-transport global probes with one bounded in-page promise that wakes on relevant events and verifies an explicit predicate.

Established model: Playwright defines actionability through visibility, stability, receiving pointer events, enabledness and, where appropriate, editability; its stability definition uses the same bounding box for two consecutive animation frames. These are useful concrete conditions, not proof that the application's business operation finished. [Playwright actionability documentation](https://playwright.dev/docs/actionability).

Proposal: register watchers before the action where a navigation/event could race; then verify target state on MutationObserver/focus/input/scroll/resize signals, with animation-frame checks only for geometry. For business completion, optionally accept an explicit postcondition such as a status text or attribute change. Return `predicate_satisfied`, `deadline_reached`, or `context_replaced`, rather than treating a quiet page as success. Reconcile navigation with a document epoch; retain a bounded deadline and cancellation cleanup.

Complexity: one transport invocation instead of O(P) RPCs for P probes; in-page work O(R × C), where R is predicate rechecks and C predicate evaluation cost. Coalesce bursts. Continuous mutation/animation still needs bounded fallback. Background-tab animation frames may stall, so predicate paths must not universally depend on rAF. No amount of observing quiet DOM establishes that a future asynchronous update will never arrive.

Experiment: local SPA fixtures with equal-length text changes, delayed enabling, overlay removal, moving buttons, endless SSE traffic, a delayed business status and background tabs. Measure time from actual predicate truth to return, RPC count, false-success rate and deadline cleanup. Target zero false-success cases, ≤1 wait RPC per stable document, ≥50% fewer settle RPCs and ≥25% less transport time without increasing action failure rate. Preserve intentional human pacing: optimization removes observation overhead, not configured user-facing delay.

## 3. Token-budgeted coverage selection and lossless rendering

brw already scores frontier elements individually and slices by count. The incremental proposal is selecting complementary information under an actual token budget: one representative from each relevant group, while retaining focused controls, invalid fields, changed state, modal context and ancestors needed to disambiguate identical names.

Established result: Krause and Guestrin analyze submodular information selection. General information gain is not submodular; their approximation guarantees require stated structural assumptions. Browser observations and LLM task success have not been shown to satisfy those assumptions. [Original paper, section 3](https://arxiv.org/pdf/1207.1394).

Concrete conservative algorithm: define weighted coverage over explicit features (task scope, semantic role, group/context, interaction state, change status). Greedily select marginal uncovered coverage per estimated token cost, after mandatory safety/context items. This coverage surrogate is submodular by construction, but a simple cost-ratio heuristic alone is not a blanket 1−1/e guarantee for a knapsack budget. Start with the heuristic; measure task outcomes. Naive O(KEF) for F features; bitsets and cached marginal bounds reduce practical cost. Computing candidates still costs the existing walk unless candidate caching is independently correct.

Compression: retain current compact/delta modes; benchmark an optional self-describing row schema against their current encoding. O(B) serialization with fixed column names once, exact Unicode/string escaping, explicit null-versus-empty semantics and state bitsets only when a readable legend fits. JSON arrays permit ordered row data. [JSON specification](https://www.rfc-editor.org/rfc/rfc8259). Binary/gzip transport compression does not reduce the tokens of decoded tool text; opaque encodings can increase agent reasoning burden. Token estimates must use the actual model tokenizer or a validated upper bound. Long names can exceed a budget even for one mandatory row: expose truncation and a reversible expansion path.

Experiment: fixed offline local tasks containing repeated navigation, large tables, multiple similar forms, modal workflows and late validation errors. Compare existing frontier/compact/delta with coverage selection at 512/1024/2048 token budgets. Use the same pinned model, prompts, task order and paired seeds; collect success, wrong-target actions, total input tokens and follow-up expansion calls. Proposed gate: ≥25% fewer total observation tokens, no increase in wrong-target actions, and task success non-inferiority within a predeclared 2 percentage-point margin. Include the expansion costs; smaller first responses alone are not success.

## 4. Merkle summaries only after incremental correctness

Established foundation: a Merkle tree recursively hashes leaf data and ordered child hashes, with distinct leaf/internal prefixes. RFC 6962 specifies such construction for certificate logs; DOM mutation semantics are a different application. [RFC 6962 section 2.1](https://www.rfc-editor.org/rfc/rfc6962#section-2.1).

Proposal: after candidate invalidation is reliable, keep hashes for semantic groups plus canonical leaf records. Equal group hashes skip host-side comparison/serialization; descend only changed groups. A balanced semantic index needs O(E) initial storage/work and O(log E) path recomputation for one fixed-position leaf update, excluding changed record bytes. Group expansion/insertion/rebalancing can cost more; an arbitrary DOM tree has O(height) update cost and potentially unbounded height. Hashes do not identify what changed unless cached child structure is retained.

Hazards: hashing every DOM subtree every call is still O(N) and may lose to the existing JSON fingerprints. A hash certifies only its chosen canonical fields and observed generation; omitted value, name dependency, order, visibility or options produce false equality regardless of cryptographic strength. Use schema/options/document epochs, deterministic canonical bytes and ordered children. Correctness-critical equality can retain canonical records and verify on hash match, treating hash as an accelerator; collision probability is not zero. No append-only consistency proof is applicable to arbitrary DOM edits.

Experiment: compare current exact fingerprints, flat record hashes and balanced group hashes on identical traces, including high churn and reorder-heavy tables. Gate on zero reconstruction mismatch, ≥20% lower end-to-end snapshot CPU after accounting for hashing, and bounded cache memory. Prefer flat cached records if Merkle overhead loses; the tree is not intrinsically a performance improvement.

## 5. Isolated-world installation: transport lifecycle, not new caching

CDP supports installing a script into named isolated worlds on new documents and creating a world in an existing frame. Persisted `compileScript` returns a script ID; Runtime also provides context lifecycle events and a system-unique context identifier to avoid accidental reuse after process changes. [Page protocol source](https://raw.githubusercontent.com/ChromeDevTools/devtools-protocol/master/pdl/domains/Page.pdl), [Runtime protocol source](https://raw.githubusercontent.com/ChromeDevTools/devtools-protocol/master/pdl/js_protocol.pdl).

Proposal: benchmark eager per-frame isolated-world installation against the existing hot-first/cold-fallback walker. Hold only generation-scoped entrypoints/object handles; clear on destruction/detach and validate before use. This could remove a first-call failed probe and separate private cache state from page globals, but eager injection can waste work on never-observed frames. Compilation caching need not improve already-cached hot calls. Startup O(S × frames) for script size S; warm calls send O(argument bytes), as existing hot calls already do. Navigation/context replacement forces reinstall.

Chrome documents isolated JavaScript environments while sharing DOM access; isolation does not isolate DOM attributes. [Chrome content scripts documentation](https://developer.chrome.com/docs/extensions/develop/concepts/content-scripts). Existing closed-shadow capture and page-world instrumentation may rely on page globals; move only helpers whose contracts tolerate world separation. Test extension/CDP ref parity, cross-origin frames, bfcache, navigation and process swaps. Keep ordinary frame permissions; do not use universal-access settings for this experiment.

Experiment: 1,000 local repeated snapshots and 100 seeded navigations with 0/10/100 frames, measuring cold/warm latency, source bytes sent, context errors and leaked handles. Target fewer cold fallback round trips with zero context mis-targeting and ≤5% warm p95 regression. Retain existing installation if no material measured gain.

## Common reproducibility requirements

Pin commit, Chrome version, transport, machine, headless/headed and pacing settings. Randomize paired variant order; warm up separately and report cold results. Collect at least 30 independent page runs per configuration with bootstrap confidence intervals, plus deterministic adversarial traces. Use fake local data and no signed-in sites. Save raw traces, actual tokenizer counts, Chrome CPU/heap and RPC bytes alongside the harness. Report total task wall time and errors, not solely serializer or renderer microbenchmarks. The five proposed algorithm experiments remain future work. The separate bounded delta-history change does not establish performance gains for them.
