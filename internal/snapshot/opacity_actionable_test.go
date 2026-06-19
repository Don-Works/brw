package snapshot_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// opacityCheckboxPage models the TodoMVC `.toggle` pattern: a real, hit-testable
// checkbox hidden from view with opacity:0 (a styled label/cross paints the
// visual state elsewhere). opacity:0 elements still have layout and still receive
// pointer events, so the control is genuinely clickable — the AX heuristic just
// reports it visible:false. A truly removed control (visibility:hidden) sits
// alongside it as the negative control.
const opacityCheckboxPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>opacity checkbox</title></head>
<body style="margin:0;padding:0">
  <input id="toggle" type="checkbox"
         style="opacity:0;position:absolute;left:20px;top:20px;width:40px;height:40px;margin:0">
  <input id="vhidden" type="checkbox"
         style="visibility:hidden;position:absolute;left:20px;top:100px;width:40px;height:40px;margin:0">
</body></html>`

// TestWaitForActionableAcceptsOpacityZeroControl proves the SPA-actuation fix: an
// opacity:0 form control that is geometrically present and the topmost hit target
// must be actionable via the geometry hit-test path (mode "hit_test"). This is the
// case (TodoMVC's completion checkbox) that previously returned "not actionable"
// and forced agents into coordinate/evaluate fallbacks. The visibility:hidden
// control is the negative boundary the fix must NOT loosen.
func TestWaitForActionableAcceptsOpacityZeroControl(t *testing.T) {
	srv := serveHTML(t, opacityCheckboxPage)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all", IncludeHidden: true})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	var toggleRef, vhiddenRef string
	for _, el := range snap.Elements {
		if el.Tag != "input" || el.Type != "checkbox" {
			continue
		}
		switch evalString(t, ctx, `document.querySelector('[data-brw-ref="`+el.Ref+`"]').id`) {
		case "toggle":
			toggleRef = el.Ref
		case "vhidden":
			vhiddenRef = el.Ref
		}
	}
	if toggleRef == "" {
		t.Fatalf("opacity:0 checkbox not enumerated (elements=%d)", len(snap.Elements))
	}

	start := time.Now()
	res, err := snapshot.WaitForActionableResult(ctx, toggleRef, 5000)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("WaitForActionableResult(toggle): %v", err)
	}
	if !res.OK {
		t.Fatalf("opacity:0 checkbox refused (reason=%q); opacity:0 controls still receive pointer events and must be actionable", res.Reason)
	}
	if res.Mode != "hit_test" {
		t.Fatalf("expected mode=hit_test for opacity:0 control, got %q", res.Mode)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("opacity:0 actionability took %v; should resolve promptly via hit-test", elapsed)
	}

	// Negative boundary: visibility:hidden genuinely cannot receive pointer events
	// and must remain non-actionable.
	if vhiddenRef != "" {
		res, err := snapshot.WaitForActionableResult(ctx, vhiddenRef, 1500)
		if err != nil {
			t.Fatalf("WaitForActionableResult(vhidden): %v", err)
		}
		if res.OK {
			t.Fatalf("visibility:hidden checkbox must NOT be actionable; the fix must only cover opacity:0")
		}
	}
}

// evalString evaluates a JS expression in the page and returns its string value.
func evalString(t *testing.T, ctx context.Context, expr string) string {
	t.Helper()
	var out string
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		obj, exception, err := runtime.Evaluate(expr).WithReturnByValue(true).Do(ctx)
		if err != nil {
			return err
		}
		if exception != nil || obj == nil || len(obj.Value) == 0 {
			return nil
		}
		return json.Unmarshal(obj.Value, &out)
	})); err != nil {
		t.Fatalf("eval %q: %v", expr, err)
	}
	return out
}
