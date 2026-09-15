package extensionbridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/siteconsent"
)

// fixtureConsentKey is an obviously fabricated MAC key for tests.
var fixtureConsentKey = []byte("fixture-bridge-consent-key-abcdef")

// fixtureConsentExtensionID is a fabricated 32-character extension id, the
// shape Chrome assigns, used so the origin pin is a real comparison.
const fixtureConsentExtensionID = "fixtureextensionidaaaaaaaaaaaaaa"

func newConsentBridge(t *testing.T) (*Bridge, *siteconsent.Guard) {
	t.Helper()
	store, err := siteconsent.NewStoreWithKey(filepath.Join(t.TempDir(), "site-grants.json"), fixtureConsentKey)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := siteconsent.NewGuard(store, siteconsent.AdminConfig{})
	if err != nil {
		t.Fatal(err)
	}
	guard.SetGrantor("fixture-user")
	// Pin the fixture extension id rather than leaving it empty: the reachability
	// rule now matches a present Origin against the configured extension, so an
	// unpinned bridge would accept any well-formed extension origin and the test
	// would stop exercising the pin it exists to check.
	bridge := New("127.0.0.1:0", time.Second, fixtureConsentExtensionID)
	bridge.SetSiteConsent(guard)
	return bridge, guard
}

func consentRequest(t *testing.T, bridge *Bridge, method, path, origin, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:17311"
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	switch path {
	case "/consent":
		bridge.handleConsent(rec, req)
	default:
		bridge.handleConsentRevoke(rec, req)
	}
	return rec
}

func TestBridgeConsentListsGrants(t *testing.T) {
	bridge, guard := newConsentBridge(t)
	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://one.test", Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	rec := consentRequest(t, bridge, http.MethodGet, "/consent", "chrome-extension://"+fixtureConsentExtensionID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got consentStatus
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || len(got.Grants) != 1 || got.Grants[0].Origin != "https://one.test" {
		t.Fatalf("grant listing: %+v", got)
	}
	if got.Source == "" || got.Update == "" {
		t.Fatalf("the options page must be told where the blocklist comes from: %+v", got)
	}
}

// TestBridgeConsentIsNotServedToAWebPage keeps the grant list off any origin a
// user might be browsing: it is a list of the sites they have said yes to.
func TestBridgeConsentIsNotServedToAWebPage(t *testing.T) {
	bridge, guard := newConsentBridge(t)
	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://one.test", Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		origin string
		host   string
	}{
		{name: "a web page origin", origin: "https://evil.test", host: "127.0.0.1:17311"},
		{name: "a rebound host", origin: "", host: "attacker.test"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/consent", nil)
			req.Host = c.host
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			rec := httptest.NewRecorder()
			bridge.handleConsent(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "one.test") {
				t.Fatalf("the grant list leaked: %s", rec.Body.String())
			}
		})
	}
}

func TestBridgeConsentRevoke(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantLeft   int
	}{
		{name: "one origin", body: `{"origin":"https://one.test"}`, wantStatus: http.StatusOK, wantLeft: 1},
		{name: "all", body: `{"all":true}`, wantStatus: http.StatusOK, wantLeft: 0},
		{name: "neither", body: `{}`, wantStatus: http.StatusBadRequest, wantLeft: 2},
		{name: "unknown field", body: `{"origins":"https://one.test"}`, wantStatus: http.StatusBadRequest, wantLeft: 2},
		{name: "bad scope", body: `{"origin":"https://one.test","scope":"sideways"}`, wantStatus: http.StatusBadRequest, wantLeft: 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bridge, guard := newConsentBridge(t)
			for _, origin := range []string{"https://one.test", "https://two.test"} {
				if _, err := guard.Allow(siteconsent.GrantOptions{Origin: origin, Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
					t.Fatal(err)
				}
			}
			rec := consentRequest(t, bridge, http.MethodPost, "/consent/revoke", "chrome-extension://"+fixtureConsentExtensionID, c.body)
			if rec.Code != c.wantStatus {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			if left := len(guard.Store().List()); left != c.wantLeft {
				t.Fatalf("%d grants left, want %d", left, c.wantLeft)
			}
		})
	}
}

func TestBridgeConsentWithoutAGuard(t *testing.T) {
	bridge := New("127.0.0.1:0", time.Second, fixtureConsentExtensionID)
	rec := consentRequest(t, bridge, http.MethodGet, "/consent", "chrome-extension://"+fixtureConsentExtensionID, "")
	var got consentStatus
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("a daemon with no consent store reported consent enabled")
	}
	rec = consentRequest(t, bridge, http.MethodPost, "/consent/revoke", "chrome-extension://"+fixtureConsentExtensionID, `{"all":true}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("revoking with no store reported success: %s", rec.Body.String())
	}
}
