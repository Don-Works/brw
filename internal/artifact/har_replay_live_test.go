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
	// The browser's own context has to outlive this helper: chromedp derives the
	// allocator from it, so a cancel here would tear the browser down before the
	// first call.
	ctx, cancel := context.WithCancel(context.Background())
	m, err := browser.New(ctx, browser.Config{
		Headless:    true,
		UserDataDir: t.TempDir(),
		Timeout:     20 * time.Second,
	})
	if err != nil {
		cancel()
		t.Skipf("headless Chrome did not start: %v", err)
	}
	t.Cleanup(func() {
		_ = m.Close()
		cancel()
	})
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
}
