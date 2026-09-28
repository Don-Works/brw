# Presentation screenshots

`brw_screenshot_save` saves page content directly to a chosen file on the browser
host and returns a small preview for visual review. Browser tabs, toolbars and
window decorations are never included. Existing `brw_screenshot` and
`brw_screenshot_element` calls keep their context-oriented behavior, including
Set-of-Marks annotations. Artifact capture still writes to the private store.

```json
{
  "name": "brw_screenshot_save",
  "arguments": {
    "tab_id": "<tab id>",
    "save_path": "/home/<user>/project/docs/hero.png",
    "scale": 2,
    "hide": [".sidebar", ".chat-overlay"],
    "settle_ms": 250
  }
}
```

The response contains a text metadata block and the same metadata as
`structuredContent`: `path`, `width`, `height`, `bytes`, `sha256`, `mime_type`.
The SHA-256 is computed from the exact file bytes. The optional image content
block is a JPEG preview derived from those bytes, never the full-size image.

| Option | Behavior |
| --- | --- |
| `save_path` | Required absolute path. Creates parent directories and atomically replaces an existing destination after capture and preview generation succeed. Files use owner-only permissions. |
| `format` | `png` (default), `jpeg`, or `webp`. Explicit format determines the encoding, not the filename suffix. |
| `quality` | 0–100 for JPEG/WebP, default 90. Rejected for PNG. |
| `scale` | 1 (default), 2, or 3 output pixels per CSS pixel, independent of the screen's native DPR. Does not change responsive layout or device emulation. Fractional CSS dimensions round to the nearest output pixel. |
| `full_page` | Capture the full document instead of the viewport. |
| `ref` | Capture the element's exact border box, including content outside the initial viewport. No padding, labels or annotations. Use a current snapshot ref. |
| `region` | `{x,y,width,height}` in CSS pixels relative to the current viewport. Positive dimensions and nonnegative coordinates. |
| `omit_background` | Transparent default background for PNG/WebP. Explicit CSS backgrounds remain; JPEG is rejected. |
| `hide` | Main-document CSS selectors, at most 100. Matching elements and their descendants become invisible without changing layout; styles are restored after success or failure. |
| `settle_ms` | Extra delay of 0–10000 ms after resource readiness, before two paint frames and capture. |
| `preview` | `small` (default): at most 512 px on the long edge and 40 KiB. `medium`: 1024 px and 100 KiB. `none`: metadata only. |

`full_page`, `ref` and `region` are mutually exclusive. The maximum capture is
32 megapixels; use a smaller region or scale for a very long page. The preview
never upscales, flattens transparency onto white, and reduces JPEG quality (then
size if necessary) to stay within its byte budget.

The host confines paths to the account's home using a traversal-resistant
filesystem root, including checks on symlinked parent directories. To permit
other locations, explicitly start the browser host with
`BRW_SCREENSHOT_ALLOW_OUTSIDE_HOME=1`. A tool argument cannot enable this option.
With `--upstream-http`, the path and setting belong to the upstream browser host;
the proxy receives only metadata and preview bytes. Disk saving is unavailable
on `off-host-cdp`, consistent with that transport's local filesystem policy.

Both direct CDP and the extension bridge capture Chrome's compositor surface,
so canvas, WebGL and playing video remain visible. The bundled extension is 0.7.9; reload an older unpacked
extension before using the new tool. The bridge may briefly activate the target
tab within its existing window and restores the prior active tab afterward.
It never raises the OS window. A locked or fully occluded browser may fail to
produce frames; this tool reports that failure instead of substituting a PDF
render that can lose video/WebGL. DRM-protected video follows Chrome's capture
restrictions.

Capture waits up to five seconds for document fonts, relevant image decodes and
a presented video frame, followed by the extra settle delay and two animation
frames. Failed or stalled resources cause an error. This is a resource-readiness
check, not network idle or an application-specific loading condition: use
`brw_wait_for` first for charts, lazy content or animations that the application
loads later. Hidden selectors apply to the main document; content inside shadow
roots and frames needs its own page styling. Cross-origin element refs remain
subject to the existing ref restrictions; use a region for those contents.

HTTP clients use `POST /api/visual/screenshot_save` with the same JSON options and
optional `tab_id`. The response contains metadata and an optional
`preview: {mime_type, base64}`. No endpoint returns full capture bytes on this path.

The local fixture `tests/fixtures/presentation.html` continuously renders a
WebGL canvas and plays a video sourced from a canvas stream. Real-browser tests
assert pixel colors, exact scaled crop dimensions, transparency and cleanup;
the extension test loads the actual service worker in a disposable Chromium
profile and talks through the real bridge.
