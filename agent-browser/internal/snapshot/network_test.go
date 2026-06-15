package snapshot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// fixturePage issues a fetch on load so browser_network_capture has something to
// record. The fetched URL is same-origin (a data: URL cannot fetch cross-origin
// reliably under headless), so we fetch a data: URL that resolves immediately.
const networkFixture = `data:text/html,` +
	`<html><body><h1>net fixture</h1>` +
	`<script>` +
	`window.__done=false;` +
	`fetch('data:application/json,{"hello":"world"}')` +
	`.then(function(r){return r.text();})` +
	`.then(function(){window.__done=true;});` +
	`</script>` +
	`</body></html>`

func newHeadlessCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Headless,
		chromedp.DisableGPU,
		chromedp.NoSandbox,
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	ctx, ctxCancel := chromedp.NewContext(allocCtx)
	// Probe on ctx directly: if no Chrome/Chromium is available in this
	// environment, skip rather than fail — these are integration tests that
	// require a real browser binary. The browser is bound to the first context it
	// runs on, so we must NOT run the probe on a sub-context we then cancel.
	if err := chromedp.Run(ctx); err != nil {
		ctxCancel()
		allocCancel()
		t.Skipf("headless chrome unavailable: %v", err)
	}
	cancel := func() {
		ctxCancel()
		allocCancel()
	}
	return ctx, cancel
}

func TestNetworkCaptureRecordsFetch(t *testing.T) {
	ctx, cancel := newHeadlessCtx(t)
	defer cancel()

	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()

	// Install the interceptor BEFORE navigating so the page's own fetch is wrapped.
	if err := chromedp.Run(runCtx,
		chromedp.Navigate("about:blank"),
	); err != nil {
		t.Fatalf("navigate about:blank: %v", err)
	}
	if err := InstallNetworkCapture(runCtx); err != nil {
		t.Fatalf("install before nav: %v", err)
	}

	// Now drive a fetch explicitly from the page so capture is deterministic.
	if err := chromedp.Run(runCtx, chromedp.Navigate(networkFixture)); err != nil {
		t.Fatalf("navigate fixture: %v", err)
	}
	// Reinstall after navigation (new document) and trigger an explicit fetch.
	if err := InstallNetworkCapture(runCtx); err != nil {
		t.Fatalf("install after nav: %v", err)
	}
	var ignored any
	if err := chromedp.Run(runCtx, chromedp.Evaluate(
		`(function(){ return fetch('data:application/json,{"k":1}').then(function(r){return r.text();}); })()`,
		&ignored,
		awaitPromise,
	)); err != nil {
		t.Fatalf("trigger fetch: %v", err)
	}
	// Give the response handler a tick to populate status/snippet.
	if err := chromedp.Run(runCtx, chromedp.Sleep(200*time.Millisecond)); err != nil {
		t.Fatalf("settle: %v", err)
	}

	requests, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if len(requests) == 0 {
		t.Fatal("expected at least one captured request, got none")
	}
	var found *CapturedRequest
	for i := range requests {
		if strings.Contains(requests[i].URL, "data:application/json") {
			found = &requests[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("captured requests do not include the fetched URL: %+v", requests)
	}
	if found.Method != "GET" {
		t.Fatalf("method = %q, want GET", found.Method)
	}
	if found.Status != 200 {
		t.Fatalf("status = %d, want 200", found.Status)
	}
	if found.Transport != "fetch" {
		t.Fatalf("transport = %q, want fetch", found.Transport)
	}

	// Draining a second time should return an empty buffer (drain clears it).
	again, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatalf("second capture: %v", err)
	}
	for _, r := range again {
		if strings.Contains(r.URL, "data:application/json,{%22k%22:1}") || strings.Contains(r.URL, `data:application/json,{"k":1}`) {
			t.Fatalf("buffer was not drained: %+v", again)
		}
	}
}

func TestReplaySafeGetReturnsStatus(t *testing.T) {
	ctx, cancel := newHeadlessCtx(t)
	defer cancel()

	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()

	if err := chromedp.Run(runCtx, chromedp.Navigate(networkFixture)); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	result, err := ReplayRequest(runCtx, "GET", `data:application/json,{"ok":true}`, nil, "")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !result.OK || result.Status != 200 {
		t.Fatalf("replay result = %+v, want ok status 200", result)
	}
	if !strings.Contains(result.Body, "ok") {
		t.Fatalf("replay body = %q, want it to contain the response", result.Body)
	}
}

// awaitPromise lets the explicit-fetch evaluation await its promise.
func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}
