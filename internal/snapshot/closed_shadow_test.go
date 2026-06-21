package snapshot_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/chromedp/chromedp"
)

// TestClosedShadowRootPiercedInWalker proves the in-walker installer
// (__abEnsureShadowPierce) lets the snapshot descend into a CLOSED shadow root
// that is attached AFTER the first snapshot — the common SPA case where a web
// component mounts after initial load. The page's own el.shadowRoot stays null;
// only the brw walker (via the __brwShadow side reference) sees inside.
func TestClosedShadowRootPiercedInWalker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body style="margin:0"><div id="host"></div></body></html>`))
	}))
	defer srv.Close()

	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL),
		chromedp.WaitVisible("#host", chromedp.ByID),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	// First snapshot installs the attachShadow patch (no shadow exists yet).
	if _, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"}); err != nil {
		t.Fatalf("priming snapshot: %v", err)
	}

	// Now attach a CLOSED shadow root and mount a control inside it.
	var closedConfirmed bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){
		var h = document.getElementById('host');
		var r = h.attachShadow({mode:'closed'});
		r.innerHTML = '<button id="cb">Closed Shadow Button</button>';
		return h.shadowRoot === null; // page-visible shadowRoot must remain null (still closed)
	})()`, &closedConfirmed)); err != nil {
		t.Fatalf("attach closed shadow: %v", err)
	}
	if !closedConfirmed {
		t.Fatalf("host.shadowRoot was not null — the root was not actually closed; test is invalid")
	}

	// Second snapshot must now surface the control inside the closed root.
	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot after closed shadow mount: %v", err)
	}
	btn := findByName(snap.Elements, "Closed Shadow Button")
	if btn == nil {
		t.Fatalf("closed-shadow button not surfaced; got %v", names(snap.Elements))
	}
	if btn.Role != "button" {
		t.Fatalf("expected closed-shadow element role button, got %q", btn.Role)
	}
}

// TestClosedShadowRootPiercedAtDocumentStart proves the document-start installer
// (RegisterShadowPierceOnNewDocument, direct-CDP) captures a closed shadow root
// that is attached during initial page load — before any snapshot runs — because
// the patch is injected ahead of the page's own scripts.
func TestClosedShadowRootPiercedAtDocumentStart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><body style="margin:0">
<div id="host"></div>
<script>
  var h = document.getElementById('host');
  var r = h.attachShadow({mode:'closed'});
  r.innerHTML = '<button id="cb">Early Closed Shadow Button</button>';
</script>
</body></html>`))
	}))
	defer srv.Close()

	browserCtx, cancel := newHeadlessChrome(t)
	defer cancel()
	ctx, ctxCancel := context.WithTimeout(browserCtx, 30*time.Second)
	defer ctxCancel()

	// Arm the document-start patch BEFORE navigating, so it runs ahead of the
	// page's inline attachShadow call on the very next document.
	if err := snapshot.RegisterShadowPierceOnNewDocument(ctx); err != nil {
		t.Skipf("document-start registration unavailable in this harness: %v", err)
	}

	if err := chromedp.Run(ctx,
		chromedp.Navigate(srv.URL),
		chromedp.WaitVisible("#host", chromedp.ByID),
		chromedp.Sleep(150*time.Millisecond),
	); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	snap, err := snapshot.EvaluateWithOptions(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if findByName(snap.Elements, "Early Closed Shadow Button") == nil {
		t.Fatalf("early closed-shadow button not surfaced via document-start pierce; got %v", names(snap.Elements))
	}
}
