# Changelog

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
