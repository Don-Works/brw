# Browser automation review

Assessment date: 4 September 2026. Scope: the `brw` source tree and a local
browser-host deployment built from this working tree. This is not evidence about
an uninspected public deployment, identity provider, reverse proxy, or operator
machine.

## Outcome

The private recipe and artifact design is fit for its intended local or
authenticated-tunnel boundary. It materially improves repeated workflows:
execution is pinned and model-free after discovery, browser bytes stay on the
browser host, synchronous page reactions are observed without a missed-event
delay, and the default MCP catalogue is much smaller. The concrete measurements
and reproduction commands are in [benchmarks](benchmarks.md).

The design deliberately does **not** turn `brw` into a secret store, scheduler,
or recipe database:

- The public project owns the strict recipe ABI and deterministic runner.
- A small installation may use an owner-only local directory outside every
  public checkout. A large installation should use its private HTTPS provider,
  which can put metadata and embeddings in its own database/vector index.
- Search discloses only name, description, exact origins, risk, version, score,
  and digest. Run fetches the exact `id + version + digest` selected by the
  caller; a substituted body fails closed.
- A Codex skill may explain when and how to call `brw_recipe_search`, but should
  not contain the operational recipe body or credentials. Skills are discovery
  adapters, not the authority or confidential corpus.
- Page timers and page/browser events are part of one bounded run. Cron,
  webhooks, calendars, authorization, and cross-run retries belong in the outer
  workflow system.

This split keeps private site structure out of the open-source repository and
allows a large provider to change ranking, retention, access control, and audit
without changing the public execution ABI.

The local directory provider checks file metadata on every operation to notice
new immutable versions without a daemon restart. Its in-memory index is fast,
but the change-detection pass is O(N); the 100,000-entry catalogue benchmark
does not include that pass. This is the concrete cutoff between the convenient
local cache and the provider-backed huge bank.

## Functional and security findings

| Priority | Finding and disposition | Evidence |
| --- | --- | --- |
| Closed | The extension transport advertised `navigate_to` inside `brw_batch` but did not implement it, and a generic committed wait could already be true on the source page. Standalone, batch, and plan now wait for the target document: the extension pins one tab, pre-arms trusted document identity plus the main-frame loader, requires the exact loader returned by `Page.navigate` (or exact same-document URL), policy-checks redirects, and verifies continuity after readiness; exact-current URLs and SPA route changes are checked against the live document's `location.href` because the CDP frame tree can retain a differently serialized URL, and a speculative loader returned for an in-place route is accepted only while document identity and the pre-armed frame/loader stay unchanged; direct CDP uses Chromedp's pre-armed loader-filtered navigation. Slow replacement, an unrelated allowed-origin navigation, a second replacement during readiness, redirects, fragments, speculative non-committing SPA loaders, URL/frame serialization disagreements, and direct-plan parity are regression-tested. | `internal/extensionbridge/bridge.go`, `internal/extensionbridge/bridge_navigation_test.go`, `internal/browser/manager_navigate_test.go`, `internal/mcp/server_test.go` |
| Closed | Draining captured network responses removed in-flight fetch/XHR rows, so a fast poll could permanently lose the eventual terminal result or let an older pending request satisfy a later step. Pending rows now survive drains with stable document-epoch capture IDs, terminal rows are consumed exactly once, recipe arming excludes pre-existing pending IDs, and the direct and extension transports execute the same redacting lifecycle scripts. | `internal/snapshot/network_test.go`, `internal/recipe/browser_surface_test.go`, `internal/browser/manager_network_test.go`, `internal/extensionbridge/bridge_network_test.go` |
| Closed | Replacing an extension connection could let delayed frames from the old socket mutate live identity, pending calls, chunks, emulation, tab ownership, observations, or download cursors. Connections are generation-gated; an identity-changing replacement clears profile-bound state while a same-identity reconnect preserves the intended state. Each 0.4.14+ hello also reports only the extension-owned pin (never foreground state), reconciled before reconnect waiters wake, so a `tab_removed` frame lost during a socket gap cannot retarget a reused numeric tab ID; one-shot pins resolved just before the gap are revalidated at dispatch, while independent session leases remain valid multi-tab targets. An older hello remains wire-compatible but clears cached ownership fail-closed. Shutdown atomically rejects new work, detaches the connection, drains pending/chunk state, and does not wait forever for a peer close handshake. | `internal/extensionbridge/bridge_release_conn_test.go`, `internal/extensionbridge/bridge_chunk_test.go`, `internal/browser/context_test.go`, `extension/tab_resolution_test.mjs` |
| Closed | Closed or externally removed tabs could retain reusable emulation/ownership state, implicit isolation fallback could select a human tab, and the per-tab lock table grew without reclamation. Tab removal and authoritative no-tab errors now invalidate tab-scoped state, isolation remains pinned and fails closed, and ref-counted locks are reclaimed after cancellation and high-cardinality churn without weakening same-tab serialization. | `internal/extensionbridge/bridge_test.go`, `internal/extensionbridge/bridge_isolation_test.go`, `internal/extensionbridge/bridge_concurrency_test.go`, `extension/tab_resolution_test.mjs` |
| Closed | An installed profile could keep running an older unpacked extension after the daemon and source were upgraded, retaining already-fixed tab-selection behavior. The macOS install now refreshes the installed extension payload; the loaded build is exposed by bridge status so deployment checks can verify parity. | `Taskfile.yml`, `scripts/package-macos.sh`, `extension/manifest.json` |
| Closed | Explicitly closing a page with unsaved state could spend the full RPC timeout in `chrome.tabs.remove`, because the debugger was detached before the `beforeunload` prompt could be handled. Explicit close now uses CDP `Page.close` while marked as an agent action and does not acknowledge success until Chrome reports the tab gone. | `extension/service_worker.js`, `extension/tab_resolution_test.mjs`, `tests/fixtures/beforeunload.html` |
| Closed | A direct-CDP post-action waiter could miss a synchronous DOM reaction. It is now armed before direct actions; the extension transport keeps its separate bounded fingerprint settle, which does not depend on seeing the mutation event. | `internal/browser/manager_settle_performance_test.go`, `internal/snapshot/settle_test.go`, `internal/extensionbridge/bridge_settle_test.go` |
| Closed | Network, download, or popup postconditions could happen before their waiter. Transient events are now legal only as pre-armed action postconditions. | `internal/recipe/schema_test.go`, `internal/recipe/runner_test.go` |
| Closed | A lost acknowledgement or explicit rerun could duplicate an external write. Such steps preflight durable desired state, have one actuation attempt, and use the pre-armed postcondition to reconcile without repeating it. | `internal/recipe/schema.go`, `internal/recipe/runner_test.go` |
| Closed | A transient response/download/tab event was admissible as an external-write postcondition even though it cannot be probed on a later rerun. External writes now require durable state-specific postconditions. | `internal/recipe/schema_test.go` |
| Closed | A download-completed waiter consumed the event needed by a following artifact capture. Downloads now use retained bounded snapshots plus per-tab change baselines; newly observed completed entries are also handed directly to the browser-host store. | `internal/recipe/browser_surface_test.go`, `internal/artifact/store_test.go` |
| Closed | A secret runtime input could enter the browser action trace when the page did not mark its field as sensitive. Declared secret inputs now propagate a transport-level redaction marker. | `internal/recipe/runner_test.go`, `scripts/test-functional.sh` |
| Closed | An allowed page could navigate during target resolution, event arming, capture, or the last action and still actuate or leave bytes from another document on disk—even when both documents shared an allowed origin. Recipe origin checks now prefer trusted browser metadata. Artifact capture binds to the exact main-document loader/document ID plus a monotonic replacement epoch before and after persistence; this catches cross-allowed-origin navigation, same-origin replacement, and A → B → BFCache-A, rolls back stored bytes on any end-check failure, and leaves same-document SPA history changes valid. Video polls the epoch at a bounded cadence for early abort and performs the mandatory final check. | `internal/recipe/browser_surface_test.go`, `internal/recipe/runner_test.go`, `internal/artifact/store_test.go`, `internal/browser/manager_document_test.go`, `extension/tab_resolution_test.mjs` |
| Closed | A completed-download path could be replaced between metadata inspection and opening. Capture now verifies the open descriptor is the exact regular file previously inspected and propagates prefix-read/seek errors. | `internal/artifact/store_test.go` |
| Closed | A macOS background daemon without Downloads-folder consent could block indefinitely in `open(2)` and each retry added another native thread. Download opening is now context-bounded and single-flight: one native open receives a three-second caller budget, concurrent/repeated attempts fail fast, and every other browser operation remains available. Direct CDP uses private staging; extension-backed capture preserves the user-owned original and reports the missing host permission. | `internal/artifact/store_test.go`, `internal/browser/manager_downloads_test.go` |
| Closed | A custom provider could retain and mutate the search-result backing slice after validation. Results and nested origins are copied at the service trust boundary. | `internal/recipe/runner_test.go` |
| Closed | Upstream proxies inherited the ordinary 20-second HTTP timeout even though a bounded recipe may legitimately run for 30 minutes. Recipe calls now share the runner's maximum duration plus transport headroom while caller cancellation remains authoritative. | `internal/httpclient/artifact_recipe_test.go` |
| Closed | Secret runtime templates in event/postcondition matches were redacted from returned errors but did not mark browser inspection as sensitive. Sensitivity now covers every runtime template a step can send to the browser, not only fill/type/select values. | `internal/recipe/runner_test.go` |
| Closed | More than 200 matching controls could make a visible candidate look uniquely safe. Truncated semantic resolution now fails closed. | `internal/recipe/browser_surface_test.go` |
| Closed | Checking a URL, HTTP status, element count, element state, attribute, or a downloaded file's digest required hand-written `brw_evaluate` JavaScript, which each caller rewrote and none of which reported expected against actual. `brw_assert` and the recipe `assert` step now run those six checks once, with no retry, through the shared getters script; a download digest is evaluated on the browser host so the proxy hashes the real file. | `internal/browser/assertions_test.go`, `internal/recipe/assertions_test.go`, `internal/http/assert_route_test.go`, `internal/httpclient/assert_test.go` |
| Closed | Long element waits scanned Gmail/LinkedIn-scale DOMs every 100 ms (up to about 1,200 scans). Bounded adaptive polling makes the first recheck faster and cuts a 120-second idle wait to about 483 scans. | `internal/recipe/browser_surface_test.go`, `docs/benchmarks.md` |
| Closed | The only published performance claims about `brw` itself were head-to-head results against Claude-in-Chrome that nothing in this repository reproduced. They are deleted and listed as removed. `task bench` now measures per-command wall time, CDP round trips, transport bytes (counted at the websocket by a relay in front of Chrome, not estimated), observation tokens, and the run's CPU and peak RSS, against the local fixture suite with an environment fingerprint; `task agent-eval` grades four agent-level tasks on the end state probed out of the page rather than on what the run claimed, and `task agent-eval-verify` reruns each with its decisive act removed and fails unless every one is caught. Neither measurement is in `go test ./...` or `task check`; one evaluation task runs there in both modes as the guard that the grading can fail. | `internal/bench`, `internal/agenteval`, `internal/harness`, `docs/benchmarks.md` |
| Closed | Provider redirects could forward its bearer token. Redirects, URL credentials, query strings, fragments, downgrade, oversized responses, malformed metadata, and body substitution are rejected. | `internal/recipe/schema_test.go` |
| Closed | A custom provider implementation could bypass the HTTPS provider's validation and substitute a recipe after discovery. The service boundary now validates every search result and revalidates exact `id + version + digest` after fetch, independent of provider implementation. | `internal/recipe/runner_test.go` |
| Closed | Two local profiles could silently share the default artifact cache. The automatic root is now namespaced by runtime identity. | `cmd/brwd/main_test.go` |
| Closed | TTL was enforced on access and later writes but an idle daemon could leave expired bytes physically present. Stores now purge at startup and run a cancellation-aware retention janitor. | `internal/artifact/store_test.go` |
| Closed | A broad cache root, symlinked parent, or prospective directory could capture unrelated files or resolve back inside a Git checkout. Broad roots are rejected, existing dedicated roots are forced to `0700`, canonical prospective paths are checked before creation, and stale commit orphans are reconciled after a grace period. | `internal/artifact/store_test.go` |
| Closed | Byte-windowed reads could split UTF-8 and corrupt JSON output; Unicode case folding could also invalidate byte indexes during search. Split windows now return exact base64 and search operates on original-string byte offsets. | `internal/artifact/store_test.go` |
| Closed | Large page reads crossed the HTTP hop before being sliced. Read windows are now applied on the browser host. | `internal/httpclient/controller_test.go` |
| Closed | Extension screenshot/PDF/semantic responses near 4 MiB could tear down the bridge because one base64 JSON message exceeded the WebSocket frame limit. Logical responses now use ordered bounded chunks, validate every frame and reconstructed identity, clean partial state on every termination path, and prove the next RPC still works after malformed/oversized input. | `internal/extensionbridge/bridge_chunk_test.go`, `extension/tab_resolution_test.mjs` |
| Closed | Video capture could inherit an unbounded caller lifetime and buffer unlimited encoder stderr. Capture, screenshots, encoder shutdown, origin checks, and persistence now share a maximum 35-second budget; stderr and surfaced diagnostics are fixed-size and failure cleanup is adversarially tested. | `internal/artifact/store_test.go` |
| Closed | A malicious or failed upstream HTTP service could flood an MCP error with an arbitrarily large body. Errors are capped at 8 KiB and normalized to valid UTF-8; success responses reject trailing JSON. | `internal/httpclient/controller_test.go` |
| Closed | MCP line and Content-Length framing could allocate an unbounded client-supplied message. Both modes now fail before allocating beyond 8 MiB; bulk browser data uses artifact handles instead. | `internal/mcp/server_test.go` |
| Closed | Artifact/recipe HTTP responses could be cached by an operator's reverse proxy. JSON responses now carry `Cache-Control: no-store` and `nosniff`. | `internal/http/server.go`, `internal/http/server_test.go` |
| High, deployment gate | The native control listener is not an Internet-facing authorization service. Keep it on loopback or behind an authenticated encrypted tunnel/reverse proxy. Do not certify a remote deployment without testing that layer. | `docs/remote-control.md`, `internal/http/server.go`, `internal/http/server_guard_test.go` |
| Medium, accepted | Artifacts are owner-only files with TTL/quota and opaque capability IDs, not encrypted records or per-caller ACL objects. Callers sharing one authorized control plane share that artifact boundary. | `internal/artifact/store.go`, `docs/recipes-and-artifacts.md` |
| Medium, accepted | Artifact `redaction` is provenance metadata, not image/PDF/video sanitization. Binary artifacts may visibly contain private material. | `internal/artifact/service.go`, `docs/recipes-and-artifacts.md` |
| Medium, accepted | A rendered page PDF is not proof that a vendor issued that document. Evidence-sensitive recipes must prefer completed source downloads and fail honestly when unavailable. | `docs/recipes-and-artifacts.md` |
| Medium, accepted | The extension transport caps serialized responses at 64 MiB (about 48 MiB of base64 binary), below the store's 128 MiB raw limit. This avoids 350–500+ MiB transient MV3/Go allocations; larger captures require reduction or direct CDP. | `internal/extensionbridge/bridge.go`, `docs/recipes-and-artifacts.md` |
| Medium, accepted | brw's `auto` catalogue relies on MCP `notifications/tools/list_changed`. MCPlexer deliberately pins each downstream surface and currently does not adopt that session-local growth, so a global MCPlexer route must use `--mcp-tools all`. This has no corresponding model-context penalty because agents see MCPlexer's six code-mode tools and semantically discover downstream calls. Direct MCP clients that honor list changes should use the smaller `auto` default. | `internal/mcp/disclosure.go`, `docs/benchmarks.md` |
| Medium, accepted | The extension's two-second explicit-close budget begins after debugger attachment. A pathological unresolved `tabs.get`, `tabs.remove`, debugger attach, or `Page.enable` promise can therefore exceed the intended transaction bound before `Page.close` starts. A future fix should establish one deadline before the first Chrome API call and cover late-attach cleanup. | `extension/service_worker.js` |
| Medium, accepted | Extension download observations survive ordinary reads but not an MV3 service-worker restart. A deterministic recipe therefore fails closed if the worker restarts during a long download; durable continuity would require a bounded journal in daemon state or `chrome.storage.session`. | `extension/service_worker.js`, `internal/extensionbridge/bridge_downloads_test.go` |
| Medium, accepted | Artifact-store locking is process-local. Automatic roots are daemon/profile namespaced, but operators must not point multiple daemon processes at one explicit artifact root. | `internal/artifact/store.go`, `docs/recipes-and-artifacts.md` |
| Low, accepted | One store serializes artifact commits while streaming each payload so its aggregate quota decision remains race-free. Large concurrent captures can delay other writes/deletes; reads remain lock-free. A future reserve/commit transaction could move streaming outside this critical section. | `internal/artifact/store.go` |
| Low, accepted | Extension download provenance depends on Chromium emitting a download event while brw is attached. Unobserved or ambiguous downloads remain available to manual callers but cannot satisfy a deterministic recipe. | `internal/extensionbridge/bridge.go`, `docs/recipes-and-artifacts.md` |
| Low, accepted | If a future Chromium build supplies contradictory direct download tab IDs, the extension records the conflict but its snapshot path can still fall back to CDP provenance. Current Chromium does not normally expose this field; the fail-closed rule should eventually suppress fallback whenever a direct conflict exists. | `extension/service_worker.js` |
| Low, accepted | A maximum-size chunked extension response is bounded but fully materialized and synchronously queued, so the 64 MiB logical ceiling can transiently require more than 100 MiB across string, UTF-8, and base64 copies. Add WebSocket backpressure or stream directly into the artifact store before raising the cap. | `extension/service_worker.js`, `internal/extensionbridge/bridge_chunk_test.go` |
| Low, accepted | Full-page extension screenshots stored as artifacts currently inherit the compact model-facing JPEG path. This saves time, disk, and tokens but may be insufficient for demanding OCR or archival use; a distinct opt-in high-quality artifact mode would preserve the fast default. | `internal/extensionbridge/bridge.go` |
| Medium, accepted | Chrome's extension debugger transport exposes neither `Browser.setDownloadBehavior` nor its deprecated `Page.setDownloadBehavior` shim (live Chromium returns `Cannot not access browser-level commands`). `Network.getResponseBody` also has no body for ordinary downloads. Extension-backed artifact capture therefore reads Chrome's completed path and may require macOS Files & Folders consent; direct CDP is the private-staging option. The host read is bounded and single-flight, but one kernel-blocked thread can remain until permission changes or the daemon restarts. | `internal/artifact/service.go`, `internal/artifact/store_test.go`, `docs/install.md` |
| Low, accepted | The network observation ring is hard-bounded at 100 rows. If more than 100 requests are simultaneously in flight and no terminal entry is available to evict, the oldest pending row is dropped; response snippets also remain best-effort asynchronous. | `internal/snapshot/network.go` |
| Low, accepted | Video has no early quota reservation. `PutContext` is the atomic capacity gate, so encoding work can be wasted if another writer consumes the remaining quota first; a correct fix needs a Store reserve/consume transaction. | `internal/artifact/service.go`, `docs/recipes-and-artifacts.md` |
| Medium, accepted | `brw` provides one actuation attempt plus durable-state reconciliation, not distributed exactly-once delivery. High-consequence providers still need backend idempotency or an outer durable run ledger. | `docs/recipes-and-artifacts.md` |

Release readiness is therefore conditional: source behavior is strongly tested
for a local/private control plane, while any public service still requires its
own authentication, authorization, encryption, backup/restore, alerting,
incident-response, and live deployment evidence. Release artifacts have
checksums, a CycloneDX SBOM, and GitHub build provenance. CI actions and build
tools are commit- or version-pinned; publication alone receives write and OIDC
permissions.

## Comparison with current automation systems

The comparison is against documented capabilities, not marketing claims.

### Parity matrix: Claude-in-Chrome and Vercel agent-browser

Audited 2026-09-14 against Claude-in-Chrome (support.claude.com article 12012173)
and `vercel-labs/agent-browser` (Apache-2.0, Rust CLI + daemon + MCP). Status is
`brw`'s: shipped, open with its ledger row, or a deliberate non-goal.

| Feature | brw | Claude-in-Chrome | agent-browser | Status |
| --- | --- | --- | --- | --- |
| Semantic snapshot and stable refs | DOM + accessibility | internal | accessibility tree | shipped |
| Post-action observation | every action | n/a | partial | shipped |
| Browser coverage | 7 Chromium builds | Chrome only | Chrome, Lightpanda, cloud | shipped |
| MCP server | stdio, catalogue grows on demand | native messaging | stdio, fixed profiles | shipped |
| HTTP JSON API | yes | no | no | shipped |
| Per-action CLI | no | no | ~120 verbs | open, `2AR4JB` |
| Artifacts held off model context | store, search, TTL | no | files on disk | shipped |
| Deterministic replay | immutable recipes, digest and origin guard | recorded workflows | batch JSON | shipped |
| Promote a successful run to a recipe | no | yes, side-panel recording | no | open, `E8X4QH` |
| Scheduled and recurring runs | no | daily/weekly/monthly | no | non-goal; trigger contract `5SFWAD` |
| Network interception | isolated contexts only | read-only | route, unroute | open, `GX7V7R` |
| HAR record and replay as a fixture | record only | no | start/stop, replay | open, `GX7V7R` |
| Device and environment emulation | viewport only | no | viewport, device, geo, media, offline | open, `5K6NVM` |
| Storage and auth state save/load | denylisted on the signed-in transport | n/a | `state save`, `auth save` | open, `S1JX0S`, host-only and redacted |
| Credential manager integration | no | 1Password, macOS beta | plugin capability | open, `FGYDV6` |
| Accessibility audit, React, Web Vitals | no | no | axe-core, react, vitals | open, `M7T2Z7` |
| Cloud browser providers | no | no | 6 providers | open, `VXK4XY`, after `J3YFH0` |
| Remote operation over SSH | yes | no | no | shipped |
| WebMCP page-declared tools | list and synchronous invoke | no | detach, poll, cancel | open, `HG5BY9` |
| Live dashboard | viewport, activity feed, gated human takeover | side panel | viewport, activity feed, chat | shipped; chat is not a goal |
| Per-origin grants and revocation | domain containment only | per-site grant/revoke | domain allowlist | open, `PBZHK0` |
| Confirmation before high-risk actions | no | yes | no | open, `27GN96` |
| Category blocklist | no | financial, adult, pirated | no | open, `PBZHK0` |
| Cross-device session continuity | no | desktop, web, mobile | no | non-goal, see remote control |
| Video capture | WebM from native compositor frames | no | `record` | shipped |
| Assertion vocabulary | text, value, visible, hidden | n/a | `is`/`get` family | open, `H1BSQJ` |
| Event-driven waits | polling on some conditions | n/a | yes | open, `FAXZ21` |
| Failure evidence bundle | no | no | no | open, `05FQQF` |
| Operation receipts for writes | no | no | no | open, `MVJT36` |
| Regression baselines | `brw_diff` against one artifact | no | no | open, `RKXE53` |
| Non-Chromium browsers | no | no | no | open, `6QWNYJ` |
| Plugin and capability model | no | no | manifest + capabilities | open, `J3YFH0` |
| Licence | AGPL-3.0 | closed | Apache-2.0 | — |

agent-browser's headline "93% fewer tokens" is measured against Playwright's MCP
server, not against `brw`. The two projects reach the same conclusion from the
same premise: an accessibility-derived snapshot with stable refs costs far less
than screenshots or raw DOM.

### What the browser itself constrains

Two platform facts sit under every row above, and explain gaps that otherwise
read as missing work:

- Chrome 136 stopped honouring `--remote-debugging-port` against the default
  user-data directory. Attackers were using it to lift cookies once App-Bound
  Encryption closed the older path. A real signed-in browser is therefore
  reachable only through an extension holding `chrome.debugger`, or through the
  Chrome 144+ opt-in at `chrome://inspect/#remote-debugging`.
- An extension cannot attach `chrome.debugger` to the BROWSER target. Documented
  in *Chrowned by an Extension* (EuroS&P 2023) and confirmed here: live Chromium
  answers `Browser.setDownloadBehavior` with "Cannot not access browser-level
  commands".

Together they account for `brw`'s incognito, HttpOnly-cookie and
deterministic-download gaps on the extension bridge. Neither is fixable in the
extension, which is why the direct-CDP lane exists and why the Chrome 144+
opt-in ships as a third transport rather than as a connection convenience:
`brwd --chrome-opt-in`, reported by `brw_identity` as `chrome-opt-in-cdp`, with
its own tool catalogue. See [install.md](install.md#the-chrome-opt-in-lane).

### The wider field

| Project | Primitive | Locus | Licence | What is worth taking |
| --- | --- | --- | --- | --- |
| Stagehand v4 | `observe()` returns candidates carrying a selector, replayed deterministically by Playwright | extension next to the page; SDKs are thin RPC clients | MIT | Pruning against the live accessibility tree rather than a serialized copy (`G8MDRX`); pop-ups blocked before load, not closed after |
| Chrome DevTools MCP | `take_snapshot` yields uid handles; every action takes a uid | direct CDP, `--autoConnect` via the Chrome 144+ opt-in | Apache-2.0 | `includeSnapshot` defaults to false, so the model asks for a snapshot (`GD6DR1`); 13 heap-snapshot tools and 3 performance-trace tools nobody else has |
| browser-use | integer index over a DOM + accessibility element list | local Chromium, real profile, or cloud | MIT | Event-driven CDP; cloud profile sync transfers cookies only, not localStorage or IndexedDB, and their README says so |
| Steel.dev | none of its own — bring Playwright or Puppeteer | self-hostable session infrastructure | Apache-2.0 | Custom extension loading, proxy rotation, session-debug UI; runs the public benchmark aggregator |
| Agent Browser Protocol | REST on a fixed loopback port, 18 embedded MCP tools | a Chromium fork with the server compiled in | BSD-3 | Freezes JavaScript execution *and virtual time* between actions, so the agent only ever acts on a stable world. An extension can pause the debugger but cannot freeze `Date.now()` or the compositor; only a fork can |

`brw`'s adaptive post-action settle is the achievable approximation of that last
one. Knowing where the ceiling is set by a fork is worth more than the protocol
comparison: no single protocol is consolidating, ABP has no second
implementation, and WebMCP (`navigator.modelContext`, origin trial in Chrome
149) is the only cross-vendor surface — `brw`, Chrome DevTools MCP, Stagehand v4
and agent-browser all added it within about four months of each other.

The trend that bears on a 74-tool MCP surface is not protocol consolidation.
Microsoft and Vercel both now steer coding agents toward a CLI plus Skills, on
the stated grounds that MCP forces tool schemas and accessibility trees into
model context; `@playwright/cli` takes bare refs (`playwright-cli check e21`)
and ships `install --skills`. `brw`'s `auto` tool-profile growth and
`--print-system-prompt` attack the same cost from the other side. The per-action
CLI (`2AR4JB`) and daemon-served skills (`JDK64N`) are the second front door.

### Benchmarks worth quoting

Agents score around 70% on sandboxed Online-Mind2Web and 33.3% on ClawBench —
153 everyday tasks across 144 live production sites, scored by intercepting the
final submission request. The sandbox-to-live gap is the product.

Stagehand's 65.0% on Online-Mind2Web is independently verified, and is the only
top-ten row that is both independent and from a framework rather than a
foundation model. That is the Stagehand number to carry.

Vendor numbers that should not go in a matrix: Stagehand's "2x faster, ~80% more
token-efficient" rests on one 50-action crawl the authors themselves label a
single run rather than a benchmark; browser-use's 97.0% Online-Mind2Web is
self-reported. agent-browser's 93% token reduction is measured against Playwright
MCP. `brw`'s own head-to-head numbers against Claude-in-Chrome were internal and
unpublished; they have been deleted from [benchmarks](benchmarks.md) rather than
restated, and that page lists each one that went.

What replaced them measures `brw` against fixtures in this repository and
compares `brw` with nothing: `task bench` records per-command wall time, CDP
round trips, transport bytes and observation tokens with the environment
fingerprint that says whether two runs are comparable, and `task agent-eval`
grades four agent-level tasks on the end state read out of the page.
`task agent-eval-verify` runs the same four with the decisive act removed and
requires every one of them to be graded a failure, which is what says the
evaluation can fail at all. Neither measurement is in `go test ./...` or
`task check`; one evaluation task runs there in both modes as the guard that the
grading can fail.

### Deliberate non-goals

These are absent by decision, not by omission, and will not be closed by a
parity argument alone:

- **Scheduling.** An external scheduler decides *when* to run. `brw`
  deterministically controls one authorized browser and returns bounded
  evidence. What `brw` owes a scheduler is a stable non-interactive contract:
  exit codes, structured output, and per-profile run serialization.
- **A secret store.** Credentials reach the browser through a provider
  capability at dispatch and are never persisted by the daemon.
- **Bulk cookie and storage export.** The signed-in extension transport
  denylists it. State save/restore, when it lands, stays on the browser host,
  scoped per origin, redacted and expiring.
- **Fleet and session orchestration.** One daemon drives one authorized browser.
- **Cross-device session continuity.** The answer is to keep the browser on the
  machine that owns it and reach it over SSH, not to replicate a session.

### Library and protocol comparison

- Stagehand caches inferred actions locally or server-side so repeat calls can
  avoid another model invocation. `brw` recipes take the stronger route for a
  promoted workflow: an explicit reviewed program, immutable identity/digest,
  exact-origin guard, semantic re-resolution, and no model during execution.
  Stagehand's cache remains a useful pattern for a future trace-to-recipe
  authoring loop. See
  [Stagehand caching](https://docs.stagehand.dev/v3/best-practices/caching).
- Playwright waits for uniqueness, visibility, stability, hit targeting,
  enabled state, and editability. `brw` already implements the important click
  actionability checks plus semantic uniqueness and transport-specific adaptive
  settling, and recipes now carry the same deterministic assertion vocabulary as
  the tool surface. See
  [Playwright actionability](https://playwright.dev/docs/actionability).
- Playwright traces combine actions, DOM snapshots, screenshots, and network
  activity into a time-travel debugging bundle. `brw_trace` is scoped and can
  produce guarded batch replay, but it is not yet a single failure artifact
  with those correlated views. See
  [Playwright tracing](https://playwright.dev/docs/api/class-tracing).
- Playwright can record and replay HAR files and intercept HTTP/WebSocket
  traffic. `brw` intentionally redacts captured headers and can replay one
  request, but cannot yet create or apply a deterministic network fixture. See
  [Playwright network mocking](https://playwright.dev/docs/mock).
- Playwright supports isolated contexts, reusable storage state, locale,
  timezone, geolocation, permissions, and visual/ARIA baselines. `brw` has
  incognito contexts and device emulation, while its principal differentiator is
  safe control of an existing signed-in profile. Bulk cookie/storage export is
  deliberately forbidden. See
  [Playwright authentication](https://playwright.dev/docs/auth),
  [emulation](https://playwright.dev/docs/next/emulation), and
  [ARIA snapshots](https://playwright.dev/docs/aria-snapshots).
- WebDriver BiDi provides a standards-based cross-browser event stream. `brw`
  currently uses Chromium/CDP or its Chrome extension bridge and polls some
  conditions. A BiDi backend would broaden browser coverage and make event
  subscriptions cheaper. See
  [Selenium WebDriver BiDi](https://www.selenium.dev/documentation/webdriver/bidi/).
- Puppeteer exposes native page screencasting. `brw` now builds its bounded WebM
  artifacts from `Page.startScreencast` frames with `Page.screencastFrameAck`
  backpressure, keeping the screenshot loop only for transports with no
  compositor stream (the extension bridge, and the locked-session print-renderer
  case). Over a 20s capture at 5fps of a page repainting twice a second the
  screenshot loop moved 277 KB against the compositor stream's 98 KB. The
  round-trip figures — 100 against 43 — are counted rather than observed on the
  wire: one capture call per tick on one side, start plus stop plus one ack per
  compositor frame on the other, so each is a floor for its own path. CPU is not
  compared: the daemon-process figure excludes the Chrome process doing the
  compositing, which is where the work the screencast saves actually moves. See
  [Puppeteer screencast](https://pptr.dev/api/puppeteer.page.screencast).

## Prioritized next level

1. **Trace-to-recipe authoring.** Compile a successful scoped trace into a
   private draft with semantic targets, explicit runtime input placeholders,
   inferred postconditions, and a human review gate. Never auto-promote a write,
   secret value, pixel coordinate, ambiguous selector, or cross-origin step.
   Publish through a provider-owned write API; do not write it into this repo.
2. **Failure evidence bundles.** On opt-in recipe failure, capture a correlated
   manifest containing redacted action trace, console summary, bounded network
   metadata, semantic snapshot, and screenshot artifact IDs. Keep payloads as
   separate expiring artifacts and make capture-on-failure configurable because
   traces can be expensive and sensitive.
3. **Provider-backed operation receipts.** For high-consequence writes, let the
   private provider record hashed idempotency keys and reviewed completion
   evidence across daemon restarts. Prefer the site's native idempotency API
   where one exists; never mistake a local receipt for proof that the remote
   transaction committed.
4. **Event stream and network fixtures.** Replace polling with one scoped event
   subscription where transports support it; then add privacy-filtered HAR
   capture/replay and request interception as explicitly opt-in capabilities.
5. **Artifact efficiency.** Use CDP stream mode for PDFs/downloads where
   available, content-addressed deduplication under opaque per-capture handles,
   optional at-rest encryption, and a manifest artifact that links related
   captures without inlining them. The download half of this was since measured
   and declined: the only CDP stream for a download's bytes is
   `Fetch.takeResponseBodyAsStream`, and taking it cancels the download the rest
   of brw tracks. See "A rendered PDF is pulled from the browser as a stream" in
   [recipes and artifacts](recipes-and-artifacts.md).
6. **Environment profiles.** Add controlled locale/timezone/media/permission
   emulation for direct-CDP contexts. Any auth-state import/export must remain a
   separately permissioned feature and must not weaken the cookie/storage
   denylist of the signed-in extension bridge.
7. **Cross-browser backend.** Prototype WebDriver BiDi after its required event,
   actionability, download, and artifact primitives are stable enough to retain
   the same fail-closed recipe guarantees.
8. **Regression comparison.** Add opt-in visual and ARIA artifact comparisons
   with environment fingerprints and tolerances. Shipped, including the
   destination: a baseline for a recipe the private provider owns is stored with
   that provider rather than in the local root.

Features intentionally left outside `brw`: long-running scheduling, a hosted
vector database, secret resolution, human approval policy, notifications to
third-party systems, and a fleet/session orchestrator. Those systems decide
*when* and *whether* to run; `brw` deterministically controls one authorized
browser and returns bounded evidence.
