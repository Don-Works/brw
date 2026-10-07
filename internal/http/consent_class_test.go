package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/siteconsent"
	"github.com/Don-Works/brw/internal/usagelog"
)

func TestConsentRefusalsAreClassifiedPolicyDenied(t *testing.T) {
	refusals := []struct {
		name   string
		method string
		route  string
		body   string
	}{

		{name: "navigation", method: http.MethodPost, route: "/api/browser/open", body: `{"url":"https://ungranted.test/page"}`},

		{name: "page read", method: http.MethodPost, route: "/api/page/find", body: `{"query":"button"}`},

		{name: "local target", method: http.MethodPost, route: "/api/browser/open", body: `{"url":"file:///etc/hosts"}`},
	}
	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			server, _, _ := newConsentServerWithController(t, &consentController{tabURL: "https://shop.test/cart"})
			rec := doJSON(t, server, refusal.method, refusal.route, refusal.body)
			if rec.Code < http.StatusBadRequest {
				t.Fatalf("the call was not refused: %d %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get(usagelog.HeaderErrorClass); got != "policy_denied" {
				t.Fatalf("refusal class = %q, want policy_denied; an unattended caller cannot tell this from a broken daemon", got)
			}
		})
	}

	granted := &consentController{tabURL: "https://shop.test/cart"}
	granted.snap = sampleSnapshot()
	server, guard, _ := newConsentServerWithController(t, granted)
	if _, err := guard.Allow(siteconsent.GrantOptions{Origin: "https://shop.test", Scope: siteconsent.ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	rec := doJSON(t, server, http.MethodPost, "/api/page/find", `{"query":"button"}`)
	if rec.Code >= http.StatusBadRequest {
		t.Fatalf("a granted read was refused: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(usagelog.HeaderErrorClass); got != "" {
		t.Fatalf("a successful call carries error class %q", got)
	}
}

func TestHealthReportsWhetherThisDaemonCanPrompt(t *testing.T) {
	server, guard, _ := newConsentServerWithController(t, &consentController{tabURL: "https://shop.test/cart"})

	rec := doJSON(t, server, http.MethodGet, "/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"consent"`, `"enabled":true`, `"interactive":false`} {
		if !strings.Contains(body, want) {
			t.Fatalf("/health does not report %s: %s", want, body)
		}
	}

	guard.SetPrompter(&stubPrompter{})
	rec = doJSON(t, server, http.MethodGet, "/health", "")
	if !strings.Contains(rec.Body.String(), `"interactive":true`) {
		t.Fatalf("/health does not report a daemon that can prompt: %s", rec.Body.String())
	}

	plain := New("", &consentController{})
	rec = doJSON(t, plain, http.MethodGet, "/health", "")
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("/health on a daemon without consent: %s", rec.Body.String())
	}
}

type stubPrompter struct{}

func (stubPrompter) AskSite(string, siteconsent.Scope) (bool, error) { return false, nil }
func (stubPrompter) ConfirmAction(siteconsent.ActionRequest, []siteconsent.Risk) (bool, error) {
	return false, nil
}
