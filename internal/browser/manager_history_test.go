package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/navpolicy"
)

// spaRouterFixture is a minimal client-side router: it renders the current path
// and only re-renders on popstate. __marker is set once per DOCUMENT load, so a
// test can tell a same-document history change from a reload — a reload serves
// this same page again and resets it.
const spaRouterFixture = `<!doctype html><html><head><meta charset="utf-8"><title>spa router</title></head>
<body>
<div id="view">unrendered</div>
<script>
window.__marker = 'first-document';
window.__renders = 0;
function render(){
  document.getElementById('view').textContent = location.pathname + location.search;
  window.__renders++;
}
window.addEventListener('popstate', render);
render();
</script>
</body></html>`

func openRouterFixture(t *testing.T, m *Manager, ctx context.Context) (context.Context, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, spaRouterFixture)
	}))
	t.Cleanup(srv.Close)
	opened, err := m.Open(ctx, srv.URL+"/")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	return WithTabID(ctx, opened.Tab.ID), srv.URL
}

func evalString(t *testing.T, m *Manager, ctx context.Context, expr string) string {
	t.Helper()
	value, err := m.Evaluate(ctx, expr)
	if err != nil {
		t.Fatalf("evaluate %q: %v", expr, err)
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("evaluate %q returned %T (%v), want a string", expr, value, value)
	}
	return text
}

func TestPushStateChangesRouteWithoutReload(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tabCtx, _ := openRouterFixture(t, m, ctx)

	result, err := m.PushState(tabCtx, HistoryStateOptions{URL: "/dashboard?tab=2", State: `{"from":"test"}`})
	if err != nil {
		t.Fatalf("pushstate: %v", err)
	}
	if !strings.HasSuffix(result.URL, "/dashboard?tab=2") {
		t.Fatalf("pushstate reported url %q, want it to end with /dashboard?tab=2", result.URL)
	}
	if !result.Notified {
		t.Fatal("pushstate should dispatch popstate by default")
	}
	// Same document: the marker set at load time survives, so nothing reloaded.
	if marker := evalString(t, m, tabCtx, "window.__marker"); marker != "first-document" {
		t.Fatalf("window.__marker = %q after pushstate: the document was replaced, so this was a navigation, not a history change", marker)
	}
	if view := evalString(t, m, tabCtx, "document.getElementById('view').textContent"); view != "/dashboard?tab=2" {
		t.Fatalf("router rendered %q, want /dashboard?tab=2", view)
	}
	if state := evalString(t, m, tabCtx, "JSON.stringify(history.state)"); state != `{"from":"test"}` {
		t.Fatalf("history.state = %s, want the pushed object", state)
	}

	// notify:false changes the URL and leaves the router alone, which is how you
	// test whether an app reacts to a route change on its own.
	renders := evalString(t, m, tabCtx, "String(window.__renders)")
	notify := false
	if _, err := m.PushState(tabCtx, HistoryStateOptions{URL: "/silent", Notify: &notify}); err != nil {
		t.Fatalf("pushstate without notify: %v", err)
	}
	if path := evalString(t, m, tabCtx, "location.pathname"); path != "/silent" {
		t.Fatalf("location.pathname = %q, want /silent", path)
	}
	if view := evalString(t, m, tabCtx, "document.getElementById('view').textContent"); view != "/dashboard?tab=2" {
		t.Fatalf("router re-rendered to %q with notify:false", view)
	}
	if after := evalString(t, m, tabCtx, "String(window.__renders)"); after != renders {
		t.Fatalf("render count moved from %s to %s with notify:false", renders, after)
	}
}

func TestPushStateReplaceDoesNotGrowHistory(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tabCtx, _ := openRouterFixture(t, m, ctx)

	pushed, err := m.PushState(tabCtx, HistoryStateOptions{URL: "/one"})
	if err != nil {
		t.Fatalf("pushstate: %v", err)
	}
	replaced, err := m.PushState(tabCtx, HistoryStateOptions{URL: "/two", Replace: true})
	if err != nil {
		t.Fatalf("replacestate: %v", err)
	}
	if !replaced.Replaced {
		t.Fatal("replace:true should be reported back")
	}
	if replaced.Length != pushed.Length {
		t.Fatalf("history length went %d -> %d across a replaceState, want it unchanged", pushed.Length, replaced.Length)
	}
	if !strings.HasSuffix(replaced.Previous, "/one") {
		t.Fatalf("previous_url = %q, want the /one entry it replaced", replaced.Previous)
	}
}

// TestPushStateRefusedByNavigationPolicy is the guardrail: a same-document
// history change is a destination like any other, and an off-allowlist one must
// be refused with the policy's own reason — not with the History API's
// same-origin complaint, which would hide why it was actually stopped.
func TestPushStateRefusedByNavigationPolicy(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	m.SetNavigationPolicy(&navpolicy.Policy{Allowed: []string{"127.0.0.1"}})
	tabCtx, _ := openRouterFixture(t, m, ctx)

	before := evalString(t, m, tabCtx, "location.href")
	_, err := m.PushState(tabCtx, HistoryStateOptions{URL: "https://evil.example/dashboard"})
	if err == nil {
		t.Fatal("pushstate to an off-allowlist origin should be refused")
	}
	if !strings.Contains(err.Error(), "navigation policy") {
		t.Fatalf("error %q should say the navigation policy refused it", err)
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("error %q should name the allowlist as the reason", err)
	}
	if after := evalString(t, m, tabCtx, "location.href"); after != before {
		t.Fatalf("the refused pushstate still moved the page from %q to %q", before, after)
	}

	// A route on the allowed origin still works under the same policy.
	if _, err := m.PushState(tabCtx, HistoryStateOptions{URL: "/allowed-route"}); err != nil {
		t.Fatalf("pushstate to an allowed same-origin route: %v", err)
	}
	if path := evalString(t, m, tabCtx, "location.pathname"); path != "/allowed-route" {
		t.Fatalf("location.pathname = %q, want /allowed-route", path)
	}
}

// TestPushStateRefusedFromABlockedDocument is the case where the policy check
// adds a refusal the History API's same-origin rule does not.
//
// While the current document is on-policy the two cannot disagree: a same-origin
// target shares the current host and so passes an allowlist by construction, and
// a cross-origin one fails both. The check earns its place when the tab is
// ALREADY on an off-policy document — a blocklist, or a page opened before the
// policy was set — where a same-origin route change would otherwise be waved
// through as "same document, same origin, fine".
func TestPushStateRefusedFromABlockedDocument(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// Open first, then block: the tab is now sitting on a document the policy
	// would not have let it reach.
	tabCtx, _ := openRouterFixture(t, m, ctx)
	m.SetNavigationPolicy(&navpolicy.Policy{Blocked: []string{"127.0.0.1"}})

	_, err := m.PushState(tabCtx, HistoryStateOptions{URL: "/deeper-into-the-blocked-app"})
	if err == nil {
		t.Fatal("a same-origin route change on a blocked document should be refused; the same-origin rule alone would allow it")
	}
	if !strings.Contains(err.Error(), "navigation policy") {
		t.Fatalf("error %q should say the navigation policy refused it", err)
	}
	if !strings.Contains(err.Error(), "blocked domain") {
		t.Fatalf("error %q should name the blocked domain as the reason", err)
	}
	// The same-origin rule would have passed this target, so its complaint must
	// not be what came back — that is the ordering the check buys.
	if strings.Contains(err.Error(), "cannot change origin") {
		t.Fatalf("error %q is the History API's same-origin complaint, not the policy's reason", err)
	}
	// Nothing was dispatched: the refusal happens before the page is touched, so
	// the tab is still on the document it was on. Reading it back through the
	// manager is not possible here — the URL guard evicts a tab sitting on a
	// blocked document to about:blank the moment anything observes it, which is
	// the outer layer this check backs up.
}

// TestPushStateRefusesCrossOrigin covers the API's own rule, with an
// explanation instead of the SecurityError the page would throw.
func TestPushStateRefusesCrossOrigin(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tabCtx, _ := openRouterFixture(t, m, ctx)

	_, err := m.PushState(tabCtx, HistoryStateOptions{URL: "https://example.test/elsewhere"})
	if err == nil {
		t.Fatal("a cross-origin pushstate should be refused")
	}
	if !strings.Contains(err.Error(), "cannot change origin") {
		t.Fatalf("error %q should explain that pushstate cannot change origin", err)
	}
}

func TestResolveHistoryTarget(t *testing.T) {
	tests := []struct {
		name    string
		current string
		target  string
		want    string
		wantErr bool
	}{
		{name: "absolute path", current: "https://app.test/a/b?x=1", target: "/c", want: "https://app.test/c"},
		{name: "relative path", current: "https://app.test/a/b", target: "c", want: "https://app.test/a/c"},
		{name: "query only", current: "https://app.test/a", target: "?tab=2", want: "https://app.test/a?tab=2"},
		{name: "absolute url", current: "https://app.test/a", target: "https://other.test/x", want: "https://other.test/x"},
		{name: "non http page", current: "about:blank", target: "/c", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveHistoryTarget(tt.current, tt.target)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveHistoryTarget(%q,%q) = %q, want an error", tt.current, tt.target, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveHistoryTarget(%q,%q): %v", tt.current, tt.target, err)
			}
			if got != tt.want {
				t.Fatalf("ResolveHistoryTarget(%q,%q) = %q, want %q", tt.current, tt.target, got, tt.want)
			}
		})
	}
}
