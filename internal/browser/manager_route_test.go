package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/chromedp/cdproto/target"
)

func TestMatchURLGlob(t *testing.T) {
	tests := []struct {
		pattern string
		url     string
		want    bool
	}{
		{"*", "https://example.com/a", true},
		{"", "https://example.com/a", true},
		{"https://api.example.com/v1/*", "https://api.example.com/v1/users", true},
		// A URL glob is not a path glob: "*" must cross "/" or the common
		// "host/*" pattern would miss every nested path.
		{"https://api.example.com/*", "https://api.example.com/v1/users/42", true},
		{"https://api.example.com/v1/*", "https://api.example.com/v2/users", false},
		{"*/users", "https://api.example.com/v1/users", true},
		{"*/users", "https://api.example.com/v1/users/42", false},
		{"*.json", "https://cdn.example.com/data/config.json", true},
		{"*.json", "https://cdn.example.com/data/config.xml", false},
		// A wildcard-free pattern matches as a prefix, so a caller need not
		// remember the query string.
		{"https://api.example.com/search", "https://api.example.com/search?q=hi", true},
		{"https://api.example.com/search", "https://api.example.com/other", false},
		{"https://a.com/*/edit", "https://a.com/docs/42/edit", true},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+" vs "+tt.url, func(t *testing.T) {
			if got := matchURLGlob(tt.pattern, tt.url); got != tt.want {
				t.Fatalf("matchURLGlob(%q, %q) = %v, want %v", tt.pattern, tt.url, got, tt.want)
			}
		})
	}
}

func routeFixtureTab(t *testing.T, m *Manager, ctx context.Context) string {
	t.Helper()
	var id target.ID
	if err := m.runBrowser(ctx, func(rc context.Context) error {
		var e error
		id, e = target.CreateTarget("about:blank").Do(rc)
		return e
	}); err != nil {
		t.Fatalf("create target: %v", err)
	}
	m.refs.SetActive(string(id))
	if _, err := m.tabContext(string(id)); err != nil {
		t.Fatalf("tab context: %v", err)
	}
	return string(id)
}

func TestRouteFulfillsAndAbortsWithoutTouchingTheNetwork(t *testing.T) {
	var apiHits, analyticsHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/user", func(w http.ResponseWriter, _ *http.Request) {
		apiHits++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"real"}`)
	})
	mux.HandleFunc("/analytics", func(w http.ResponseWriter, _ *http.Request) {
		analyticsHits++
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "tracked")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><script>
			window.__api = 'pending'; window.__an = 'pending';
			fetch('/api/user').then(function(r){return r.json();})
			  .then(function(j){ window.__api = j.name; })
			  .catch(function(){ window.__api = 'failed'; });
			fetch('/analytics').then(function(){ window.__an = 'reached'; })
			  .catch(function(){ window.__an = 'aborted'; });
			</script></body></html>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	tabID := routeFixtureTab(t, m, ctx)

	if _, err := m.Route(ctx, RouteOptions{
		Action: "add", TabID: tabID,
		Pattern: "*/api/user", Behaviour: "fulfill",
		Body: `{"name":"mocked"}`,
	}); err != nil {
		t.Fatalf("add fulfill route: %v", err)
	}
	if _, err := m.Route(ctx, RouteOptions{
		Action: "add", TabID: tabID,
		Pattern: "*/analytics", Behaviour: "abort",
	}); err != nil {
		t.Fatalf("add abort route: %v", err)
	}

	if _, err := m.NavigateTo(ctx, srv.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := m.WaitFor(ctx, "fn:window.__api !== 'pending' && window.__an !== 'pending'", 15*time.Second); err != nil {
		t.Fatalf("fetches never settled: %v", err)
	}

	api, _ := m.Evaluate(ctx, `window.__api`)
	if got, _ := api.(string); got != "mocked" {
		t.Errorf("page saw %q, want the mocked body", got)
	}
	if apiHits != 0 {
		t.Errorf("a fulfilled route still reached the server %d times", apiHits)
	}
	an, _ := m.Evaluate(ctx, `window.__an`)
	if got, _ := an.(string); got != "aborted" {
		t.Errorf("aborted fetch = %q, want aborted", got)
	}
	if analyticsHits != 0 {
		t.Errorf("an aborted route still reached the server %d times", analyticsHits)
	}

	listed, err := m.Route(ctx, RouteOptions{Action: "list", TabID: tabID})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if listed.Count != 2 {
		t.Fatalf("listed %d routes, want 2", listed.Count)
	}
	for _, r := range listed.Routes {
		if r.Matched == 0 {
			t.Errorf("route %q reports 0 matches; a caller cannot tell a working mock from a request that never happened", r.Pattern)
		}
	}
}

// A route must never become a way around the navigation policy.
func TestRouteCannotBypassContainment(t *testing.T) {
	var offHits int
	off := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		offHits++
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprint(w, "leaked")
	}))
	defer off.Close()
	offURL := asLocalhost(off.URL)

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><body><script>
			window.__r = 'pending';
			fetch(%q).then(function(r){return r.text();})
			  .then(function(t){ window.__r = 'reached:' + t; })
			  .catch(function(){ window.__r = 'blocked'; });
			</script></body></html>`, offURL+"/exfil")
	}))
	defer page.Close()

	m := newHeadlessManager(t)
	m.SetNavigationPolicy(&navpolicy.Policy{Allowed: []string{hostOfTestURL(t, page.URL)}})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	tabID := routeFixtureTab(t, m, ctx)

	// A route that tries to answer for the forbidden host must not make the
	// request happen, and must not be consulted at all.
	if _, err := m.Route(ctx, RouteOptions{
		Action: "add", TabID: tabID, Pattern: "*/exfil", Behaviour: "fulfill", Body: "mocked",
	}); err != nil {
		t.Fatalf("add route: %v", err)
	}
	if _, err := m.NavigateTo(ctx, page.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	if err := m.WaitFor(ctx, "fn:window.__r !== 'pending'", 15*time.Second); err != nil {
		t.Fatalf("fetch never settled: %v", err)
	}
	value, _ := m.Evaluate(ctx, `window.__r`)
	if got, _ := value.(string); got != "blocked" {
		t.Errorf("result = %q, want blocked: containment must win over a route", got)
	}
	if offHits != 0 {
		t.Errorf("off-allowlist host was reached %d times", offHits)
	}
}

func TestRouteTimesRetiresTheRule(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tabID := routeFixtureTab(t, m, ctx)

	if _, err := m.Route(ctx, RouteOptions{
		Action: "add", TabID: tabID, Pattern: "https://x/once", Behaviour: "fulfill", Body: "one", Times: 1,
	}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if hit := m.routes.match(tabID, "https://x/once"); hit == nil {
		t.Fatal("first match should hit")
	}
	if hit := m.routes.match(tabID, "https://x/once"); hit != nil {
		t.Fatal("a times=1 route must retire after one match")
	}
	if m.routes.count(tabID) != 0 {
		t.Fatal("retired route should be gone from the table")
	}
}

func TestRouteValidationAndClear(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tabID := routeFixtureTab(t, m, ctx)

	if _, err := m.Route(ctx, RouteOptions{Action: "add", TabID: tabID}); err == nil ||
		!strings.Contains(err.Error(), "pattern") {
		t.Fatalf("add without a pattern should be refused, got %v", err)
	}
	if _, err := m.Route(ctx, RouteOptions{Action: "add", TabID: tabID, Pattern: "*", Behaviour: "teleport"}); err == nil ||
		!strings.Contains(err.Error(), "unknown route behaviour") {
		t.Fatalf("unknown behaviour should be refused, got %v", err)
	}
	if _, err := m.Route(ctx, RouteOptions{Action: "wat", TabID: tabID}); err == nil ||
		!strings.Contains(err.Error(), "unknown route action") {
		t.Fatalf("unknown action should be refused, got %v", err)
	}

	for _, pattern := range []string{"https://a/*", "https://b/*"} {
		if _, err := m.Route(ctx, RouteOptions{Action: "add", TabID: tabID, Pattern: pattern, Body: "x"}); err != nil {
			t.Fatalf("add %s: %v", pattern, err)
		}
	}
	cleared, err := m.Route(ctx, RouteOptions{Action: "clear", TabID: tabID, Pattern: "https://a/*"})
	if err != nil {
		t.Fatalf("clear one: %v", err)
	}
	if cleared.Count != 1 {
		t.Fatalf("after clearing one pattern %d routes remain, want 1", cleared.Count)
	}
	all, err := m.Route(ctx, RouteOptions{Action: "clear", TabID: tabID})
	if err != nil {
		t.Fatalf("clear all: %v", err)
	}
	if all.Count != 0 {
		t.Fatalf("clear-all left %d routes", all.Count)
	}
}

// Fulfilling a JSON body without naming a content type is the common case.
func TestRouteDefaultsContentType(t *testing.T) {
	tests := []struct {
		pattern string
		body    string
		want    string
	}{
		{"https://x/api", `{"a":1}`, "application/json"},
		{"https://x/api", `[1,2]`, "application/json"},
		{"https://x/data.json", ``, "application/json"},
		{"https://x/page.html", `<p>hi</p>`, "text/html"},
		{"https://x/app.js", `var a=1`, "text/javascript"},
		{"https://x/thing", `plain`, "text/plain"},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+tt.body, func(t *testing.T) {
			route, err := buildRoute(RouteOptions{Action: "add", Pattern: tt.pattern, Body: tt.body})
			if err != nil {
				t.Fatalf("buildRoute: %v", err)
			}
			if route.ContentType != tt.want {
				t.Fatalf("content type = %q, want %q", route.ContentType, tt.want)
			}
			if route.Status != 200 {
				t.Fatalf("default status = %d, want 200", route.Status)
			}
		})
	}
}

// routeInterceptionFixture is a plain page on the fixture's own origin, so the
// fetch the assertions run is same-origin and cannot be refused by CORS.
const routeInterceptionFixture = `<html><head><title>route</title></head><body><h1>route</h1></body></html>`

// Request interception is turned back OFF once nothing on a tab needs it, and
// armInterception installs a tab's listener once and returns early ever after.
// So brw_route has to bring interception back itself: without that the route is
// accepted, listed and reported as active while every matching request goes to
// the real network — a mocked test passing against production.
func TestRouteArmsInterceptionOnATabThatTurnedItOff(t *testing.T) {
	const fixtureUser, fixturePassword = "fixture-user", "fixture-pw-a1b2"

	tests := []struct {
		name    string
		turnOff func(t *testing.T, m *Manager, ctx context.Context, tabID, origin string)
	}{
		{
			name: "after an authenticate that has finished",
			turnOff: func(t *testing.T, m *Manager, ctx context.Context, tabID, origin string) {
				t.Helper()
				if _, err := m.Authenticate(ctx, CredentialsOptions{
					TabID:    tabID,
					Origin:   origin,
					Username: fixtureUser,
					Password: fixturePassword,
					URL:      origin + "/protected",
				}); err != nil {
					t.Fatalf("authenticate: %v", err)
				}
			},
		},
		{
			name: "after the extra header table is cleared",
			turnOff: func(t *testing.T, m *Manager, ctx context.Context, tabID, origin string) {
				t.Helper()
				if _, err := m.SetExtraHeaders(ctx, ExtraHeadersOptions{
					TabID:   tabID,
					Origins: []OriginHeaders{{Origin: origin, Headers: map[string]string{"X-Fixture": "1"}}},
				}); err != nil {
					t.Fatalf("set extra headers: %v", err)
				}
				settleInterception()
				if _, err := m.SetExtraHeaders(ctx, ExtraHeadersOptions{TabID: tabID, Clear: true}); err != nil {
					t.Fatalf("clear extra headers: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var apiHits int64
			mux := http.NewServeMux()
			mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt64(&apiHits, 1)
				w.Header().Set("Content-Type", "text/plain")
				fmt.Fprint(w, "from-the-network")
			})
			mux.HandleFunc("/protected", func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				if !ok || user != fixtureUser || password != fixturePassword {
					w.Header().Set("WWW-Authenticate", `Basic realm="fixture"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, routeInterceptionFixture)
			})
			mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, routeInterceptionFixture)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			m := newHeadlessManager(t)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			tabID := routeFixtureTab(t, m, ctx)
			if _, err := m.NavigateTo(ctx, srv.URL); err != nil {
				t.Fatalf("navigate: %v", err)
			}

			tt.turnOff(t, m, ctx, tabID, srv.URL)
			settleInterception()

			if _, err := m.Route(ctx, RouteOptions{
				Action: "add", TabID: tabID,
				Pattern: srv.URL + "/api", Behaviour: "fulfill",
				Body: "mocked-by-brw", Status: 200,
			}); err != nil {
				t.Fatalf("route add: %v", err)
			}

			got := evaluateString(t, m, ctx, fmt.Sprintf(`fetch(%q).then(r => r.text())`, srv.URL+"/api"))
			if got != "mocked-by-brw" {
				t.Fatalf("the page read %q and the server was hit %d time(s); the route was accepted but never intercepted", got, atomic.LoadInt64(&apiHits))
			}
			if hits := atomic.LoadInt64(&apiHits); hits != 0 {
				t.Fatalf("a fulfilled route still reached the server %d time(s)", hits)
			}
		})
	}
}

// settleInterception waits for the enable armInterception schedules on its own
// goroutine. It is the difference between this test observing "the route never
// fired" and racing a late enable that would answer the request anyway and hide
// the defect.
func settleInterception() {
	time.Sleep(1500 * time.Millisecond)
}
