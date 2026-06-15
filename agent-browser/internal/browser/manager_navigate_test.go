package browser

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/revitt/agent-browser/internal/cdp"
)

func TestNormalizeNavigateDirection(t *testing.T) {
	cases := map[string]string{
		"back":      NavigateBack,
		"  Back ":   NavigateBack,
		"FORWARD":   NavigateForward,
		"reload":    NavigateReload,
		"  reLoad ": NavigateReload,
	}
	for in, want := range cases {
		got, err := normalizeNavigateDirection(in)
		if err != nil {
			t.Fatalf("normalizeNavigateDirection(%q) unexpected error: %v", in, err)
		}
		if got != want {
			t.Fatalf("normalizeNavigateDirection(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "up", "go-back", "refresh"} {
		if _, err := normalizeNavigateDirection(bad); err == nil {
			t.Fatalf("normalizeNavigateDirection(%q) expected error, got nil", bad)
		}
	}
}

// navFixtureServer serves two generic HTML pages (/a and /b) with unique
// markers so the test can assert which document the active tab is showing.
// Standard same-origin pages — no real site, no site-specific logic — so
// location assignment between them creates real session-history entries.
func navFixtureServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>FixtureA</title><h1>page-A-marker</h1>`))
	})
	mux.HandleFunc("/b", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>FixtureB</title><h1>page-B-marker</h1>`))
	})
	return httptest.NewServer(mux)
}

func TestManagerNavigateHistory(t *testing.T) {
	if _, err := cdp.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}

	srv := navFixtureServer()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	m, err := New(ctx, Config{
		Timeout:    20 * time.Second,
		ChromeArgs: []string{"--headless=new", "--disable-gpu", "--no-sandbox"},
	})
	if err != nil {
		t.Skipf("could not launch headless Chrome: %v", err)
	}
	defer m.Close()

	// Open fixture A, then navigate the same tab to B via standard location
	// assignment so both entries share one session history.
	if _, err := m.Open(ctx, srv.URL+"/a"); err != nil {
		t.Fatalf("open fixtureA: %v", err)
	}
	waitForMarker(t, ctx, m, "page-A-marker")

	if _, err := m.Evaluate(ctx, "location.assign("+jsString(srv.URL+"/b")+")"); err != nil {
		t.Fatalf("navigate to fixtureB: %v", err)
	}
	waitForMarker(t, ctx, m, "page-B-marker")

	// back -> should land on fixtureA.
	res, err := m.Navigate(ctx, "back")
	if err != nil {
		t.Fatalf("navigate back: %v", err)
	}
	if !res.OK {
		t.Fatalf("navigate back result not ok: %+v", res)
	}
	waitForMarker(t, ctx, m, "page-A-marker")
	assertLocationContains(t, ctx, m, "/a")

	// forward -> should land back on fixtureB.
	if _, err := m.Navigate(ctx, "forward"); err != nil {
		t.Fatalf("navigate forward: %v", err)
	}
	waitForMarker(t, ctx, m, "page-B-marker")
	assertLocationContains(t, ctx, m, "/b")

	// reload -> stays on fixtureB and re-renders the marker.
	if _, err := m.Navigate(ctx, "reload"); err != nil {
		t.Fatalf("navigate reload: %v", err)
	}
	waitForMarker(t, ctx, m, "page-B-marker")
	assertLocationContains(t, ctx, m, "/b")

	// invalid direction is rejected without touching the page.
	if _, err := m.Navigate(ctx, "sideways"); err == nil {
		t.Fatal("expected error for invalid navigate direction")
	}
}

func waitForMarker(t *testing.T, ctx context.Context, m *Manager, marker string) {
	t.Helper()
	if err := m.WaitFor(ctx, "text:"+marker, 10*time.Second); err != nil {
		t.Fatalf("waiting for marker %q: %v", marker, err)
	}
}

func assertLocationContains(t *testing.T, ctx context.Context, m *Manager, want string) {
	t.Helper()
	got, err := m.Evaluate(ctx, "location.pathname")
	if err != nil {
		t.Fatalf("read location.pathname: %v", err)
	}
	path, _ := got.(string)
	if !strings.Contains(path, want) {
		t.Fatalf("location.pathname = %q, want it to contain %q", path, want)
	}
}

// jsString renders a Go string as a JS string literal (via JSON encoding) for
// safe embedding in an evaluate expression.
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
