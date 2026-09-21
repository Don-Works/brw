package snapshot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// TestWaitForLoadIsNotAnAliasForReady pins the difference brw_wait_for and
// docs/waiting.md advertise between the two readiness conditions: a document is
// interactive — and satisfies "ready" — as soon as it has parsed, while "load" is
// the load event, which does not fire until its subresources have arrived.
//
// The fixture holds one image open, so the two conditions cannot both be true at
// once: whichever condition returns, the document's own readyState says whether
// the answer was honest. Folding "load" into "ready", which is what this script
// used to do on every transport that runs it (the extension bridge always, and
// direct CDP whenever no lifecycle event was observed), reports a loaded page
// while its resources are still in flight.
func TestWaitForLoadIsNotAnAliasForReady(t *testing.T) {
	const holdSubresource = 900 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow-image" {
			// The load event cannot fire until this response completes.
			time.Sleep(holdSubresource)
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("fixture-bytes"))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><html><body><p id="marker">fixture</p><img src="/slow-image"></body></html>`)
	}))
	t.Cleanup(srv.Close)

	tests := []struct {
		name           string
		condition      string
		wantReadyState string
	}{
		{
			name:           "ready is satisfied while subresources are still loading",
			condition:      "ready",
			wantReadyState: "interactive",
		},
		{
			name:           "load waits for the load event, not for interactive",
			condition:      "load",
			wantReadyState: "complete",
		},
		{
			name:           "page_ready is the documented alias of ready",
			condition:      "page_ready",
			wantReadyState: "interactive",
		},
	}

	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// page.Navigate returns when the navigation commits, not when the
			// document loads; chromedp.Navigate would wait out the load event
			// itself and leave nothing for the wait under test to observe.
			if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
				_, _, _, _, err := page.Navigate(srv.URL).Do(ctx)
				return err
			})); err != nil {
				t.Fatalf("navigate: %v", err)
			}

			started := time.Now()
			matched, err := WaitForCondition(ctx, tt.condition, 5000)
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("WaitForCondition(%q): %v", tt.condition, err)
			}
			if !matched {
				t.Fatalf("WaitForCondition(%q) timed out after %s", tt.condition, elapsed)
			}

			var readyState string
			if err := chromedp.Run(ctx, chromedp.Evaluate(`document.readyState`, &readyState)); err != nil {
				t.Fatalf("read document.readyState: %v", err)
			}
			if readyState != tt.wantReadyState {
				t.Fatalf("wait for %q returned after %s with document.readyState = %q, want %q",
					tt.condition, elapsed, readyState, tt.wantReadyState)
			}
		})
	}
}

// networkidle used to fall through to the plain-text form and wait for the
// literal word to appear on the page, which ran the whole timeout on every
// site. It is now the page's own quiet signal: load fired and no resource has
// finished for 500 ms. Resource Timing records completions, so a request that
// finishes inside the window pushes idle back; one still in flight after 500 ms
// of silence is not seen, which docs/waiting.md states.
func TestWaitForNetworkIdleWaitsForLateResourcesToFinish(t *testing.T) {
	const holdResource = 300 * time.Millisecond
	const idleWindow = 500 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/late" {
			time.Sleep(holdResource)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"late":true}`)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		// The fetch starts after load, so load alone does not cover it.
		fmt.Fprint(w, `<!doctype html><html><body><p>fixture</p><script>
window.addEventListener('load', function(){ fetch('/late').then(function(r){ return r.text(); }).then(function(){ document.body.dataset.late = 'done'; }); });
</script></body></html>`)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := newHeadlessSettleCtx(t)
	defer cancel()
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, _, _, err := page.Navigate(srv.URL).Do(ctx)
		return err
	})); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	started := time.Now()
	matched, err := WaitForCondition(ctx, "networkidle", 6000)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("WaitForCondition(networkidle): %v", err)
	}
	if !matched {
		t.Fatalf("WaitForCondition(networkidle) timed out after %s", elapsed)
	}
	var late string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.body.dataset.late || ''`, &late)); err != nil {
		t.Fatalf("read the late marker: %v", err)
	}
	if late != "done" {
		t.Fatalf("networkidle resolved after %s while the late fetch was still in flight", elapsed)
	}
	if elapsed < holdResource+idleWindow-100*time.Millisecond {
		t.Fatalf("networkidle resolved after %s, before the late fetch (%s) plus the idle window (%s)", elapsed, holdResource, idleWindow)
	}
}
