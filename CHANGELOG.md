# Changelog

## Unreleased

- Add `brw_screenshot_save` for PNG/JPEG/WebP files on the browser host, with
  1×/2×/3× scaling, full-page/element/region captures, transparent backgrounds,
  temporary selector hiding and resource settling on CDP and the extension bridge.
- Return file dimensions, byte count and SHA-256 with a bounded JPEG preview
  (512 px / 40 KiB by default); keep full-size images out of MCP context.
- Confine screenshot destinations to the user's home by default, with an explicit
  host configuration for other paths. Preserve existing screenshot/artifact tools.
