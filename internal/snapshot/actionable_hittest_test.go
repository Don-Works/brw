package snapshot_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

// customComponentPage models the Decathlon size-selector shape: a custom web
// component whose interactive target is painted and hit-testable but is reported
// AX-INVISIBLE because an aria-hidden wrapper sits above it (the strict AX
// visible() heuristic bails on closest('[aria-hidden="true"]')). The element is a
// normal box in the viewport, so geometry + elementFromPoint resolve it. This is
// exactly the case that used to burn the full WaitForActionable timeout and then
// fail; the gate must now accept it via the hit-test path and report mode:hit_test.
const customComponentPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>custom component</title></head>
<body style="margin:0;padding:0">
  <div aria-hidden="true" style="position:absolute;left:20px;top:20px">
    <div role="button" tabindex="0" id="size-42"
         style="width:120px;height:48px;background:#0a0;color:#fff">Size 42</div>
  </div>
  <button id="plain" style="position:absolute;left:20px;top:120px;width:120px;height:40px">Plain</button>
</body></html>`

// TestWaitForActionableAcceptsAXInvisibleHitTestableElement proves SPEC 1: an
// element that the AX heuristic reports visible:false but that is geometrically
// present and hit-testable becomes actionable via the hit-test path (mode
// "hit_test"), and does so PROMPTLY (well under the fail-fast budget, not the full
// 5s timeout).
func TestWaitForActionableAcceptsAXInvisibleHitTestableElement(t *testing.T) {
	srv := serveHTML(t, customComponentPage)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Snapshot with include_hidden so the aria-hidden custom button is enumerated
	// and assigned a ref. It must report visible:false (the false-negative we work
	// around).
	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all", IncludeHidden: true})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var ref string
	for _, el := range snap.Elements {
		if el.Role == "button" && el.Name == "Size 42" {
			ref = el.Ref
			if el.Visible {
				t.Fatalf("fixture precondition broken: custom button reported AX-visible; expected visible:false")
			}
			break
		}
	}
	if ref == "" {
		t.Fatalf("custom component button not found in snapshot (elements=%d)", len(snap.Elements))
	}

	start := time.Now()
	res, err := snapshot.WaitForActionableResult(ctx, ref, 5000)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("WaitForActionableResult: %v", err)
	}
	if !res.OK {
		t.Fatalf("AX-invisible but hit-testable element was refused (reason=%q); the geometry hit-test path must accept it", res.Reason)
	}
	if res.Mode != "hit_test" {
		t.Fatalf("expected mode=hit_test for an AX-invisible element, got %q", res.Mode)
	}
	// Must not have burned anywhere near the full 5s timeout.
	if elapsed > 2*time.Second {
		t.Fatalf("actionable resolution took %v; the hit-test path should resolve promptly, not stall toward the timeout", elapsed)
	}
}

// TestSnapshotEmitsCoverageHintOnSparsePage proves SPEC 3 (auto-steer): a
// content-heavy page (lots of DOM / visible text) with almost no semantic
// interactive elements — the custom-component / CSR shape — must surface
// low_semantic_coverage:true plus a coverage_hint steering the agent toward an
// annotated screenshot. A well-populated page must NOT trip it.
func TestSnapshotEmitsCoverageHintOnSparsePage(t *testing.T) {
	// Build a content-heavy page: many non-interactive divs with text, but only
	// one real interactive element. The DOM walker finds a sparse semantic surface.
	body := "<div style='font-size:18px'>"
	for i := 0; i < 200; i++ {
		body += "<div>Lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod.</div>"
	}
	body += "</div><button id='only'>Only Button</button>"
	sparse := "<!doctype html><html><head><meta charset='utf-8'><title>sparse</title></head><body style='margin:0'>" + body + "</body></html>"

	srv := serveHTML(t, sparse)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "frontier"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	low, _ := snap.Metadata["low_semantic_coverage"].(bool)
	if !low {
		t.Fatalf("expected low_semantic_coverage:true on a content-heavy page with one interactive element; metadata=%v", snap.Metadata)
	}
	hint, _ := snap.Metadata["coverage_hint"].(string)
	if hint == "" {
		t.Fatalf("expected a coverage_hint steering toward annotate; got empty")
	}
	if !strings.Contains(hint, "annotate") {
		t.Fatalf("coverage_hint should mention annotate; got %q", hint)
	}
}

// TestSnapshotNoCoverageHintOnRichPage is the regression guard: a page with a
// healthy interactive surface must not report low coverage.
func TestSnapshotNoCoverageHintOnRichPage(t *testing.T) {
	body := ""
	for i := 0; i < 12; i++ {
		body += "<button>Button " + string(rune('A'+i)) + "</button> <a href='/x'>Link</a> "
	}
	rich := "<!doctype html><html><head><meta charset='utf-8'><title>rich</title></head><body style='margin:0'>" + body + "</body></html>"

	srv := serveHTML(t, rich)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "frontier"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if low, _ := snap.Metadata["low_semantic_coverage"].(bool); low {
		t.Fatalf("rich interactive page falsely reported low_semantic_coverage")
	}
}

// TestWaitForActionableFailsFastForTrulyInvisibleElement proves the fail-fast
// half of SPEC 1: an element present in the DOM but neither AX-visible NOR
// geometry-actionable (display:none) returns not-OK quickly (within the bounded
// fail-fast window), not after the full 5s.
func TestWaitForActionableFailsFastForTrulyInvisibleElement(t *testing.T) {
	const page = `<!doctype html><html><head><meta charset="utf-8"><title>hidden</title></head>
<body style="margin:0">
  <button id="gone" style="display:none">Hidden Button</button>
  <button id="seen" style="position:absolute;left:10px;top:10px">Seen</button>
</body></html>`
	srv := serveHTML(t, page)
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
	var ref string
	for _, el := range snap.Elements {
		if el.Name == "Hidden Button" {
			ref = el.Ref
			break
		}
	}
	if ref == "" {
		t.Skip("hidden button not enumerated; nothing to fail-fast on")
	}

	start := time.Now()
	res, err := snapshot.WaitForActionableResult(ctx, ref, 5000)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("WaitForActionableResult: %v", err)
	}
	if res.OK {
		t.Fatalf("display:none element must not be actionable")
	}
	// Fail-fast budget is ~400ms in-page; allow generous slack for the CDP round
	// trip but it must be far below the 5s full timeout.
	if elapsed > 2*time.Second {
		t.Fatalf("present-but-invisible element took %v to fail; fail-fast should bound this well under the 5s timeout", elapsed)
	}
}
