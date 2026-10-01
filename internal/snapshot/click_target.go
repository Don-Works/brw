package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/chromedp/chromedp"
)

// ClickTargetScript validates a ref against its painted target without dispatching input.
const ClickTargetScript = `(function(ref, pointX, pointY) {` + FrameWalkHelpers + `
  function parent(el) {
    if (el.parentElement) return el.parentElement;
    var root = el.getRootNode && el.getRootNode();
    if (root && root.host) return root.host;
    try { return el.ownerDocument.defaultView.frameElement; } catch (_) { return null; }
  }
  function deepest(root, x, y) {
    var el = root && root.elementFromPoint ? root.elementFromPoint(x, y) : null;
    if (!el) return null;
    var shadow = el.shadowRoot || el.__brwShadow;
    if (shadow && shadow !== root) {
      var inside = deepest(shadow, x, y);
      if (inside && inside !== el) return inside;
    }
    if (el.tagName === 'IFRAME') {
      try {
        var r = el.getBoundingClientRect();
        var inside = deepest(el.contentDocument, x - r.left - el.clientLeft, y - r.top - el.clientTop);
        if (inside) return inside;
      } catch (_) {}
    }
    return el;
  }
  var found = ref ? __abFindDeep(ref) : {el:deepest(document, pointX, pointY),ox:0,oy:0};
  if (!found || !found.el) return {ok:false,error:'click ref no longer exists; re-run brw_snapshot'};
  var target = found.el;
  if (target.matches(':disabled')) return {ok:false,error:'click target is disabled'};
  for (var node = target; node; node = parent(node)) {
    if ((node.tagName !== 'FIELDSET' && node.matches(':disabled')) || (node.control && node.control.matches(':disabled,[aria-disabled="true"]'))) return {ok:false,error:'click target control is disabled'};
    if (node.matches('[aria-disabled="true"],[hidden],[inert],[aria-hidden="true"]')) return {ok:false,error:'click target is disabled or hidden'};
  }
  if (target.control && target.control.matches(':disabled,[aria-disabled="true"]')) return {ok:false,error:'click target control is disabled'};
  var explicitPoint = Number.isFinite(pointX) && Number.isFinite(pointY);
  if (ref && !explicitPoint) target.scrollIntoView({block:'center',inline:'center',behavior:'instant'});
  var r = target.getBoundingClientRect();
  if (!(r.width > 0 && r.height > 0)) return {ok:false,error:'click target has no visible box'};
  var x = ref && !explicitPoint ? r.left + found.ox + r.width / 2 : pointX;
  var y = ref && !explicitPoint ? r.top + found.oy + r.height / 2 : pointY;
  var painted = deepest(document, x, y);
  var related = false;
  for (var node = painted; node; node = parent(node)) {
    if (node === target || node.control === target || target.control === node) { related = true; break; }
  }
  if (!related) return {ok:false,error:'click target not hit-testable; another element covers its center'};
  return {ok:true,x:x,y:y,tag:target.tagName.toLowerCase(),role:target.getAttribute('role')||'',name:(target.getAttribute('aria-label')||target.getAttribute('title')||target.innerText||'').trim().slice(0,100),href:target.href||target.getAttribute('href')||''};
})`

// ResolveClickTarget returns a painted click point for an enabled ref without dispatching input.
func ResolveClickTarget(ctx context.Context, ref string) (ClickXYResult, error) {
	raw, _ := json.Marshal(ref)
	var result ClickXYResult
	if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf("%s(%s)", ClickTargetScript, raw), &result)); err != nil {
		return result, err
	}
	if !result.OK {
		return result, fmt.Errorf("click %q: %s", ref, result.Error)
	}
	return result, nil
}

// ResolveClickPoint validates an explicit viewport point without dispatching input.
func ResolveClickPoint(ctx context.Context, x, y float64) (ClickXYResult, error) {
	if math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) {
		return ClickXYResult{}, fmt.Errorf("click coordinates must be finite")
	}
	var result ClickXYResult
	if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf("%s(null,%g,%g)", ClickTargetScript, x, y), &result)); err != nil {
		return result, err
	}
	if !result.OK {
		return result, fmt.Errorf("click point: %s", result.Error)
	}
	return result, nil
}

// ResolveClickTargetAtPoint validates a located element at its chosen viewport point.
func ResolveClickTargetAtPoint(ctx context.Context, ref string, x, y float64) (ClickXYResult, error) {
	raw, _ := json.Marshal(ref)
	var result ClickXYResult
	if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf("%s(%s,%g,%g)", ClickTargetScript, raw, x, y), &result)); err != nil {
		return result, err
	}
	if !result.OK {
		return result, fmt.Errorf("click %q: %s", ref, result.Error)
	}
	return result, nil
}
