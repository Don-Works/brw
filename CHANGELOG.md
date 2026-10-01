# Changelog

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
