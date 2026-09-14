package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/chromedp/chromedp"
)

// frameScopeHelpers is the second half of FrameWalkHelpers: it resolves an
// <iframe> by ref or CSS selector using the frame walk already performed by
// __abRootsCompute, and narrows the walker's root list to that frame.
//
// The scope lives on the PAGE (window.__brwFrameScope), not in the daemon, for
// two reasons. It works identically over both transports, because every walker
// script already runs in the page. And it is per-document, so a navigation drops
// it — a frame switch that silently outlived the page it was made against would
// scope a later snapshot to a frame that no longer exists.
//
// An unresolvable scope throws rather than falling back to the whole document:
// answering a scoped question from the wrong frame is the failure mode a switch
// exists to prevent.
const frameScopeHelpers = `
  function __abFrameScopeTarget() {
    try { return (window.__brwFrameScope || '') + ''; } catch (e) { return ''; }
  }
  // __abFrameElementIn finds an <iframe>/<frame> element by brw ref or CSS
  // selector. It searches the entries it is GIVEN rather than calling __abRoots,
  // which is what lets the scope filter use it without recursing.
  function __abFrameElementIn(target, entries) {
    if (!target) return null;
    var escaped = (window.CSS && CSS.escape) ? CSS.escape(target) : target;
    var refSelector = '[data-brw-ref="' + escaped + '"]';
    for (var pass = 0; pass < 2; pass++) {
      for (var i = 0; i < entries.length; i++) {
        var root = entries[i].root;
        if (!root || !root.querySelectorAll) continue;
        var found = null;
        try { found = root.querySelector(pass === 0 ? refSelector : target); } catch (e) { found = null; }
        if (!found) continue;
        var tag = String(found.tagName || '').toLowerCase();
        if (tag === 'iframe' || tag === 'frame') return found;
        // A ref that names an element INSIDE a frame still identifies the frame
        // that contains it, which is what an agent holding a snapshot ref has.
        var owner = __abFrameOf(found);
        if (owner) return owner;
      }
    }
    return null;
  }
  // __abFrameOf returns the frame element hosting el, or null when el belongs to
  // the top document.
  function __abFrameOf(el) {
    try {
      var doc = el.ownerDocument;
      var win = doc && doc.defaultView;
      return (win && win.frameElement) ? win.frameElement : null;
    } catch (e) { return null; }
  }
  // __abDocumentWithin reports whether root (a document or shadow root) is the
  // frame document doc, or lives in a frame nested inside it.
  function __abDocumentWithin(root, doc) {
    var owner = null;
    try { owner = (root.nodeType === 9) ? root : (root.ownerDocument || null); } catch (e) { owner = null; }
    if (!owner) return false;
    if (owner === doc) return true;
    var win = null;
    try { win = owner.defaultView; } catch (e) { return false; }
    for (var depth = 0; win && depth < MAX_FRAME_DEPTH + 1; depth++) {
      var parent = null;
      try { parent = win.parent; } catch (e) { return false; }
      if (!parent || parent === win) return false;
      try { if (parent.document === doc) return true; } catch (e) { return false; }
      win = parent;
    }
    return false;
  }
  function __abApplyFrameScope(entries) {
    var scope = __abFrameScopeTarget();
    if (!scope) return entries;
    var frame = __abFrameElementIn(scope, entries);
    var doc = null;
    try { doc = frame ? frame.contentDocument : null; } catch (e) { doc = null; }
    if (!doc) {
      throw new Error('frame scope ' + JSON.stringify(scope) + ' no longer resolves to a readable frame; switch back with frame target "main"');
    }
    var out = [];
    for (var i = 0; i < entries.length; i++) {
      if (__abDocumentWithin(entries[i].root, doc)) out.push(entries[i]);
    }
    return out;
  }
`

// FrameSwitchScript resolves a frame and makes it the active scope for every
// later element lookup on the page — snapshot, find, get, and the ref resolvers
// all read their roots through the same filter.
//
// It answers three shapes of target:
//
//   - "main" (or an empty target) clears the scope and reports the top document.
//   - "f<i>" is the ref the snapshot walker mints for a CROSS-ORIGIN iframe. Its
//     document is isolated by the browser, so there is nothing to scope into:
//     the frame is reported with its top-level box and origin, switched:false,
//     and the caller acts on it by coordinate.
//   - anything else is a brw ref or CSS selector for the frame element (or for
//     an element inside it, which identifies the same frame).
const FrameSwitchScript = `(function(target){` + FrameWalkHelpers + `
  var t = String(target == null ? '' : target).trim();
  var entries = __abRootsCompute();
  if (t === '' || t === 'main' || t === 'top') {
    try { delete window.__brwFrameScope; } catch (e) { window.__brwFrameScope = ''; }
    return {ok:true, switched:true, scope:'main', kind:'main', accessible:true,
            url: location.href, origin: location.origin};
  }
  function crossOrigin(ref, box) {
    return {ok:true, switched:false, scope: __abFrameScopeTarget() || 'main', kind:'cross_origin',
            ref: ref, accessible:false, origin: box.origin || '',
            x: box.x, y: box.y, width: box.width, height: box.height,
            note:'This frame is cross-origin: the browser isolates its DOM, so it cannot be scoped into or read as refs. Act on it with brw_click_xy at the center of the reported box (brw_screenshot first to see it).'};
  }
  var promoted = /^f(\d+)$/.exec(t);
  if (promoted) {
    var box = __abInaccessibleFrames[parseInt(promoted[1], 10)];
    if (!box) throw new Error('no cross-origin frame ' + JSON.stringify(t) + ' on this page; take a snapshot with include_frames to refresh frame refs');
    return crossOrigin(t, box);
  }
  var frame = __abFrameElementIn(t, entries);
  if (!frame) throw new Error('no frame matched ' + JSON.stringify(t) + '; pass a brw ref, a CSS selector for the iframe, or "main"');
  var rect = frame.getBoundingClientRect();
  var doc = null;
  try { doc = frame.contentDocument; } catch (e) { doc = null; }
  if (!doc) {
    var srcOrigin = '';
    try { srcOrigin = new URL(frame.src, location.href).origin; } catch (e) { srcOrigin = ''; }
    return crossOrigin(t, {origin: srcOrigin, x: Math.round(rect.left), y: Math.round(rect.top),
                           width: Math.round(rect.width), height: Math.round(rect.height)});
  }
  window.__brwFrameScope = t;
  var frameURL = '';
  var frameOrigin = '';
  try { frameURL = doc.location.href; frameOrigin = doc.location.origin; } catch (e) {}
  return {ok:true, switched:true, scope:t, kind:'frame', ref:t, accessible:true,
          url:frameURL, origin:frameOrigin,
          x: Math.round(rect.left), y: Math.round(rect.top),
          width: Math.round(rect.width), height: Math.round(rect.height),
          element_count: doc.querySelectorAll('*').length};
})`

// FrameInfo is the resolved frame a switch reports.
type FrameInfo struct {
	OK bool `json:"ok"`
	// Switched is false for a cross-origin frame: it was resolved and described,
	// but the scope was left where it was because its DOM cannot be read.
	Switched bool `json:"switched"`
	// Scope is the target later lookups are narrowed to, or "main".
	Scope        string  `json:"scope"`
	Kind         string  `json:"kind"`
	Ref          string  `json:"ref,omitempty"`
	Accessible   bool    `json:"accessible"`
	URL          string  `json:"url,omitempty"`
	Origin       string  `json:"origin,omitempty"`
	X            float64 `json:"x,omitempty"`
	Y            float64 `json:"y,omitempty"`
	Width        float64 `json:"width,omitempty"`
	Height       float64 `json:"height,omitempty"`
	ElementCount int     `json:"element_count,omitempty"`
	Note         string  `json:"note,omitempty"`
}

// BuildFrameSwitchExpression renders FrameSwitchScript for one target.
func BuildFrameSwitchExpression(target string) string {
	targetJSON, _ := json.Marshal(target)
	return fmt.Sprintf("%s(%s)", FrameSwitchScript, targetJSON)
}

// SwitchFrame resolves target and installs it as the page's frame scope.
func SwitchFrame(ctx context.Context, target string) (FrameInfo, error) {
	var info FrameInfo
	if err := chromedp.Run(ctx, chromedp.Evaluate(BuildFrameSwitchExpression(target), &info)); err != nil {
		return FrameInfo{}, err
	}
	if !info.OK {
		return info, errors.New("frame switch failed")
	}
	return info, nil
}
