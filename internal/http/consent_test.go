package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/siteconsent"
)

// fixtureConsentKey is an obviously fabricated MAC key for tests.
var fixtureConsentKey = []byte("fixture-http-consent-key-abcdefgh")

// consentController reports an open tab so the act-scope gate has a live page
// origin, and records whether the underlying handler actually ran.
type consentController struct {
	fakeController
	tabURL  string
	clicked bool
}

func (c *consentController) ListTabs(context.Context) ([]browser.Tab, error) {
	return []browser.Tab{{ID: "tab1", URL: c.tabURL, Active: true}}, nil
}

func (c *consentController) Click(context.Context, string) (browser.ActionResult, error) {
	c.clicked = true
	return browser.ActionResult{OK: true}, nil
}

func newConsentServer(t *testing.T) (*Server, *siteconsent.Guard) {
	server, guard, _ := newConsentServerWithController(t, &consentController{tabURL: "https://shop.test/cart"})
	return server, guard
}

func newConsentServerWithController(t *testing.T, ctrl *consentController) (*Server, *siteconsent.Guard, *consentController) {
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
	server := New("", ctrl)
	server.SetSiteConsent(guard)
	return server, guard, ctrl
}

// TestConsentGatesTheHTTPAPIToo is the bypass test. The MCP server and this API
// drive the same controller, so a gate on MCP alone would be walked past by
// calling the daemon's own route - which is exactly what `brw open` does.
func TestConsentGatesTheHTTPAPIToo(t *testing.T) {
	server, guard, ctrl := newConsentServerWithController(t, &consentController{tabURL: "https://shop.test/cart"})

	rec := doJSON(t, server, http.MethodPost, "/api/browser/open", `{"url":"https://ungranted.test/page"}`)
	if rec.Code < http.StatusBadRequest {
		t.Fatalf("an un-granted origin was opened over HTTP: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "https://ungranted.test") || !strings.Contains(rec.Body.String(), "read") {
		t.Fatalf("the refusal must name the origin and the missing scope: %s", rec.Body.String())
	}
	if ctrl.openURL != "" {
		t.Fatalf("the handler ran despite the refusal: opened %q", ctrl.openURL)
	}

	rec = doJSON(t, server, http.MethodPost, "/api/page/click", `{"ref":"e1"}`)
	if rec.Code < http.StatusBadRequest {
		t.Fatalf("an un-granted act was allowed over HTTP: %d %s", rec.Code, rec.Body.String())
	}
	if ctrl.clicked {
		t.Fatal("the click handler ran despite the refusal")
	}

	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://ungranted.test", Scope: siteconsent.ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://shop.test", Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}

	rec = doJSON(t, server, http.MethodPost, "/api/browser/open", `{"url":"https://ungranted.test/page"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a granted origin was still refused: %d %s", rec.Code, rec.Body.String())
	}
	if ctrl.openURL != "https://ungranted.test/page" {
		t.Fatalf("the body did not survive the gate: handler saw %q", ctrl.openURL)
	}
	rec = doJSON(t, server, http.MethodPost, "/api/page/click", `{"ref":"e1"}`)
	if rec.Code != http.StatusOK || !ctrl.clicked {
		t.Fatalf("a granted act was refused: %d %s", rec.Code, rec.Body.String())
	}
}

// TestEveryGatedOperationIsRefusedOverHTTP walks the daemon's own route table
// and proves each route whose operation carries a consent rule is actually
// refused without a grant.
//
// It is the anti-drift test for the two surfaces: the middleware finds the
// operation through usageOperations, so a gated tool whose route is missing from
// that map, or a rule added to the shared table with no route wiring, shows up
// here as a route that answered 200.
//
// The request uses the method the route is REGISTERED with, discovered from the
// Allow header a method mismatch returns. Posting to every route instead would
// count a GET-only route's 405 as proof the gate fired, which is a pass for the
// wrong reason - and the refusal body is checked for the same reason, since only
// the gate names the origin and the missing scope.
func TestEveryGatedOperationIsRefusedOverHTTP(t *testing.T) {
	for route, operation := range usageOperations {
		rule, gated := siteconsent.ToolRules[operation]
		if !gated && !siteconsent.SequenceTools[operation] {
			continue
		}
		for _, method := range registeredMethods(t, route) {
			t.Run(operation+"/"+method, func(t *testing.T) {
				server, _, ctrl := newConsentServerWithController(t, &consentController{tabURL: "https://ungranted.test/cart"})
				body := ""
				if method != http.MethodGet {
					switch {
					case siteconsent.SequenceTools[operation]:
						body = `{"steps":[{"action":"open","url":"https://ungranted.test/x"}]}`
					case rule.Target == siteconsent.TargetURL:
						// Addressed in the argument the RULE declares, so a rule
						// naming a field the tool does not have fails here too.
						body = destinationBody(t, rule.Fields[0])
					default:
						body = `{}`
					}
				}
				rec := doJSON(t, server, method, route, body)
				if rec.Code < http.StatusBadRequest {
					t.Fatalf("%s %s (%s) answered %d without a grant: %s", method, route, operation, rec.Code, rec.Body.String())
				}
				answer := rec.Body.String()
				if !strings.Contains(answer, "https://ungranted.test") {
					t.Fatalf("%s %s (%s) refused with %d but did not name the origin, so this is not the gate refusing: %s", method, route, operation, rec.Code, answer)
				}
				if ctrl.openURL != "" || ctrl.clicked {
					t.Fatalf("%s reached the controller despite the refusal", route)
				}
			})
		}
	}
}

// destinationBody writes an un-granted origin into the argument a rule says the
// tool names its destination in.
func destinationBody(t *testing.T, field siteconsent.DestinationField) string {
	t.Helper()
	switch field {
	case siteconsent.FieldURL:
		return `{"url":"https://ungranted.test/x"}`
	case siteconsent.FieldOrigin:
		return `{"origin":"https://ungranted.test"}`
	case siteconsent.FieldDomain:
		return `{"domain":"ungranted.test"}`
	case siteconsent.FieldOrigins:
		return `{"origins":[{"origin":"https://ungranted.test","headers":{"X-Test":"1"}}]}`
	}
	t.Fatalf("no request body shape for destination field %q", field)
	return ""
}

// registeredMethods asks the route table which methods a path is registered
// with. A mismatch answers 405 with an Allow header, so the test reads the
// methods out of the mux rather than guessing them.
func registeredMethods(t *testing.T, route string) []string {
	t.Helper()
	// A server with no consent guard, so the gate cannot answer before the mux
	// reports the method mismatch.
	server := New("", &consentController{})
	req := httptest.NewRequest("BREW", route, strings.NewReader(""))
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("%s answered %d for an unregistered method; the Allow header is how this test finds the real methods", route, rec.Code)
	}
	var methods []string
	for _, method := range strings.Split(rec.Header().Get("Allow"), ",") {
		method = strings.TrimSpace(method)
		if method != "" && method != http.MethodOptions && method != http.MethodHead {
			methods = append(methods, method)
		}
	}
	if len(methods) == 0 {
		t.Fatalf("%s reports no methods in Allow: %q", route, rec.Header().Get("Allow"))
	}
	return methods
}

// TestEveryAPIRouteIsClassifiedForConsent is the route-table half of the
// exhaustiveness rule: an /api/ route whose operation is in neither half of the
// consent table is a route nothing decided about.
func TestEveryAPIRouteIsClassifiedForConsent(t *testing.T) {
	// The consent surface itself: these routes list and revoke grants rather
	// than drive a site, and gating them behind a grant would make a user unable
	// to revoke without first granting.
	surface := map[string]string{
		"brw_consent_grants": "lists this profile's own grants; it touches no site",
		"brw_consent_revoke": "revokes this profile's own grants; it touches no site",
	}
	for route, operation := range usageOperations {
		_, gated := siteconsent.ToolRules[operation]
		if gated || siteconsent.SequenceTools[operation] {
			continue
		}
		if _, ungated := siteconsent.UngatedTools[operation]; ungated {
			continue
		}
		if surface[operation] != "" {
			continue
		}
		t.Errorf("%s (%s) is in neither siteconsent.ToolRules nor siteconsent.UngatedTools", route, operation)
	}
}

// TestConsentMiddlewareLeavesUngatedRoutesAlone keeps the gate from becoming a
// blanket refusal: a route that drives no site passes through untouched, and a
// gated one still delivers its body to the handler once the origin is granted.
func TestConsentMiddlewareLeavesUngatedRoutesAlone(t *testing.T) {
	ctrl := &consentController{tabURL: "https://shop.test/cart"}
	ctrl.snap = sampleSnapshot()
	server, guard, _ := newConsentServerWithController(t, ctrl)
	rec := doJSON(t, server, http.MethodGet, "/api/browser/tabs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("an ungated route was refused: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, server, http.MethodPost, "/api/page/find", `{"query":"button"}`); rec.Code < http.StatusBadRequest {
		t.Fatalf("a read of an un-granted page was allowed: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://shop.test", Scope: siteconsent.ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	rec = doJSON(t, server, http.MethodPost, "/api/page/find", `{"query":"button"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("find was refused with a read grant: %d %s", rec.Code, rec.Body.String())
	}
	if ctrl.findOpts.Query != "button" {
		t.Fatalf("the buffered body did not reach the handler: %+v", ctrl.findOpts)
	}
}

func doJSON(t *testing.T, server *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)
	return rec
}

func TestConsentGrantsListsRecordsAndProvenance(t *testing.T) {
	server, guard := newConsentServer(t)
	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://one.test", Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, server, http.MethodGet, "/api/consent/grants", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got consentListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || len(got.Grants) != 1 || got.Grants[0].Origin != "https://one.test" {
		t.Fatalf("listing did not report the grant: %+v", got)
	}
	if got.Grants[0].Expired {
		t.Fatal("a grant with no expiry was reported expired")
	}
	if got.Category.Source == "" || got.Category.Update == "" || len(got.Category.Names) == 0 {
		t.Fatalf("listing must carry the blocklist provenance: %+v", got.Category)
	}
}

// TestConsentGrantsReportsForgedRecords proves a hand-written record is both
// refused and visible as a refusal on the surface a person actually looks at.
func TestConsentGrantsReportsForgedRecords(t *testing.T) {
	server, guard := newConsentServer(t)
	path := guard.Store().Path()
	forged := `{"version":1,"grants":[{"origin":"https://attacker.test","scope":"act","decision":"allow","granted_at":"2026-03-01T12:00:00Z","granted_by":"fixture-user","mac":"bm90LWEtcmVhbC1tYWM"}]}`
	if err := os.WriteFile(path, []byte(forged), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, server, http.MethodGet, "/api/consent/grants", "")
	var got consentListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Grants) != 0 {
		t.Fatalf("a forged record was listed as a grant: %+v", got.Grants)
	}
	if len(got.Rejected) != 1 || got.Rejected[0].Origin != "https://attacker.test" {
		t.Fatalf("the refusal was not surfaced: %+v", got.Rejected)
	}
}

func TestConsentRevokeRoutes(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantStatus  int
		wantRemoved float64
		wantLeft    int
	}{
		{name: "one origin", body: `{"origin":"https://one.test"}`, wantStatus: http.StatusOK, wantRemoved: 1, wantLeft: 1},
		{name: "one origin and scope", body: `{"origin":"https://one.test","scope":"act"}`, wantStatus: http.StatusOK, wantRemoved: 1, wantLeft: 1},
		{name: "all", body: `{"all":true}`, wantStatus: http.StatusOK, wantRemoved: 2, wantLeft: 0},
		{name: "unknown origin removes nothing", body: `{"origin":"https://nope.test"}`, wantStatus: http.StatusOK, wantRemoved: 0, wantLeft: 2},
		{name: "neither origin nor all", body: `{}`, wantStatus: http.StatusBadRequest, wantLeft: 2},
		{name: "both origin and all", body: `{"origin":"https://one.test","all":true}`, wantStatus: http.StatusBadRequest, wantLeft: 2},
		{name: "bad scope", body: `{"origin":"https://one.test","scope":"sideways"}`, wantStatus: http.StatusBadRequest, wantLeft: 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server, guard := newConsentServer(t)
			for _, origin := range []string{"https://one.test", "https://two.test"} {
				if _, err := guard.Allow(siteconsent.GrantOptions{Origin: origin, Scope: siteconsent.ScopeAct, Actor: "fixture-user"}); err != nil {
					t.Fatal(err)
				}
			}
			rec := doJSON(t, server, http.MethodPost, "/api/consent/revoke", c.body)
			if rec.Code != c.wantStatus {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.wantStatus, rec.Body.String())
			}
			if c.wantStatus == http.StatusOK {
				var got map[string]any
				if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
					t.Fatal(err)
				}
				if got["removed"] != c.wantRemoved {
					t.Fatalf("removed = %v, want %v", got["removed"], c.wantRemoved)
				}
			}
			if left := len(guard.Store().List()); left != c.wantLeft {
				t.Fatalf("%d grants left, want %d", left, c.wantLeft)
			}
		})
	}
}

func TestConsentRoutesWithoutAGuard(t *testing.T) {
	server := New("", &fakeController{})
	rec := doJSON(t, server, http.MethodGet, "/api/consent/grants", "")
	var got consentListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Enabled || len(got.Grants) != 0 {
		t.Fatalf("an unconfigured daemon must say so rather than imply an empty allowlist: %+v", got)
	}
	rec = doJSON(t, server, http.MethodPost, "/api/consent/revoke", `{"all":true}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("revoking on an unconfigured daemon reported success: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Fatalf("the error does not say consent is unconfigured: %s", rec.Body.String())
	}
}
