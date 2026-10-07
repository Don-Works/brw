# Changelog

## 0.21.0 - 2026-10-06

- Add durable read-only page watchers with metadata-only change, unavailable and
  recovery events. Definitions, baselines, event sequences and bounded queues
  survive browser-host restart and disposable MCP client exit.
- Add optional periodic refresh for static pages. Watchers use private background
  tabs, pin the exact URL, and recheck navigation policy and current read grants.
  Browser connection ownership guards prevent reading or closing recycled tab IDs.
- Expose watcher registration, management and event cursors over MCP and HTTP.
  Consumers persist cursors after processing; delivery may repeat an event and
  browser sampling can miss activity between observations. Page text, titles and
  message bodies are never included in the watcher queue.
- Close proven owned tabs on graceful shutdown, discard unproven tab IDs after
  restart/reconnection, and honor explicit tab targeting in readiness waits.
- Add bearer-token auth for a browser host on a private network.
  `--http-token-file` makes every HTTP request, watcher routes and `/health`
  included, carry `Authorization: Bearer <token>`; `--upstream-token-file` or
  `BRW_UPSTREAM_TOKEN` makes an `--upstream-http` proxy send it to the upstream
  host only. A non-loopback `--http` without a token starts with a warning.
- `--timeout 0`, or a profile's `"operation_timeout": "0"`, removes the fixed
  per-operation limit (#51).
- A plan's HTTP request timeout now covers the sum of its steps' waits (#50).

## 0.20.1 - 2026-10-02

- Allow batch `assert_value` steps to check for an empty field on both direct
  CDP and the extension bridge. A cleared field now passes; a non-empty field
  still fails and stops the batch. Missing refs remain an error.
- Keep extension 0.7.11; this fix is in the daemon's batch runners.

## 0.20.0 - 2026-10-01

- Prioritize active dialog controls and discover styled native checkboxes with
  visible labels in bounded snapshots. Preserve checked state and task context.
- Deliver trusted input for default ref, text and coordinate clicks, including
  native touch under touch emulation. Validate painted targets, reject inactive
  profile tabs with an explicit focus remedy, and never replay ambiguous input.
  Verify application postconditions separately from dispatch receipts.
- Extension 0.7.11 adds matching input retry protection. Keep passive batch/plan
  steps outside human-action pacing and enforce extension wait deadlines.
- Record bounded metadata for operation outcomes, timings and input/output sizes
  across HTTP, MCP, CLI and reader boundaries. Add `brw usage` reporting; exclude
  page content, URLs and credentials. Token estimates are not complete host-model
  context or provider spend, and overlapping layers must not be added together.
- Add compact filtered tab discovery with explicit truncation. Reuse per-element
  snapshot state, push down form-role filters, and support explicit following
  readiness checks for eligible direct actions with document-identity guards.
- Improve optional reader evidence packing, completeness, cancellation and
  process cleanup. Keep direct browser use independent of a helper model.
- Expand owned-fixture differential and transport coverage, including 24 seeded
  desktop/touch × headed/headless × direct/extension flows with matching semantic
  views and final states. Measurements still show transport latency differences;
  this release does not establish human-speed navigation or universal parity.

## 0.19.0 - 2026-10-01

- Index sibling paths once per DOM walk and apply role filters before expensive
  names, geometry and paths. Measured extraction improvements range from 3.25x
  to 9.46x on three public-page role queries; these are extraction timings, not
  whole browsing task speedups. Add seeded differential mutation coverage.
- Preserve custom-element reference invalidation and report compact snapshot
  delta fallback reasons.
- Bundle optional Python reading workers and a separate stdio MCP adapter with
  one `brw_ask` tool. Return a bounded answer or excerpt and source; keep full
  pages, provider usage and phase timings in private job artifacts. Direct brw
  tools remain independent of models and classifiers.
- Make answer and classifier models, endpoints, credential environment names,
  evidence budgets and reasoning settings configurable. Support classifier off,
  shadow and selection modes; include replay measurements and routing tests.
- Update and validate the agent skill, including reader discovery, CLI fallback,
  separate MCP registration and deployment checks. The optional reader is for
  known public URLs; it does not add autonomous interactive actions, mesh job
  scheduling, Firefox support or exact-content write approvals.

## 0.18.1 - 2026-10-01

- Fix direct-CDP viewport screenshots after horizontal or vertical scrolling.
  Plain and annotated captures now use the document's scroll offset instead of
  capturing the page origin. Keep annotation legends in viewport coordinates
  and preserve element captures.

## 0.18.0 - 2026-10-01

- Add `settle_ms` to `brw_read` and `--settle-ms` to `brw read`: keep the
  default 800 ms sparse-page wait, read immediately with 0 after an explicit
  readiness check, or allow up to 5000 ms for delayed text. The budget crosses
  the HTTP proxy and works on both direct CDP and the extension bridge.
- Fix direct-CDP batch fills ignoring the advertised `value` alias and clearing
  the field. Match standalone fills and the extension bridge.
- Preserve spaces between inline HTML elements and the indentation and line
  breaks of preformatted code in no-browser reads.
- Add named `extraction_json` captures for exact sections, tables and normalized
  structured fields. Return bounded artifact handles in recipe `outputs`, with
  source provenance and explicit completeness, ambiguity and budget failures.
  Recipes with runtime secrets cannot use extraction.
- Retain up to eight snapshot delta baselines within a 2 MiB serialized budget,
  so intermediate observations do not immediately discard an explicit baseline.
  Use daemon-lifetime versions and explicit full-snapshot fallback reasons.
- Advance repeated tool searches through undisclosed matches; preserve frame
  click coordinates in compact snapshots.
- Summarize bounded UCP profile declarations during existing agent-surface
  discovery without following or invoking payment endpoints.
- Select the native WebMCP input format before dispatch and stop retrying calls
  based on error text. Test modern object input, pending calls, unregistering
  and cancellation separately.
- Reduce the initial skill by moving detailed contracts into linked references;
  document repository-owned `.brw/recipes/` files, optional registry adapters and
  gateway schema refresh requirements.

## 0.17.0 - 2026-09-30

- Run caller-supplied recipes through `brw_recipe_run` and `/api/recipes/run`
  using a `recipe` object, or `brw run --file recipe.json`. No recipe provider
  or installation is required; results identify the executed content by digest.
- Keep stored recipes and pinned ID/version/digest execution supported. Both
  sources share validation, origin checks, locking, write safeguards and evidence.
  Inline recipes are not automatically saved or added to search.

## 0.16.0 - 2026-09-29

- Add human pacing (`--pacing human|off`, `BRW_PACING`, or a profile's `"pacing"`):
  random human-length gaps between actions on a tab and typing one character at a
  time with varied key delays (long text in word chunks, within 6 seconds). On by
  default on the extension bridge, off elsewhere.
- Extension 0.7.10: keep a tab drivable when another extension's frame (a
  password manager's autofill menu) is in the page. Evaluation and text input
  fall back to `chrome.scripting` in the top frame, which needs the new
  `scripting` permission; where that is unavailable the error is a named
  `foreign_extension_frame` instead of an opaque debugger refusal.
- Add a headless lane: `brwctl setup --transport headless` creates a brw-owned,
  never-signed-in headless profile, a background daemon that keeps its browser
  warm, and an MCP server `brw-headless` that proxies to it.
- Reclaim a Chrome left behind by a daemon killed with SIGKILL instead of
  refusing to start on its locked profile.
- Open headless Chrome at 1440x900 instead of 800x600.
- Fix site consent on direct CDP, where every untargeted call failed with
  "no active tab".
- Add `brw_screenshot_save` for PNG/JPEG/WebP files on the browser host, with
  1×/2×/3× scaling, full-page/element/region captures, transparent backgrounds,
  temporary selector hiding and resource settling on CDP and the extension bridge.
- Return file dimensions, byte count and SHA-256 with a bounded JPEG preview
  (512 px / 40 KiB by default); keep full-size images out of MCP context.
- Confine screenshot destinations to the user's home by default, with an explicit
  host configuration for other paths. Preserve existing screenshot/artifact tools.
