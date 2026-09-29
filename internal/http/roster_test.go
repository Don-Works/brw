package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/profilepolicy"
	"github.com/Don-Works/brw/internal/profileroster"
	"github.com/Don-Works/brw/internal/setup"
	"github.com/Don-Works/brw/internal/siteconsent"
)

// jarController is a browser with a real cookie jar behind brw_cookies, so a
// copy through the daemon's own middleware can be observed end to end.
type jarController struct {
	fakeController
	mu  sync.Mutex
	jar []browser.Cookie
	ops []string
}

func (c *jarController) Cookies(_ context.Context, p browser.CookieParams) (browser.CookieResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ops = append(c.ops, p.Action+" "+p.Name)
	host := ""
	if u, err := url.Parse(p.URL); err == nil {
		host = u.Hostname()
	}
	switch p.Action {
	case browser.CookieActionSet:
		domain := p.Domain
		if domain == "" {
			domain = host
		}
		stored := browser.Cookie{Name: p.Name, Value: p.Value, Domain: domain, Path: p.Path, Secure: p.Secure, HTTPOnly: p.HTTPOnly}
		c.jar = append(c.jar, stored)
		return browser.CookieResult{Action: p.Action, Cookie: &stored, Cookies: []browser.Cookie{stored}}, nil
	case browser.CookieActionDelete:
		kept := c.jar[:0]
		for _, k := range c.jar {
			if k.Name != p.Name {
				kept = append(kept, k)
			}
		}
		c.jar = kept
		return browser.CookieResult{Action: p.Action}, nil
	}
	var out []browser.Cookie
	for _, k := range c.jar {
		d := strings.TrimPrefix(k.Domain, ".")
		if host == d || strings.HasSuffix(host, "."+d) {
			out = append(out, k)
		}
	}
	return browser.CookieResult{Action: p.Action, Cookies: out, Count: len(out)}, nil
}

func (c *jarController) writes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, op := range c.ops {
		if !strings.HasPrefix(op, "list") {
			out = append(out, op)
		}
	}
	return out
}

type rosterDaemon struct {
	server *Server
	ctrl   *jarController
	http   *httptest.Server
}

func startRosterDaemon(t *testing.T, name string, jar ...browser.Cookie) *rosterDaemon {
	t.Helper()
	ctrl := &jarController{jar: jar}
	id := brwidentity.Identity{Profile: name, Transport: brwidentity.TransportDirectCDP, UserDataDir: filepath.Join(t.TempDir(), name)}
	server := NewWithIdentity("127.0.0.1:0", ctrl, id)
	server.SetProfileRoster(profileroster.Service{})
	ts := httptest.NewServer(server.Handler())
	t.Cleanup(ts.Close)
	return &rosterDaemon{server: server, ctrl: ctrl, http: ts}
}

func (d *rosterDaemon) addr() string { return strings.TrimPrefix(d.http.URL, "http://") }

func writeRosterPolicy(t *testing.T, daemons map[string]*rosterDaemon) string {
	t.Helper()
	var policy profilepolicy.Policy
	for name, d := range daemons {
		policy.Profiles = append(policy.Profiles, profilepolicy.Profile{
			Name: name, DirectCDPAllowed: true, BridgeHTTPAddr: d.addr(),
			UserDataDir: filepath.Join(t.TempDir(), name),
		})
	}
	path := filepath.Join(t.TempDir(), "browser-profiles.json")
	if _, err := setup.WritePolicy(path, policy, time.Now()); err != nil {
		t.Fatal(err)
	}
	return path
}

func postRoster(t *testing.T, base, path string, body any) (int, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(base+path, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestRosterCopyIsGatedByTheDestinationsConsent(t *testing.T) {
	session := browser.Cookie{Name: "SID", Value: "secret-session-value", Domain: ".example.test", Path: "/", Secure: true, HTTPOnly: true}
	src := startRosterDaemon(t, "source", session)
	dst := startRosterDaemon(t, "dest")
	store, err := siteconsent.NewStoreWithKey(filepath.Join(t.TempDir(), "site-grants.json"), fixtureConsentKey)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := siteconsent.NewGuard(store, siteconsent.AdminConfig{})
	if err != nil {
		t.Fatal(err)
	}
	guard.SetGrantor("fixture-user")
	dst.server.SetSiteConsent(guard)
	src.server.SetProfilePolicyPath(writeRosterPolicy(t, map[string]*rosterDaemon{"source": src, "dest": dst}))

	req := map[string]string{"from": "source", "to": "dest", "domain": "example.test", "mode": "move"}
	code, body := postRoster(t, src.http.URL, "/api/roster/copy", req)
	if code < http.StatusBadRequest || !strings.Contains(body["error"].(string), "https://example.test") {
		t.Fatalf("an ungranted copy answered %d %v", code, body)
	}
	if w := dst.ctrl.writes(); len(w) != 0 {
		t.Fatalf("the destination was written without a grant: %v", w)
	}
	if w := src.ctrl.writes(); len(w) != 0 || len(src.ctrl.jar) != 1 {
		t.Fatalf("the source changed after a refused move: %v", w)
	}

	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://example.test", Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	code, body = postRoster(t, src.http.URL, "/api/roster/copy", req)
	if code != http.StatusOK || body["copied"] != float64(1) || body["removed"] != float64(1) {
		t.Fatalf("granted move answered %d %v", code, body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "secret-session-value") {
		t.Fatalf("the roster returned a cookie value: %s", raw)
	}
	if len(dst.ctrl.jar) != 1 || dst.ctrl.jar[0].Value != session.Value || len(src.ctrl.jar) != 0 {
		t.Fatalf("after the move src=%+v dst=%+v", src.ctrl.jar, dst.ctrl.jar)
	}
}

func TestRosterRoutesServeLoopbackOnly(t *testing.T) {
	server := New("127.0.0.1:0", &fakeController{})
	server.SetProfileRoster(profileroster.Service{})
	server.SetProfilePolicyPath(filepath.Join(t.TempDir(), "absent.json"))
	routes := []struct{ method, path string }{
		{http.MethodGet, "/profiles"},
		{http.MethodGet, "/profiles/roster.js"},
		{http.MethodGet, "/api/roster/board"},
		{http.MethodPost, "/api/roster/profiles"},
		{http.MethodPost, "/api/roster/copy"},
		{http.MethodPost, "/api/roster/pin"},
		{http.MethodPost, "/api/roster/open"},
	}
	for _, rt := range routes {
		req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(`{"name":"x","from":"a","to":"b","domain":"example.test"}`))
		req.Host = "127.0.0.1"
		req.RemoteAddr = "100.64.0.9:51000"
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "loopback") {
			t.Errorf("%s %s from a tailnet peer answered %d %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}

func TestRosterRoutesAreAbsentWithoutARoster(t *testing.T) {
	server := New("127.0.0.1:0", &fakeController{})
	req := httptest.NewRequest(http.MethodGet, "/api/roster/board", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:51000"
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("board without a roster answered %d %s", rec.Code, rec.Body.String())
	}
}

func TestRosterRefusesACrossSiteBrowserRequest(t *testing.T) {
	server := New("127.0.0.1:0", &fakeController{})
	server.SetProfileRoster(profileroster.Service{})
	policyPath := filepath.Join(t.TempDir(), "browser-profiles.json")
	server.SetProfilePolicyPath(policyPath)
	req := httptest.NewRequest(http.MethodPost, "/api/roster/profiles", strings.NewReader(`{"name":"planted"}`))
	req.Host = "127.0.0.1:17310"
	req.RemoteAddr = "127.0.0.1:51000"
	req.Header.Set("Origin", "https://evil.test")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a cross-site page created a profile: %d %s", rec.Code, rec.Body.String())
	}
	if _, found, _ := setup.LoadPolicyFile(policyPath); found {
		t.Fatal("the policy was written")
	}
}

func TestRosterPageIsSelfContained(t *testing.T) {
	server := New("127.0.0.1:0", &fakeController{})
	server.SetProfileRoster(profileroster.Service{})
	req := httptest.NewRequest(http.MethodGet, "/profiles", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:51000"
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `src="/profiles/roster.js"`) {
		t.Fatalf("page answered %d", rec.Code)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("CSP = %q", csp)
	}
}

func TestRosterOpenGoesThroughTheProfilesOwnGate(t *testing.T) {
	target := startRosterDaemon(t, "agent")
	store, err := siteconsent.NewStoreWithKey(filepath.Join(t.TempDir(), "site-grants.json"), fixtureConsentKey)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := siteconsent.NewGuard(store, siteconsent.AdminConfig{})
	if err != nil {
		t.Fatal(err)
	}
	guard.SetGrantor("fixture-user")
	target.server.SetSiteConsent(guard)
	target.server.SetProfilePolicyPath(writeRosterPolicy(t, map[string]*rosterDaemon{"agent": target}))

	code, body := postRoster(t, target.http.URL, "/api/roster/open", map[string]string{"profile": "agent", "url": "https://ungranted.test/login"})
	if code < http.StatusBadRequest || target.ctrl.openURL != "" {
		t.Fatalf("an ungranted open answered %d %v and reached %q", code, body, target.ctrl.openURL)
	}
	if code, _ := postRoster(t, target.http.URL, "/api/roster/open", map[string]string{"profile": "agent", "url": "javascript:alert(1)"}); code != http.StatusBadRequest {
		t.Fatalf("a javascript: URL answered %d", code)
	}
}
