package snapshot

import (
	"net/url"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestSnapshotInstallOnce verifies the walker is installed under the private
// per-process name once per document (cold path), reused on subsequent calls
// (fast path ships only the tiny call expression, not the ~30KB source), and
// re-installed cleanly after a navigation replaces the JS context.
func TestSnapshotInstallOnce(t *testing.T) {
	page1 := `<!DOCTYPE html><html><body>
<button id="a">Alpha</button><a href="/x">Link</a><input id="t">
</body></html>`

	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("data:text/html,"+url.PathEscape(page1))); err != nil {
		t.Fatal(err)
	}

	installed := func() bool {
		var ok bool
		if err := chromedp.Run(ctx, chromedp.Evaluate("typeof "+snapshotInstallTarget+" === 'function'", &ok)); err != nil {
			t.Fatalf("probe install: %v", err)
		}
		return ok
	}

	if installed() {
		t.Fatal("walker unexpectedly installed before first snapshot")
	}

	cold, err := EvaluateWithOptions(ctx, SnapshotOptions{})
	if err != nil {
		t.Fatalf("cold snapshot: %v", err)
	}
	if len(cold.Elements) == 0 {
		t.Fatal("cold snapshot returned no elements")
	}
	if !installed() {
		t.Fatal("walker not installed after cold snapshot")
	}

	hot, err := EvaluateWithOptions(ctx, SnapshotOptions{})
	if err != nil {
		t.Fatalf("fast-path snapshot: %v", err)
	}
	if len(hot.Elements) != len(cold.Elements) {
		t.Fatalf("fast-path element count %d != cold %d", len(hot.Elements), len(cold.Elements))
	}

	// A navigation replaces the JS context: the walker is gone and the next call
	// must transparently re-install rather than erroring.
	page2 := `<!DOCTYPE html><html><body><button id="b">Beta</button></body></html>`
	if err := chromedp.Run(ctx, chromedp.Navigate("data:text/html,"+url.PathEscape(page2))); err != nil {
		t.Fatal(err)
	}
	if installed() {
		t.Fatal("walker should not survive a navigation to a fresh document")
	}
	after, err := EvaluateWithOptions(ctx, SnapshotOptions{})
	if err != nil {
		t.Fatalf("post-nav snapshot: %v", err)
	}
	if len(after.Elements) == 0 {
		t.Fatal("post-nav snapshot returned no elements")
	}
}

// TestSnapshotInstallOnceResistsPageTampering proves a page cannot shadow or
// wedge snapshots: it uses a private random target name (so a page hijacking the
// legacy window.__brwSnapshot name is ignored), and the cold path overwrites the
// target unconditionally (so a colliding/non-function value cannot break it).
func TestSnapshotInstallOnceResistsPageTampering(t *testing.T) {
	html := `<!DOCTYPE html><html><body>
<button id="real">Real Button</button><a href="/y">Real Link</a>
<script>
  // A hostile page predefines the legacy fixed name with a spoofing walker...
  window.__brwSnapshot = function(){ return {elements:[{ref:'SPOOFED',role:'button',name:'evil'}]}; };
</script>
</body></html>`

	ctx, cancel := structuredTestContext(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.Navigate("data:text/html,"+url.PathEscape(html))); err != nil {
		t.Fatal(err)
	}

	// The real walker (private name) must run, not the page's spoof.
	snap, err := EvaluateWithOptions(ctx, SnapshotOptions{})
	if err != nil {
		t.Fatalf("snapshot under page tampering: %v", err)
	}
	for _, el := range snap.Elements {
		if el.Ref == "SPOOFED" {
			t.Fatalf("page spoofed the snapshot via window.__brwSnapshot: %+v", snap.Elements)
		}
	}
	if len(snap.Elements) == 0 {
		t.Fatal("expected real elements from the trusted walker")
	}

	// Now poison the actual private target with a truthy non-function and confirm
	// the cold path unconditionally reinstalls rather than wedging permanently.
	var ok bool
	if err := chromedp.Run(ctx, chromedp.Evaluate("(function(){"+snapshotInstallTarget+"=42;return true;})()", &ok)); err != nil {
		t.Fatalf("poison target: %v", err)
	}
	recovered, err := EvaluateWithOptions(ctx, SnapshotOptions{})
	if err != nil {
		t.Fatalf("snapshot after target poisoned: %v", err)
	}
	if len(recovered.Elements) == 0 {
		t.Fatal("cold path failed to reinstall over a poisoned target")
	}
}
