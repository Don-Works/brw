package snapshot

import (
	"net/url"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestSnapshotInstallOnce verifies the walker is installed on window once per
// document (cold path), reused on subsequent calls (fast path ships only the
// tiny call expression, not the ~30KB source), and re-installed cleanly after a
// navigation replaces the JS context.
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
		if err := chromedp.Run(ctx, chromedp.Evaluate("typeof window.__brwSnapshot === 'function'", &ok)); err != nil {
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
