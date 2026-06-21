package snapshot

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/chromedp/chromedp"
)

// AnnotationMark is the minimal per-element input the Set-of-Marks overlay needs:
// the stable ref (which becomes the visible label) plus role/name carried through
// to the returned legend so the caller can map a label back to a semantic target.
type AnnotationMark struct {
	Ref  string `json:"ref"`
	Name string `json:"name,omitempty"`
	Role string `json:"role,omitempty"`
}

// AnnotationBox is one entry of the legend returned by the overlay injector: the
// ref's actual top-level viewport box (the same coordinate space the overlay
// label was painted at and the CDP mouse path clicks in). OK is false when the
// ref could not be resolved/painted (offscreen, removed) and was skipped.
type AnnotationBox struct {
	Ref    string  `json:"ref"`
	OK     bool    `json:"ok"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// annotationAttr is the data attribute stamped on every injected overlay node so
// RemoveAnnotationOverlay can find and delete them all without touching anything
// else on the page.
const annotationAttr = "data-brw-annotation"

// injectAnnotationOverlayScript draws one absolutely-positioned, non-interactive
// label box per resolvable mark and returns the legend of painted boxes. It reuses
// FrameWalkHelpers' __abFindDeep so iframe/shadow-root coordinates translate to
// top-level viewport space EXACTLY like ResolveBoxScript — the labels line up with
// the boxes brw_click would target. Overlay nodes are pointer-events:none and
// carry the annotation data attribute for clean removal. Marks are NOT scrolled
// into view (a screenshot captures the current viewport), so off-screen refs are
// simply skipped and reported ok:false.
var injectAnnotationOverlayScript = `(function(marks) {` + FrameWalkHelpers + `
  marks = marks || [];
  // Remove any stale overlay first so repeated calls never stack labels.
  try {
    var prev = document.querySelectorAll('[` + annotationAttr + `]');
    for (var p = 0; p < prev.length; p++) prev[p].remove();
  } catch (_) {}

  var layer = document.createElement('div');
  layer.setAttribute('` + annotationAttr + `', 'layer');
  layer.style.cssText = 'position:fixed;left:0;top:0;width:0;height:0;margin:0;padding:0;border:0;z-index:2147483647;pointer-events:none;';
  document.documentElement.appendChild(layer);

  var legend = [];
  for (var i = 0; i < marks.length; i++) {
    var mark = marks[i];
    var ref = mark && mark.ref;
    if (!ref) continue;
    var hit = __abFindDeep(ref);
    if (!hit || !hit.el) { legend.push({ ref: ref, ok: false, x: 0, y: 0, width: 0, height: 0 }); continue; }
    var r;
    try { r = hit.el.getBoundingClientRect(); } catch (_) { r = null; }
    if (!r || r.width <= 0 || r.height <= 0) { legend.push({ ref: ref, ok: false, x: 0, y: 0, width: 0, height: 0 }); continue; }
    // Top-level viewport coordinates (frame offset added), matching ResolveBoxScript's
    // viewport_x/viewport_y space.
    var vx = r.left + hit.ox;
    var vy = r.top + hit.oy;

    var box = document.createElement('div');
    box.setAttribute('` + annotationAttr + `', 'box');
    box.style.cssText = 'position:fixed;pointer-events:none;box-sizing:border-box;' +
      'left:' + vx + 'px;top:' + vy + 'px;width:' + r.width + 'px;height:' + r.height + 'px;' +
      'border:2px solid #ff0050;background:rgba(255,0,80,0.06);';
    layer.appendChild(box);

    var label = document.createElement('div');
    label.setAttribute('` + annotationAttr + `', 'label');
    label.textContent = ref;
    // Anchor the badge to the top-left corner; clamp to viewport top so a box that
    // starts above the fold still shows its label.
    var ly = Math.max(0, vy - 14);
    label.style.cssText = 'position:fixed;pointer-events:none;left:' + Math.max(0, vx) + 'px;top:' + ly + 'px;' +
      'font:bold 11px/13px -apple-system,Arial,sans-serif;color:#fff;background:#ff0050;' +
      'padding:0 4px;border-radius:3px;white-space:nowrap;';
    layer.appendChild(label);

    legend.push({ ref: ref, ok: true, x: vx, y: vy, width: r.width, height: r.height });
  }
  return { count: legend.length, legend: legend };
})`

// removeAnnotationOverlayScript deletes every node the injector stamped with the
// annotation attribute and reports how many were removed, so callers (and tests)
// can assert the page is left byte-clean.
var removeAnnotationOverlayScript = `(function() {
  var nodes = document.querySelectorAll('[` + annotationAttr + `]');
  var n = nodes.length;
  for (var i = 0; i < nodes.length; i++) nodes[i].remove();
  return { removed: n };
})()`

// AnnotationOverlayResult is the parsed result of the injection script: the
// painted-box legend. Exported so transports that run JS over their own channel
// (the extension bridge) can decode it without re-deriving the shape.
type AnnotationOverlayResult struct {
	Count  int             `json:"count"`
	Legend []AnnotationBox `json:"legend"`
}

// InjectAnnotationOverlayExpr returns the JS expression string that paints the
// overlay for the given marks. Transports that own their own Runtime.evaluate
// channel (the extension bridge) call this and evaluate it themselves; the
// direct-CDP path uses InjectAnnotationOverlay which wraps chromedp.
func InjectAnnotationOverlayExpr(marks []AnnotationMark) (string, error) {
	if marks == nil {
		marks = []AnnotationMark{}
	}
	args, err := json.Marshal(marks)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s(%s)", injectAnnotationOverlayScript, args), nil
}

// RemoveAnnotationOverlayExpr returns the JS expression that deletes every
// injected overlay node, for transports running JS over their own channel.
func RemoveAnnotationOverlayExpr() string {
	return removeAnnotationOverlayScript
}

// InjectAnnotationOverlay paints a Set-of-Marks overlay (one labelled box per
// resolvable mark) and returns the painted-box legend. Coordinates are in
// top-level viewport space, matching the CDP screenshot the caller is about to
// take. The overlay must be removed with RemoveAnnotationOverlay (the manager
// defers this in every path).
func InjectAnnotationOverlay(ctx context.Context, marks []AnnotationMark) ([]AnnotationBox, error) {
	if marks == nil {
		marks = []AnnotationMark{}
	}
	args, err := json.Marshal(marks)
	if err != nil {
		return nil, err
	}
	var result struct {
		Count  int             `json:"count"`
		Legend []AnnotationBox `json:"legend"`
	}
	expr := fmt.Sprintf("%s(%s)", injectAnnotationOverlayScript, args)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &result)); err != nil {
		return nil, err
	}
	return result.Legend, nil
}

// RemoveAnnotationOverlay deletes all overlay nodes injected by
// InjectAnnotationOverlay and returns how many were removed.
func RemoveAnnotationOverlay(ctx context.Context) (int, error) {
	var result struct {
		Removed int `json:"removed"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(removeAnnotationOverlayScript, &result)); err != nil {
		return 0, err
	}
	return result.Removed, nil
}
