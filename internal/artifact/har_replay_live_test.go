package artifact

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/browsertest"
	"github.com/Don-Works/brw/internal/cdp"
)

// harReplayFixture is a shell page whose data comes entirely from two API calls.
// It reports READY only when both answered with the values the recording holds,
// so "green" means the fixture actually served the page rather than the page
// merely surviving a failed fetch.
const harReplayFixture = `<html><head><title>fixture</title></head><body>
<p id="status">pending</p>
<script>
window.__load = function () {
  window.__status = 'pending';
  return Promise.all([
    fetch('/api/config').then(function (r) { return r.json(); }),
    fetch('/api/items').then(function (r) { return r.json(); })
  ]).then(function (parts) {
    var ok = parts[0].label === 'recorded-config' && parts[1].items.length === 2;
    window.__status = ok ? 'READY:' + parts[0].label + ':' + parts[1].items.join(',') : 'WRONG';
  }).catch(function (e) {
    window.__status = 'FAILED:' + e;
  }).then(function () {
    document.getElementById('status').textContent = window.__status;
    return window.__status;
  });
};
</script></body></html>`

func newLiveManager(t *testing.T) *browser.Manager {
	t.Helper()
	if _, err := cdp.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	profile := browsertest.NewProfile(t)
	// The browser's own context has to outlive this helper: chromedp derives the
	// allocator from it, so a cancel here would tear the browser down before the
	// first call.
	ctx, cancel := context.WithCancel(context.Background())
	m, err := browser.New(ctx, browser.Config{
		Headless:    true,
		UserDataDir: profile.Dir(),
		Timeout:     20 * time.Second,
	})
	if err != nil {
		cancel()
		t.Skipf("headless Chrome did not start: %v", err)
	}
	// Stops unwind in reverse: the manager closes, then the allocator context is
	// cancelled, and only then is the profile reclaimed.
	profile.StopWith(cancel)
	profile.StopWith(func() { _ = m.Close() })
	return m
}

func evaluateStringLive(t *testing.T, m *browser.Manager, ctx context.Context, expr string) string {
	t.Helper()
	value, err := m.Evaluate(ctx, expr)
	if err != nil {
		t.Fatalf("evaluate %s: %v", expr, err)
	}
	text, _ := value.(string)
	return text
}

// The whole point of a HAR fixture: record a page's traffic once, then load the
// page again from the recording with the backend refusing every request. It is
// the one test that drives the complete path — capture, redacted export, store,
// decode, replay through Fetch interception — so deleting any link in it turns
// the page red or lets a request through to the server this asserts is untouched.
func TestPageLoadsFromARecordedHARWithTheBackendDenied(t *testing.T) {
	var apiHits int64
	var offline atomic.Bool

	api := func(w http.ResponseWriter, payload string) {
		atomic.AddInt64(&apiHits, 1)
		if offline.Load() {
			http.Error(w, "the backend is denied for this test", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, payload)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, _ *http.Request) {
		api(w, `{"label":"recorded-config"}`)
	})
	mux.HandleFunc("/api/items", func(w http.ResponseWriter, _ *http.Request) {
		api(w, `{"items":["alpha","beta"]}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, harReplayFixture)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := newLiveManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, srv.URL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tabID := opened.Tab.ID

	// Installs the in-page capture before the page makes its calls; a HAR of a
	// page whose traffic already happened is empty.
	if _, err := m.NetworkCapture(ctx, ""); err != nil {
		t.Fatalf("install network capture: %v", err)
	}
	if got := evaluateStringLive(t, m, ctx, `window.__load()`); !strings.HasPrefix(got, "READY:") {
		t.Fatalf("the live page did not load green before recording: %q", got)
	}
	recordedHits := atomic.LoadInt64(&apiHits)
	if recordedHits != 2 {
		t.Fatalf("recording pass hit the API %d times, want 2", recordedHits)
	}

	store, err := NewStore(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	svc, err := NewService(store, m)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	meta, err := svc.CaptureArtifact(ctx, CaptureOptions{Kind: "har"})
	if err != nil {
		t.Fatalf("capture har: %v", err)
	}
	entries, err := LoadHARFixture(ctx, svc, meta.ID)
	if err != nil {
		t.Fatalf("load har fixture: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("the HAR holds %d entries, want at least the two API calls", len(entries))
	}

	// From here the backend refuses everything under /api. Anything the page
	// still renders came out of the recording.
	offline.Store(true)
	atomic.StoreInt64(&apiHits, 0)

	replayed, err := m.Route(ctx, browser.RouteOptions{
		Action:        "replay",
		TabID:         tabID,
		Pattern:       srv.URL + "/api/*",
		HARArtifactID: meta.ID,
		HAR:           entries,
		Match:         []string{browser.HARMatchMethod, browser.HARMatchURL},
		OnMiss:        browser.HARMissFail,
	})
	if err != nil {
		t.Fatalf("route replay: %v", err)
	}
	if replayed.Count != 1 || replayed.Routes[0].Fixture == nil {
		t.Fatalf("replay did not install a fixture-backed route: %+v", replayed)
	}

	if _, err := m.NavigateTo(browser.WithTabID(ctx, tabID), srv.URL); err != nil {
		t.Fatalf("reload: %v", err)
	}
	status := evaluateStringLive(t, m, ctx, `window.__load()`)
	if status != "READY:recorded-config:alpha,beta" {
		t.Fatalf("the page did not load green from the HAR: %q (server was hit %d times)", status, atomic.LoadInt64(&apiHits))
	}
	if hits := atomic.LoadInt64(&apiHits); hits != 0 {
		t.Fatalf("the denied backend was still reached %d time(s); the fixture is not answering", hits)
	}

	listed, err := m.Route(ctx, browser.RouteOptions{Action: "list", TabID: tabID})
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	fixture := listed.Routes[0].Fixture
	if fixture == nil || fixture.Served < 2 {
		t.Fatalf("the fixture reports %+v; two recorded entries should have been served", fixture)
	}
	if fixture.ArtifactID != meta.ID {
		t.Fatalf("the listed fixture names %q, want the HAR artifact %q", fixture.ArtifactID, meta.ID)
	}

	// on_miss:"fail" has to refuse a request the recording does not hold, and say
	// which one: an incomplete fixture otherwise reads as a broken page.
	missed := evaluateStringLive(t, m, ctx,
		fmt.Sprintf(`fetch(%q).then(function(){return 'reached';}).catch(function(){return 'refused';})`, srv.URL+"/api/never-recorded"))
	if missed != "refused" {
		t.Fatalf("an unrecorded request under on_miss:fail returned %q, want refused", missed)
	}
	if hits := atomic.LoadInt64(&apiHits); hits != 0 {
		t.Fatalf("an on_miss:fail request still reached the backend %d time(s)", hits)
	}

	listed, err = m.Route(ctx, browser.RouteOptions{Action: "list", TabID: tabID})
	if err != nil {
		t.Fatalf("list routes after the miss: %v", err)
	}
	fixture = listed.Routes[0].Fixture
	if fixture == nil || len(fixture.Misses) != 1 {
		t.Fatalf("the miss was not recorded: %+v", fixture)
	}
	miss := fixture.Misses[0]
	if miss.Method != "GET" || !strings.Contains(miss.URL, "/api/never-recorded") {
		t.Fatalf("the miss does not name the request: %+v", miss)
	}
	if !strings.Contains(miss.Reason, "GET") || !strings.Contains(miss.Reason, "/api/never-recorded") {
		t.Fatalf("the miss reason names neither the method nor the URL: %q", miss.Reason)
	}

	// The miss has to reach brw_observe, not only brw_route{action:"list"}. An
	// agent driving a page observes after each action and lists routes almost
	// never, so a fixture that misses only in the route table leaves a
	// half-loaded page pointing at nothing — which is the whole reason the
	// observation carries the count and the reasons.
	observed, err := m.Observe(browser.WithTabID(ctx, tabID))
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if observed.ActiveRoutes != 1 {
		t.Fatalf("brw_observe reports %d active routes, want the installed replay", observed.ActiveRoutes)
	}
	if observed.RouteMisses != 1 {
		t.Fatalf("brw_observe reports %d fixture misses, want the one the page just made", observed.RouteMisses)
	}
	if len(observed.RouteMissReasons) != 1 || !strings.Contains(observed.RouteMissReasons[0], "/api/never-recorded") {
		t.Fatalf("brw_observe does not name what the fixture could not answer: %v", observed.RouteMissReasons)
	}
}

// harDefaultPatternFixture is a page whose data comes from one API call and one
// oversized one, so a single load exercises both the request kinds a brw HAR can
// hold and a response larger than the capture's per-body cap.
const harDefaultPatternFixture = `<html><head><title>default-pattern</title></head><body>
<p id="status">pending</p>
<script>
window.__load = function () {
  return Promise.all([
    fetch('/api/config').then(function (r) { return r.json(); }),
    fetch('/api/big').then(function (r) { return r.text(); })
  ]).then(function (parts) {
    window.__status = parts[0].label === 'recorded-config' ? 'READY:' + parts[1].length : 'WRONG';
  }).catch(function (e) {
    window.__status = 'FAILED:' + e;
  }).then(function () {
    document.getElementById('status').textContent = window.__status;
    return window.__status;
  });
};
</script></body></html>`

// The documented default is pattern "*" with on_miss:"fail". A brw HAR is built
// from the in-page fetch/XHR wrappers and holds no document, script or image, so
// a replay that claimed every request would refuse the navigation itself and
// leave a dead tab. This drives the documented call verbatim and asserts the
// page still loads, that its API calls come from the recording, and that the
// fixture reports both the passthrough and the clipped recording rather than
// serving a half body as though it were whole.
func TestReplayWithTheDefaultPatternLoadsThePageAndReportsWhatItCannotHold(t *testing.T) {
	var apiHits int64
	var offline atomic.Bool
	// Comfortably over the in-page 2048-character body cap, so the recording of
	// this response is necessarily a clipped snippet.
	bigPayload := `{"blob":"` + strings.Repeat("z", 4096) + `"}`

	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&apiHits, 1)
		if offline.Load() {
			http.Error(w, "the backend is denied for this test", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"label":"recorded-config"}`)
	})
	mux.HandleFunc("/api/big", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&apiHits, 1)
		if offline.Load() {
			http.Error(w, "the backend is denied for this test", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, bigPayload)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, harDefaultPatternFixture)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := newLiveManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, srv.URL)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	tabID := opened.Tab.ID
	if _, err := m.NetworkCapture(ctx, ""); err != nil {
		t.Fatalf("install network capture: %v", err)
	}
	if got := evaluateStringLive(t, m, ctx, `window.__load()`); !strings.HasPrefix(got, "READY:") {
		t.Fatalf("the live page did not load green before recording: %q", got)
	}

	store, err := NewStore(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	svc, err := NewService(store, m)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	meta, err := svc.CaptureArtifact(ctx, CaptureOptions{Kind: "har"})
	if err != nil {
		t.Fatalf("capture har: %v", err)
	}
	entries, err := LoadHARFixture(ctx, svc, meta.ID)
	if err != nil {
		t.Fatalf("load har fixture: %v", err)
	}

	truncated := 0
	for _, entry := range entries {
		if entry.Truncated {
			truncated++
		}
	}
	if truncated != 1 {
		t.Fatalf("%d of %d recorded responses are flagged truncated, want exactly the oversized one", truncated, len(entries))
	}

	offline.Store(true)
	atomic.StoreInt64(&apiHits, 0)

	// Pattern omitted on purpose: this is the documented default, and with
	// on_miss:"fail" it is the combination that used to refuse the document.
	replayed, err := m.Route(ctx, browser.RouteOptions{
		Action:        "replay",
		TabID:         tabID,
		HARArtifactID: meta.ID,
		HAR:           entries,
		OnMiss:        browser.HARMissFail,
	})
	if err != nil {
		t.Fatalf("route replay: %v", err)
	}
	if replayed.Routes[0].Pattern != "*" {
		t.Fatalf("the default pattern is %q, want the documented *", replayed.Routes[0].Pattern)
	}
	for _, want := range []string{"fetch and XHR", "truncated"} {
		if !strings.Contains(replayed.Note, want) {
			t.Fatalf("the replay note does not mention %q: %q", want, replayed.Note)
		}
	}

	if _, err := m.NavigateTo(browser.WithTabID(ctx, tabID), srv.URL); err != nil {
		t.Fatalf("reload under the default replay pattern: %v", err)
	}
	title := evaluateStringLive(t, m, ctx, `document.title`)
	if title != "default-pattern" {
		t.Fatalf("document title = %q; the navigation itself was refused by the fixture", title)
	}
	if body := evaluateStringLive(t, m, ctx, `document.body ? document.body.textContent.trim() : 'NO BODY'`); body == "NO BODY" {
		t.Fatal("the page has no body: the replay refused the document request")
	}

	status := evaluateStringLive(t, m, ctx, `window.__load()`)
	if !strings.HasPrefix(status, "READY:") {
		t.Fatalf("the page did not load green from the HAR: %q (server was hit %d times)", status, atomic.LoadInt64(&apiHits))
	}
	if hits := atomic.LoadInt64(&apiHits); hits != 0 {
		t.Fatalf("the denied backend was still reached %d time(s); the fixture is not answering", hits)
	}

	listed, err := m.Route(ctx, browser.RouteOptions{Action: "list", TabID: tabID})
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	fixture := listed.Routes[0].Fixture
	if fixture == nil {
		t.Fatal("the listed route carries no fixture")
	}
	if fixture.NotReplayable == 0 {
		t.Fatalf("the fixture reports %+v; the document it let through has to be counted, not silent", fixture)
	}
	if fixture.Truncated != 1 {
		t.Fatalf("the fixture reports %d truncated recordings, want 1", fixture.Truncated)
	}
	if fixture.ServedTruncated != 1 {
		t.Fatalf("the fixture served %d truncated bodies, want the oversized recording counted as it is handed over", fixture.ServedTruncated)
	}
	// The clipped recording is served rather than withheld, so the page sees a
	// short body; what must not happen is brw reporting it as a whole one.
	if !strings.HasPrefix(status, "READY:") || status == "READY:"+fmt.Sprint(len(bigPayload)) {
		t.Fatalf("the replayed oversized body reported as the whole %d-byte response: %q", len(bigPayload), status)
	}
}
