package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

// CheckController is the optional capability for setting a checkbox or radio to
// a known state. It is separate from Controller so a lane without it errors by
// name rather than falling back to a click that toggles the wrong way.
type CheckController interface {
	Check(context.Context, CheckOptions) (ActionResult, error)
}

// CheckOptions sets one checkbox or radio button to an explicit state.
type CheckOptions struct {
	// Ref is a brw element ref. Either Ref or Query is required.
	Ref string `json:"ref,omitempty"`
	// Query locates the element by accessible name/role when no ref is known.
	Query string `json:"query,omitempty"`
	Role  string `json:"role,omitempty"`
	// Checked is the state to enforce. It is a pointer so an omitted value is a
	// refusal rather than a silent uncheck.
	Checked *bool  `json:"checked,omitempty"`
	TabID   string `json:"tab_id,omitempty"`
}

// NormalizeCheck validates a check request and resolves its target.
func NormalizeCheck(opts CheckOptions) (CheckOptions, error) {
	out := opts
	out.Ref = strings.TrimSpace(opts.Ref)
	out.Query = strings.TrimSpace(opts.Query)
	out.Role = strings.TrimSpace(opts.Role)
	out.TabID = strings.TrimSpace(opts.TabID)
	if out.Ref == "" && out.Query == "" {
		return CheckOptions{}, errors.New("check needs ref or query to name the checkbox")
	}
	if out.Ref != "" && out.Query != "" {
		return CheckOptions{}, errors.New("check takes ref or query, not both")
	}
	if out.Checked == nil {
		return CheckOptions{}, errors.New("check needs checked:true or checked:false; it will not toggle blind")
	}
	return out, nil
}

// CheckExpression builds the in-page expression that enforces a boolean state on
// one element. It goes through the shared __abFindDeep lookup so a ref inside a
// frame resolves, and it drives the native checked setter followed by input and
// change events so a framework listening for them (React among them) sees the
// change rather than only the DOM property moving.
func CheckExpression(target string, wantChecked bool) string {
	quoted, _ := json.Marshal(target)
	want, _ := json.Marshal(wantChecked)
	return `(function (target, want) {` + snapshot.FrameWalkHelpers + `
  var el = null;
  var hit = __abFindDeep(target);
  if (hit) el = hit.el;
  if (!el) { try { el = document.querySelector(target); } catch (e) { el = null; } }
  if (!el) return { ok: false, error: 'no element matches ' + JSON.stringify(target) };
  var type = (el.getAttribute && (el.getAttribute('type') || '')).toLowerCase();
  if (type !== 'checkbox' && type !== 'radio') {
    return { ok: false, error: 'element is <' + (el.tagName || '').toLowerCase() + ' type=' + type + '>, not a checkbox or radio' };
  }
  var before = !!el.checked;
  if (before !== want) {
    var proto = el instanceof HTMLInputElement ? HTMLInputElement.prototype : null;
    var desc = proto && Object.getOwnPropertyDescriptor(proto, 'checked');
    if (desc && desc.set) { desc.set.call(el, want); } else { el.checked = want; }
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
  }
  return { ok: true, checked: !!el.checked, changed: before !== want,
    tag: (el.tagName || '').toLowerCase(), name: (el.getAttribute('name') || undefined) };
})(` + string(quoted) + `, ` + string(want) + `)`
}

// Check sets a checkbox or radio button to an explicit state.
func (m *Manager) Check(ctx context.Context, opts CheckOptions) (ActionResult, error) {
	req, err := NormalizeCheck(opts)
	if err != nil {
		return ActionResult{}, err
	}
	// Refuse a cross-origin ref before any browser I/O, then again on the ref a
	// query resolves to below.
	if err := GuardCrossOriginRefs("check", GenericCrossOriginRemedy, req.Ref); err != nil {
		return ActionResult{}, err
	}
	if err := m.guardTakeover("check"); err != nil {
		return ActionResult{}, err
	}
	start := time.Now()
	tabID, tabCtx, cancel, err := m.contextForTab(ctx, req.TabID)
	if err != nil {
		return ActionResult{}, err
	}
	defer cancel()

	target := req.Ref
	if target == "" {
		matches, findErr := m.Find(tabCtx, snapshot.FindOptions{Query: req.Query, Role: req.Role, Limit: 2})
		if findErr != nil {
			return ActionResult{}, fmt.Errorf("locate checkbox: %w", findErr)
		}
		if len(matches.Elements) == 0 {
			return ActionResult{}, fmt.Errorf("no element matches query %q", req.Query)
		}
		if len(matches.Elements) > 1 {
			return ActionResult{}, fmt.Errorf("query %q matched %d elements; pass a ref", req.Query, len(matches.Elements))
		}
		target = matches.Elements[0].Ref
	}
	if err := GuardCrossOriginRefs("check", GenericCrossOriginRemedy, target); err != nil {
		return ActionResult{}, err
	}

	var out struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Checked bool   `json:"checked"`
		Changed bool   `json:"changed"`
		Tag     string `json:"tag"`
		Name    string `json:"name"`
	}
	before := m.cachedBefore(tabID, tabCtx)
	if err := chromedp.Run(tabCtx, chromedp.Evaluate(CheckExpression(target, *req.Checked), &out)); err != nil {
		return ActionResult{}, err
	}
	if !out.OK {
		return ActionResult{}, fmt.Errorf("check %s: %s", target, out.Error)
	}
	desc := fmt.Sprintf("set checked=%t on %s", out.Checked, target)
	if out.Changed {
		desc = fmt.Sprintf("changed checked to %t on %s", out.Checked, target)
	}
	result := m.observeActionWithBefore(tabID, tabCtx, desc, before)
	result.DurationMS = time.Since(start).Milliseconds()
	return result, nil
}
