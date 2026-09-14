package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/devtools"
	"github.com/Don-Works/brw/internal/httpclient"
)

// routeFixtureDelay is the server think-time the fixture controls. Without it
// a loopback response is fast enough that TTFB rounds to zero, and an assertion
// on "greater than zero" would be an assertion about the machine.
const routeFixtureDelay = 150 * time.Millisecond

// devtoolsRouteFixture fails one axe rule on purpose: #bbbbbb text on white is
// about 1.9:1 where 4.5:1 is required.
const devtoolsRouteFixture = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<title>Route fixture</title></head><body>
<h1>Route fixture</h1>
<p id="low-contrast" style="color:#bbbbbb;background-color:#ffffff">Unreadable on purpose.</p>
</body></html>`

// newDevtoolsRouteServer is the daemon as an upstream proxy actually meets it:
// a real browser behind the routes and a real artifact store beside them. The
// routes are where the audit and its artifact are joined, so a fake on either
// side would test the wrong seam.
func newDevtoolsRouteServer(t *testing.T) (*Server, string) {
	t.Helper()
	chromePath, err := cdp.FindChrome("")
	if err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	manager, err := browser.New(ctx, browser.Config{
		ChromePath:  chromePath,
		UserDataDir: t.TempDir(),
		Headless:    true,
		Timeout:     45 * time.Second,
	})
	if err != nil {
		t.Skipf("headless Chrome did not start: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	store, err := artifact.NewStore(artifact.Config{
		Root:             filepath.Join(t.TempDir(), "artifacts"),
		MaxArtifactBytes: 8 << 20,
		MaxTotalBytes:    32 << 20,
		TTL:              time.Hour,
	})
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	service, err := artifact.NewService(store, manager)
	if err != nil {
		t.Fatalf("artifact service: %v", err)
	}

	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(routeFixtureDelay)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, devtoolsRouteFixture)
	}))
	t.Cleanup(fixture.Close)
	if _, err := manager.Open(ctx, fixture.URL); err != nil {
		t.Fatalf("open the fixture: %v", err)
	}

	server := New("", manager)
	server.SetArtifactAPI(service)
	return server, fixture.URL
}

// ttfbFloor is eighty per cent of the fixture's own think-time: a reading below
// it did not come from this navigation.
func ttfbFloor() float64 { return float64(routeFixtureDelay/time.Millisecond) * 0.8 }

func postRoute(t *testing.T, server *Server, path, body string, out any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s = %d, body = %s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %s: %v (%s)", path, err, rec.Body.String())
	}
}

// TestDevtoolsRoutesAnswerFromTheRealBrowser covers the daemon surface an
// upstream MCP proxy forwards to. The audit route is the one that does more
// than forward: it has to store the report here and answer with a handle,
// because the process on the other side of the proxy has no store.
func TestDevtoolsRoutesAnswerFromTheRealBrowser(t *testing.T) {
	server, _ := newDevtoolsRouteServer(t)

	var vitals devtools.Vitals
	postRoute(t, server, "/api/page/vitals", `{"settle_ms":400}`, &vitals)
	if vitals.TTFBMS == nil || *vitals.TTFBMS < ttfbFloor() {
		t.Fatalf("ttfb_ms = %v, want at least %v for a fixture that waited %v", vitals.TTFBMS, ttfbFloor(), routeFixtureDelay)
	}
	if vitals.SettledMS != 400 {
		t.Errorf("settled_ms = %d, want the requested 400", vitals.SettledMS)
	}

	var audit devtools.AuditResult
	postRoute(t, server, "/api/page/a11y", `{"rules":["color-contrast"]}`, &audit)
	if len(audit.Rules) != 1 || audit.Rules[0].ID != "color-contrast" {
		t.Fatalf("audit rules = %+v, want the one rule that was asked for", audit.Rules)
	}
	if audit.Artifact == nil || audit.Artifact.ID == "" {
		t.Fatalf("no artifact handle came back from the route: %+v", audit)
	}
	if len(audit.Report) != 0 {
		t.Fatal("the route answered with the full report; it belongs in the artifact")
	}

	// The handle has to name a report readable through the artifact routes the
	// same daemon serves, or the two halves are not actually joined.
	var chunk artifact.Chunk
	postRoute(t, server, "/api/artifacts/read",
		fmt.Sprintf(`{"artifact_id":%q,"max_bytes":%d}`, audit.Artifact.ID, artifact.MaxReadBytes), &chunk)
	if !strings.Contains(chunk.Text, "color-contrast") {
		t.Errorf("stored report = %.300s, want the audit document", chunk.Text)
	}
	// The audit is read-shaped but not effect-free, and the route has to carry
	// that out with the answer rather than leaving it to the tool description.
	if !strings.Contains(audit.PageEffects, "data-brw-ref") || !strings.Contains(audit.PageEffects, "window.axe") {
		t.Errorf("page_effects = %q, want it to name what the audit left in the page", audit.PageEffects)
	}

	// The stored report holds the raw HTML of every failing element, so a caller
	// on a page carrying real data has to be able to bound its retention through
	// this route — it is the one an upstream MCP process forwards to. The store
	// behind these routes keeps artifacts for an hour.
	var bounded devtools.AuditResult
	postRoute(t, server, "/api/page/a11y", `{"rules":["color-contrast"],"ttl_seconds":120}`, &bounded)
	if bounded.Artifact == nil {
		t.Fatalf("no artifact handle came back with a ttl: %+v", bounded)
	}
	if left := time.Until(bounded.Artifact.ExpiresAt); left > 10*time.Minute {
		t.Errorf("the report expires in %v with ttl_seconds:120; the route dropped the caller's retention", left)
	}

	ref := audit.Rules[0].Refs[0]
	var marked devtools.HighlightResult
	postRoute(t, server, "/api/page/highlight", fmt.Sprintf(`{"ref":%q}`, ref), &marked)
	if marked.Active != 1 || len(marked.Marked) != 1 || !marked.Marked[0].Found {
		t.Fatalf("highlight = %+v, want the audit's own ref to resolve", marked)
	}

	var cleared devtools.HighlightResult
	postRoute(t, server, "/api/page/highlight", `{"clear":true}`, &cleared)
	if !cleared.Cleared || cleared.Active != 0 {
		t.Fatalf("clear = %+v, want the overlay reported gone", cleared)
	}
}

// TestAccessibilityRouteSaysSoWhenItCannotStoreTheReport: a daemon started with
// --artifact-dir off still answers, and says the report is gone rather than
// letting the summary read as everything axe found.
func TestAccessibilityRouteSaysSoWhenItCannotStoreTheReport(t *testing.T) {
	server, _ := newDevtoolsRouteServer(t)
	server.SetArtifactAPI(nil)

	var audit devtools.AuditResult
	postRoute(t, server, "/api/page/a11y", `{"rules":["color-contrast"]}`, &audit)
	if audit.Artifact != nil {
		t.Fatalf("an artifact handle came back with no store configured: %+v", audit.Artifact)
	}
	if !strings.Contains(audit.Note, "no artifact store") {
		t.Fatalf("note = %q, want it to say the full report was not kept", audit.Note)
	}
	if audit.Violations != 1 {
		t.Fatalf("violations = %d, want the fixture's deliberate failure still reported", audit.Violations)
	}
}

// TestDevtoolsRoutesRefuseATransportThatCannotObserve pins the named capability
// error. A transport without the capability must not answer 200 with an empty
// page report, which reads exactly like a clean result.
func TestDevtoolsRoutesRefuseATransportThatCannotObserve(t *testing.T) {
	server := New("", &fakeController{})
	for _, path := range []string{"/api/page/vitals", "/api/page/a11y", "/api/page/highlight"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			server.server.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want a refusal; body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "does not support developer observations") {
				t.Fatalf("body = %s, want the named capability error", rec.Body.String())
			}
		})
	}
}

// TestUpstreamProxyGetsTheHandleAndNotTheReport is the --upstream-http topology
// end to end: a disposable MCP process holds an httpclient.Controller pointed
// at this daemon. The audit runs here, the report is stored here, and what
// crosses is a summary and a handle — the data-locality rule the artifact
// capture path already follows.
func TestUpstreamProxyGetsTheHandleAndNotTheReport(t *testing.T) {
	server, fixtureURL := newDevtoolsRouteServer(t)
	daemon := httptest.NewServer(server.server.Handler)
	t.Cleanup(daemon.Close)

	proxy, err := httpclient.New(daemon.URL, 120*time.Second)
	if err != nil {
		t.Fatalf("upstream controller: %v", err)
	}
	ctx := context.Background()

	// A proxied session gets its own leased working tab rather than whatever the
	// browser was already showing, so it has to put the page there itself. That
	// is the flow an agent runs, and it is what makes the readings below belong
	// to this session's page.
	if _, err := proxy.Open(ctx, fixtureURL); err != nil {
		t.Fatalf("open the fixture over the proxy: %v", err)
	}

	vitals, err := proxy.Vitals(ctx, devtools.VitalsOptions{SettleMS: 400})
	if err != nil {
		t.Fatalf("vitals over the proxy: %v", err)
	}
	if vitals.TTFBMS == nil || *vitals.TTFBMS < ttfbFloor() {
		t.Fatalf("ttfb_ms across the proxy = %v, want at least %v", vitals.TTFBMS, ttfbFloor())
	}
	if !strings.HasPrefix(vitals.URL, fixtureURL) {
		t.Fatalf("url = %q, want the page this session opened at %q", vitals.URL, fixtureURL)
	}

	audit, err := proxy.AccessibilityAudit(ctx, devtools.AuditOptions{Rules: []string{"color-contrast"}})
	if err != nil {
		t.Fatalf("audit over the proxy: %v", err)
	}
	if len(audit.Report) != 0 {
		t.Fatal("the full report crossed the proxy boundary")
	}
	if audit.Artifact == nil || audit.Artifact.ID == "" {
		t.Fatalf("no artifact handle crossed the proxy: %+v", audit)
	}
	if len(audit.Rules) != 1 || audit.Rules[0].ID != "color-contrast" || len(audit.Rules[0].Refs) == 0 {
		t.Fatalf("summary across the proxy = %+v", audit.Rules)
	}

	// An upstream MCP process runs the same attach step, with its own artifact
	// API being the proxy. It must leave the daemon's handle alone rather than
	// reporting the report lost.
	forwarded := artifact.AttachAuditReport(ctx, proxy, audit, 0)
	if forwarded.Artifact == nil || forwarded.Artifact.ID != audit.Artifact.ID {
		t.Fatalf("the upstream process lost the daemon's handle: %+v", forwarded.Artifact)
	}
	if forwarded.Note != "" {
		t.Errorf("note = %q, want none when the browser host already stored the report", forwarded.Note)
	}

	marked, err := proxy.Highlight(ctx, devtools.HighlightOptions{Ref: audit.Rules[0].Refs[0]})
	if err != nil {
		t.Fatalf("highlight over the proxy: %v", err)
	}
	if marked.Active != 1 || !marked.Marked[0].Found {
		t.Fatalf("highlight across the proxy = %+v", marked)
	}
	if _, err := proxy.Highlight(ctx, devtools.HighlightOptions{Clear: true}); err != nil {
		t.Fatalf("clear over the proxy: %v", err)
	}
}
