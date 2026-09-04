package snapshot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// fixturePage issues a fetch on load so brw_network_capture has something to
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

func TestNetworkCaptureRetainsSlowFetchUntilTerminalDrain(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slow" {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body>slow network fixture</body></html>`))
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("slow response complete"))
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	ctx, cancel := newHeadlessCtx(t)
	defer cancel()
	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()
	if err := chromedp.Run(runCtx, chromedp.Navigate(srv.URL)); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := InstallNetworkCapture(runCtx); err != nil {
		t.Fatalf("install: %v", err)
	}

	var ignored any
	if err := chromedp.Run(runCtx, chromedp.Evaluate(
		`(function(){ window.__slowPromise = fetch('/slow', {headers:{Authorization:'Bearer must-not-leak'}}).then(function(r){ return r.text(); }); return true; })()`,
		&ignored,
	)); err != nil {
		t.Fatalf("start slow fetch: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow request did not reach fixture server")
	}

	first, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatalf("drain pending request: %v", err)
	}
	pending := capturedRequestContaining(first, "/slow")
	if pending == nil {
		t.Fatalf("manual drain did not report the in-flight request: %+v", first)
	}
	if pending.Completed || pending.Status != 0 || pending.Error != "" || pending.CaptureID == "" || len(pending.CaptureID) > 40 {
		t.Fatalf("pending request has invalid lifecycle state: %+v", *pending)
	}
	redacted := RedactCapturedCredentials(first)
	pending = capturedRequestContaining(redacted, "/slow")
	if pending == nil || pending.RequestHeaders["Authorization"] != "[redacted]" {
		t.Fatalf("credential redaction regressed for pending capture: %+v", pending)
	}
	pendingID := pending.CaptureID
	stillPending, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatalf("repeat pending drain: %v", err)
	}
	repeated := capturedRequestContaining(stillPending, "/slow")
	if repeated == nil || repeated.Completed || repeated.CaptureID != pendingID || repeated.Status != 0 {
		t.Fatalf("in-flight row did not survive repeated drains: %+v", repeated)
	}

	releaseOnce.Do(func() { close(release) })
	if err := chromedp.Run(runCtx, chromedp.Evaluate(`window.__slowPromise`, &ignored, awaitPromise)); err != nil {
		t.Fatalf("await slow fetch: %v", err)
	}
	terminal, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatalf("drain completed request: %v", err)
	}
	completed := capturedRequestContaining(terminal, "/slow")
	if completed == nil {
		t.Fatalf("slow request disappeared after the earlier drain: %+v", terminal)
	}
	if !completed.Completed || completed.CaptureID != pendingID || completed.Status != http.StatusOK || !completed.OK || completed.Error != "" {
		t.Fatalf("terminal lifecycle did not preserve identity/status: pending=%q completed=%+v", pendingID, *completed)
	}

	again, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatalf("third drain: %v", err)
	}
	if duplicate := capturedRequestContaining(again, "/slow"); duplicate != nil {
		t.Fatalf("terminal request was not consumed exactly once: %+v", *duplicate)
	}
}

func TestNetworkCaptureRetainsThenTerminallyDrainsAbortedXHR(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hang-xhr" {
			select {
			case started <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`<html><body>xhr fixture</body></html>`))
	}))
	defer srv.Close()

	ctx, cancel := newHeadlessCtx(t)
	defer cancel()
	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()
	if err := chromedp.Run(runCtx, chromedp.Navigate(srv.URL)); err != nil {
		t.Fatal(err)
	}
	if err := InstallNetworkCapture(runCtx); err != nil {
		t.Fatal(err)
	}
	var ignored any
	if err := chromedp.Run(runCtx, chromedp.Evaluate(
		`(function(){ var x = new XMLHttpRequest(); window.__testXHR=x; window.__xhrDone=false; x.addEventListener('loadend', function(){window.__xhrDone=true}); x.open('GET','/hang-xhr'); x.send(); return true; })()`,
		&ignored,
	)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("XHR did not reach fixture server")
	}
	pendingRows, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatal(err)
	}
	pending := capturedRequestContaining(pendingRows, "/hang-xhr")
	if pending == nil || pending.Completed || pending.Transport != "xhr" || pending.Status != 0 || pending.CaptureID == "" {
		t.Fatalf("pending XHR capture = %+v", pending)
	}
	if err := chromedp.Run(runCtx, chromedp.Evaluate(
		`(function(){ window.__testXHR.abort(); return window.__xhrDone; })()`, &ignored,
	)); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(runCtx, chromedp.Poll(`window.__xhrDone === true`, nil)); err != nil {
		t.Fatalf("wait for XHR abort: %v", err)
	}
	terminalRows, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatal(err)
	}
	terminal := capturedRequestContaining(terminalRows, "/hang-xhr")
	if terminal == nil || !terminal.Completed || terminal.CaptureID != pending.CaptureID || terminal.Status != 0 || terminal.OK || terminal.Error != "request aborted" {
		t.Fatalf("terminal aborted XHR capture = %+v", terminal)
	}
	again, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate := capturedRequestContaining(again, "/hang-xhr"); duplicate != nil {
		t.Fatalf("aborted XHR was returned more than once: %+v", *duplicate)
	}
}

func TestNetworkCaptureDrainsFailedFetchExactlyOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("fixture response writer does not support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack failing response: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>failed network fixture</body></html>`))
	}))
	defer srv.Close()

	ctx, cancel := newHeadlessCtx(t)
	defer cancel()
	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()
	if err := chromedp.Run(runCtx, chromedp.Navigate(srv.URL)); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := InstallNetworkCapture(runCtx); err != nil {
		t.Fatalf("install: %v", err)
	}
	var ignored any
	if err := chromedp.Run(runCtx, chromedp.Evaluate(
		`(function(){ window.__failedPromise = fetch('/fail').then(function(){ return 'unexpected success'; }, function(){ return 'expected failure'; }); return true; })()`,
		&ignored,
	)); err != nil {
		t.Fatalf("start failing fetch: %v", err)
	}
	if err := chromedp.Run(runCtx, chromedp.Evaluate(`window.__failedPromise`, &ignored, awaitPromise)); err != nil {
		t.Fatalf("await failing fetch: %v", err)
	}

	requests, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatalf("drain failure: %v", err)
	}
	failed := capturedRequestContaining(requests, "/fail")
	if failed == nil || !failed.Completed || failed.CaptureID == "" || failed.Status != 0 || failed.OK || failed.Error == "" {
		t.Fatalf("failed request was not recorded as terminal: %+v", failed)
	}
	again, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatalf("repeat failure drain: %v", err)
	}
	if duplicate := capturedRequestContaining(again, "/fail"); duplicate != nil {
		t.Fatalf("failed request was returned more than once: %+v", *duplicate)
	}
}

func TestNetworkCaptureUpgradesInflightV1EntryWithoutLosingIt(t *testing.T) {
	ctx, cancel := newHeadlessCtx(t)
	defer cancel()
	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()
	if err := chromedp.Run(runCtx, chromedp.Navigate("about:blank")); err != nil {
		t.Fatal(err)
	}
	var ignored any
	if err := chromedp.Run(runCtx, chromedp.Evaluate(`(function(){
		window.__legacyEntry = {method:'GET',url:'/legacy-slow',request_headers:{},request_body:'',status:0,ok:false,response_snippet:'',transport:'fetch',error:'',started_at:1,duration_ms:0};
		window.__brwNet = [window.__legacyEntry];
		return true;
	})()`, &ignored)); err != nil {
		t.Fatal(err)
	}
	pendingRows, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatal(err)
	}
	pending := capturedRequestContaining(pendingRows, "/legacy-slow")
	if pending == nil || pending.Completed || pending.CaptureID == "" {
		t.Fatalf("v1 pending entry upgrade = %+v", pending)
	}
	if err := chromedp.Run(runCtx, chromedp.Evaluate(`(function(){ window.__legacyEntry.status=200; window.__legacyEntry.ok=true; window.__legacyEntry.duration_ms=10; return true; })()`, &ignored)); err != nil {
		t.Fatal(err)
	}
	terminalRows, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatal(err)
	}
	terminal := capturedRequestContaining(terminalRows, "/legacy-slow")
	if terminal == nil || !terminal.Completed || terminal.CaptureID != pending.CaptureID || terminal.Status != http.StatusOK {
		t.Fatalf("v1 terminal entry upgrade = %+v", terminal)
	}
	again, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate := capturedRequestContaining(again, "/legacy-slow"); duplicate != nil {
		t.Fatalf("upgraded terminal entry repeated: %+v", *duplicate)
	}
}

func TestNetworkCaptureRingBoundPrefersInflightRows(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow-ring" {
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-release:
				_, _ = w.Write([]byte("done"))
			case <-r.Context().Done():
			}
			return
		}
		_, _ = w.Write([]byte(`<html><body>bounded ring fixture</body></html>`))
	}))
	defer srv.Close()

	ctx, cancel := newHeadlessCtx(t)
	defer cancel()
	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()
	if err := chromedp.Run(runCtx, chromedp.Navigate(srv.URL)); err != nil {
		t.Fatal(err)
	}
	if err := InstallNetworkCapture(runCtx); err != nil {
		t.Fatal(err)
	}
	var ignored any
	if err := chromedp.Run(runCtx, chromedp.Evaluate(
		`(function(){ window.__ringSlowPromise = fetch('/slow-ring'); return true; })()`, &ignored,
	)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow ring request did not start")
	}

	var buffered int
	if err := chromedp.Run(runCtx, chromedp.Evaluate(
		`(async function(){ for (var i=0; i<140; i++) await fetch('/quick-ring?i=' + i); return window.__brwNet.length; })()`,
		&buffered,
		awaitPromise,
	)); err != nil {
		t.Fatalf("fill capture ring: %v", err)
	}
	if buffered != 100 {
		t.Fatalf("capture ring length=%d, want hard bound 100", buffered)
	}
	requests, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 100 {
		t.Fatalf("bounded drain returned %d rows, want 100", len(requests))
	}
	pending := capturedRequestContaining(requests, "/slow-ring")
	if pending == nil || pending.Completed || pending.Status != 0 {
		t.Fatalf("completed churn evicted the older in-flight request: %+v", pending)
	}
	seen := make(map[string]bool, len(requests))
	for _, request := range requests {
		if request.CaptureID == "" || len(request.CaptureID) > 40 || seen[request.CaptureID] {
			t.Fatalf("unbounded or duplicate capture id %q", request.CaptureID)
		}
		seen[request.CaptureID] = true
	}

	releaseOnce.Do(func() { close(release) })
	if err := chromedp.Run(runCtx, chromedp.Evaluate(`window.__ringSlowPromise`, &ignored, awaitPromise)); err != nil {
		t.Fatal(err)
	}
	terminal, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatal(err)
	}
	if completed := capturedRequestContaining(terminal, "/slow-ring"); completed == nil || !completed.Completed || completed.CaptureID != pending.CaptureID || completed.Status != http.StatusOK {
		t.Fatalf("retained ring request did not complete with stable identity: %+v", completed)
	}
}

func capturedRequestContaining(requests []CapturedRequest, needle string) *CapturedRequest {
	for i := range requests {
		if strings.Contains(requests[i].URL, needle) {
			return &requests[i]
		}
	}
	return nil
}

func TestNetworkCaptureSurvivesNavigation(t *testing.T) {
	ctx, cancel := newHeadlessCtx(t)
	defer cancel()

	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()

	if err := chromedp.Run(runCtx, chromedp.Navigate("about:blank")); err != nil {
		t.Fatalf("navigate about:blank: %v", err)
	}
	// Arm the interceptor to re-install at document-start on every new document.
	if err := RegisterNetworkCaptureOnNewDocument(runCtx); err != nil {
		t.Fatalf("register on new document: %v", err)
	}
	// Navigate to a fresh page that fetches on load. Crucially we do NOT call
	// InstallNetworkCapture after navigating — the armed script must have
	// re-installed the interceptor at document-start and wrapped the page's own
	// fetch.
	if err := chromedp.Run(runCtx, chromedp.Navigate(networkFixture)); err != nil {
		t.Fatalf("navigate fixture: %v", err)
	}
	if err := chromedp.Run(runCtx, chromedp.Sleep(300*time.Millisecond)); err != nil {
		t.Fatalf("settle: %v", err)
	}
	var requests []CapturedRequest
	if err := chromedp.Run(runCtx, chromedp.Evaluate(NetworkCaptureDrainScript, &requests)); err != nil {
		t.Fatalf("drain: %v", err)
	}
	found := false
	for _, r := range requests {
		if strings.Contains(r.URL, "data:application/json") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("interceptor did not survive navigation (no manual reinstall); captured: %+v", requests)
	}
}

func TestNetworkCaptureIDUsesANewEpochAfterNavigation(t *testing.T) {
	ctx, cancel := newHeadlessCtx(t)
	defer cancel()
	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()
	if err := chromedp.Run(runCtx, chromedp.Navigate("about:blank")); err != nil {
		t.Fatal(err)
	}
	if err := RegisterNetworkCaptureOnNewDocument(runCtx); err != nil {
		t.Fatal(err)
	}
	if err := InstallNetworkCapture(runCtx); err != nil {
		t.Fatal(err)
	}
	var ignored any
	if err := chromedp.Run(runCtx, chromedp.Evaluate(`fetch('data:text/plain,before-navigation')`, &ignored, awaitPromise)); err != nil {
		t.Fatal(err)
	}
	beforeRows, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatal(err)
	}
	before := capturedRequestContaining(beforeRows, "before-navigation")
	if before == nil || before.CaptureID == "" {
		t.Fatalf("before-navigation capture = %+v", before)
	}
	beforeEpoch, _, ok := strings.Cut(before.CaptureID, ":")
	if !ok {
		t.Fatalf("before capture id has no epoch: %q", before.CaptureID)
	}

	if err := chromedp.Run(runCtx, chromedp.Navigate(`data:text/html,<html><body>new document</body></html>`)); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(runCtx, chromedp.Evaluate(`fetch('data:text/plain,after-navigation')`, &ignored, awaitPromise)); err != nil {
		t.Fatal(err)
	}
	afterRows, err := CaptureNetwork(runCtx)
	if err != nil {
		t.Fatal(err)
	}
	after := capturedRequestContaining(afterRows, "after-navigation")
	if after == nil || after.CaptureID == "" {
		t.Fatalf("after-navigation capture = %+v", after)
	}
	afterEpoch, _, ok := strings.Cut(after.CaptureID, ":")
	if !ok || afterEpoch == beforeEpoch {
		t.Fatalf("capture epochs did not change across navigation: before=%q after=%q", before.CaptureID, after.CaptureID)
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

	result, err := ReplayRequest(runCtx, "GET", `data:application/json,{"ok":true}`, nil, "", 0, 0)
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

func TestReplayReturnsParseableNineteenKilobyteJSONByDefault(t *testing.T) {
	ctx, cancel := newHeadlessCtx(t)
	defer cancel()
	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()
	if err := chromedp.Run(runCtx, chromedp.Navigate(networkFixture)); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	payload := `{"clients":[{"name":"` + strings.Repeat("x", 19*1024) + `"}]}`
	target := "data:application/json," + url.PathEscape(payload)
	result, err := ReplayRequest(runCtx, "GET", target, nil, "", 0, 0)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if result.BodyTruncated || result.NextOffset != 0 || result.BodyTotalBytes != len(payload) {
		t.Fatalf("unexpected window metadata: %+v", result)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result.Body), &decoded); err != nil {
		t.Fatalf("default replay body is not parseable JSON: %v (bytes=%d total=%d)", err, result.BodyBytes, result.BodyTotalBytes)
	}
}

func TestReplayLargeBodyExposesDeterministicByteWindows(t *testing.T) {
	ctx, cancel := newHeadlessCtx(t)
	defer cancel()
	runCtx, runCancel := context.WithTimeout(ctx, 30*time.Second)
	defer runCancel()
	if err := chromedp.Run(runCtx, chromedp.Navigate(networkFixture)); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	payload := strings.Repeat("abcdef", 20_000)
	target := "data:text/plain," + payload
	first, err := ReplayRequest(runCtx, "GET", target, nil, "", 0, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if !first.BodyTruncated || first.NextOffset != 10_000 || first.BodyBytes != 10_000 || first.BodyTotalBytes != len(payload) {
		t.Fatalf("first window = %+v", first)
	}
	second, err := ReplayRequest(runCtx, "GET", target, nil, "", first.NextOffset, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if second.BodyOffset != first.NextOffset || second.Body != payload[10_000:20_000] {
		t.Fatalf("second window mismatch: offset=%d bytes=%d", second.BodyOffset, second.BodyBytes)
	}
}

// awaitPromise lets the explicit-fetch evaluation await its promise.
func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}

// TestRedactCapturedCredentials proves credential header VALUES are blanked while
// the header NAME and all non-credential fields survive, so brw_network_capture
// cannot hand back a live token/session cookie but still shows the header existed.
func TestRedactCapturedCredentials(t *testing.T) {
	reqs := []CapturedRequest{{
		Method: "POST",
		URL:    "https://api.example.com/login",
		RequestHeaders: map[string]string{
			"Authorization": "Bearer sk-secret-token",
			"COOKIE":        "session=abc123", // case-insensitive match
			"X-Api-Key":     "key-live-123",
			"Content-Type":  "application/json",
			"Accept":        "*/*",
		},
		RequestBody: `{"user":"a","password":"p"}`,
	}}
	out := RedactCapturedCredentials(reqs)
	h := out[0].RequestHeaders
	for _, k := range []string{"Authorization", "COOKIE", "X-Api-Key"} {
		if h[k] != "[redacted]" {
			t.Errorf("header %q value = %q, want [redacted]", k, h[k])
		}
	}
	if h["Content-Type"] != "application/json" {
		t.Errorf("non-credential header Content-Type was altered: %q", h["Content-Type"])
	}
	if h["Accept"] != "*/*" {
		t.Errorf("non-credential header Accept was altered: %q", h["Accept"])
	}
	// The body is intentionally left untouched (documented behavior).
	if out[0].RequestBody == "" {
		t.Errorf("request body must not be blanked by header redaction")
	}
	// A nil-headers request must not panic.
	_ = RedactCapturedCredentials([]CapturedRequest{{URL: "https://x/"}})
}
