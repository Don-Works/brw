package mcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Don-Works/brw/internal/browser"
)

func screenshotSaveTool() map[string]any {
	return tool("brw_screenshot_save", "Save a presentation-quality page-content screenshot to an absolute path on the browser host (never browser chrome). Creates parent directories and atomically replaces the destination. Returns path, width, height, bytes and sha256 plus a small JPEG preview; never returns the full image. Home directory only unless the host configures BRW_SCREENSHOT_ALLOW_OUTSIDE_HOME=1. Uses the compositor on CDP and extension bridge (reload an older extension); no print fallback. Waits up to 5s for fonts, visible images and video frames, then optional settle_ms and two paint frames. Region is viewport-relative CSS pixels; ref captures the exact element box without labels or padding. Output dimensions are CSS dimensions times scale, independent of screen DPR; maximum 32 megapixels. A locked or fully occluded browser can refuse capture. Off-host CDP disk saving is unavailable. For annotations use brw_screenshot annotate:true.", object(map[string]any{
		"save_path":       stringSchema("Absolute destination on the browser host; parent directories are created. Existing files are replaced atomically."),
		"tab_id":          stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		"format":          map[string]any{"type": "string", "enum": []string{"png", "jpeg", "webp"}, "default": "png"},
		"quality":         map[string]any{"type": "integer", "minimum": 0, "maximum": 100, "description": "JPEG/WebP quality, default 90. Not valid for PNG."},
		"scale":           map[string]any{"type": "integer", "enum": []int{1, 2, 3}, "default": 1, "description": "Output pixels per CSS pixel, independent of the browser's device pixel ratio."},
		"full_page":       boolSchema("Capture the full document. Mutually exclusive with ref and region."),
		"ref":             stringSchema("Element ref from brw_snapshot. Exact border box, no annotation or padding. Mutually exclusive with region/full_page."),
		"region":          object(map[string]any{"x": map[string]any{"type": "number", "minimum": 0}, "y": map[string]any{"type": "number", "minimum": 0}, "width": map[string]any{"type": "number", "exclusiveMinimum": 0}, "height": map[string]any{"type": "number", "exclusiveMinimum": 0}}, []string{"x", "y", "width", "height"}),
		"omit_background": boolSchema("Transparent default background for PNG/WebP. Explicit page CSS backgrounds remain."),
		"hide":            map[string]any{"type": "array", "items": stringSchema("CSS selector in the main document."), "maxItems": 100, "description": "Temporarily hide matching elements and descendants without changing layout; restored even on capture failure."},
		"settle_ms":       map[string]any{"type": "integer", "minimum": 0, "maximum": 10000, "description": "Additional delay after fonts/images/video readiness, before capture."},
		"preview":         map[string]any{"type": "string", "enum": []string{"none", "small", "medium"}, "default": "small", "description": "small: JPEG at most 512px long edge / 40KiB; medium: 1024px / 100KiB; none: metadata only."},
	}, []string{"save_path"}))
}

func (s *Server) screenshotSave(ctx context.Context, args json.RawMessage) (any, *rpcError) {
	var opts browser.ScreenshotSaveOptions
	if err := unmarshalArgs(args, &opts); err != nil {
		return nil, invalid(err)
	}
	if err := opts.Normalize(); err != nil {
		return toolError(err), nil
	}
	saver, ok := s.manager.(browser.ScreenshotSaver)
	if !ok {
		return toolError(errors.New("screenshot disk saving is unavailable on this transport; upgrade the browser host")), nil
	}
	result, err := saver.SaveScreenshot(ctx, opts)
	if err != nil {
		return toolError(err), nil
	}
	preview := result.Preview
	result.Preview = nil
	metadata, _ := json.Marshal(result)
	content := []toolContent{{Type: "text", Text: string(metadata)}}
	if preview != nil {
		content = append(content, toolContent{Type: "image", Data: preview.Base64, MIMEType: preview.MIMEType})
	}
	return map[string]any{"content": content, "structuredContent": result}, nil
}
