package snapshot_test

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/revitt/agent-browser/internal/snapshot"
)

const annotatePage = `<!doctype html>
<html><head><meta charset="utf-8"><title>annotate</title></head>
<body style="margin:0;padding:0">
  <button id="b1" style="position:absolute;left:20px;top:30px;width:120px;height:40px">Submit</button>
  <a id="l1" href="/next" style="position:absolute;left:200px;top:30px">Next page</a>
  <input id="t1" type="text" style="position:absolute;left:20px;top:120px;width:200px">
</body></html>`

func TestAnnotationOverlayInjectsLegendAndRemovesCleanly(t *testing.T) {
	srv := serveHTML(t, annotatePage)
	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx, chromedp.Navigate(srv.URL), chromedp.WaitReady("body")); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// Authoritative snapshot mints the refs that become the overlay labels.
	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "frontier"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	marks := make([]snapshot.AnnotationMark, 0, len(snap.Elements))
	refSet := map[string]bool{}
	for _, el := range snap.Elements {
		if !el.InViewport {
			continue
		}
		marks = append(marks, snapshot.AnnotationMark{Ref: el.Ref, Name: el.Name, Role: el.Role})
		refSet[el.Ref] = true
	}
	if len(marks) == 0 {
		t.Fatal("expected at least one frontier element to annotate")
	}

	boxes, err := snapshot.InjectAnnotationOverlay(ctx, marks)
	if err != nil {
		t.Fatalf("inject overlay: %v", err)
	}

	okBoxes := 0
	for _, b := range boxes {
		if !b.OK {
			continue
		}
		okBoxes++
		if !refSet[b.Ref] {
			t.Fatalf("legend ref %q is not one of the snapshot refs", b.Ref)
		}
		if b.Width <= 0 || b.Height <= 0 {
			t.Fatalf("legend box for %q has non-positive size: %#v", b.Ref, b)
		}
	}
	if okBoxes == 0 {
		t.Fatal("expected at least one painted overlay box")
	}

	// Overlay nodes must actually exist before removal.
	var injectedCount int
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`document.querySelectorAll('[data-agent-browser-annotation]').length`, &injectedCount,
	)); err != nil {
		t.Fatalf("count overlay nodes: %v", err)
	}
	if injectedCount == 0 {
		t.Fatal("expected overlay nodes in the DOM after injection")
	}

	removed, err := snapshot.RemoveAnnotationOverlay(ctx)
	if err != nil {
		t.Fatalf("remove overlay: %v", err)
	}
	if removed == 0 {
		t.Fatal("RemoveAnnotationOverlay reported 0 nodes removed")
	}

	var remaining int
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`document.querySelectorAll('[data-agent-browser-annotation]').length`, &remaining,
	)); err != nil {
		t.Fatalf("count overlay nodes post-remove: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("overlay nodes still present after removal: %d", remaining)
	}
}
