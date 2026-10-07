package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/approvalgate"
	"github.com/Don-Works/brw/internal/browser"
)

const fixtureAPIApprovalToken = "fixture-api-token-distinct-from-operator-123456789"

type approvalAuthController struct {
	browser.Controller
	calls int
}

func (c *approvalAuthController) ListTabs(context.Context) ([]browser.Tab, error) {
	c.calls++
	return []browser.Tab{{ID: "fixture-tab", URL: "https://example.test", Active: true}}, nil
}

func (c *approvalAuthController) Evaluate(context.Context, string) (any, error) {
	c.calls++
	return map[string]any{"url": "https://example.test", "complete": true}, nil
}

func (c *approvalAuthController) Click(context.Context, string) (browser.ActionResult, error) {
	c.calls++
	return browser.ActionResult{OK: true}, nil
}

func TestApprovalAndAPITokensHaveSeparateRegisteredRoutes(t *testing.T) {
	s, request := newApprovalHTTPTest(t)
	controller := &approvalAuthController{}
	s.manager = controller
	gate, err := approvalgate.New(controller, s.approvals, "all")
	if err != nil {
		t.Fatal(err)
	}
	s.SetApprovalGate(gate)
	s.SetAuthToken(fixtureAPIApprovalToken)
	decisionPath := "/operator/approvals/" + request.ID + "/decision"
	for _, token := range []string{"", fixtureAPIApprovalToken, "fixture-wrong-token"} {
		for _, call := range []struct{ method, path, body string }{
			{"GET", "/operator/approvals", ""},
			{"POST", decisionPath, `{"decision":"approved"}`},
		} {
			got := serveApprovalHTTP(s, call.method, call.path, call.body, token)
			if got.Code != http.StatusUnauthorized || controller.calls != 0 {
				t.Fatalf("operator auth preceded by browser access: status=%d calls=%d", got.Code, controller.calls)
			}
		}
	}
	for _, path := range []string{"/api/page/click", "/api/watchers/register", "/api/approvals/" + request.ID, "/operator/approvals/", "/operator/approvals/missing/unknown", "/%6fperator/approvals", "/operator//approvals"} {
		method, body := "GET", ""
		if path == "/api/page/click" || path == "/api/watchers/register" {
			method, body = "POST", `{"tab_id":"fixture-tab","ref":"e1"}`
		}
		for _, token := range []string{"", testApprovalOperatorToken} {
			got := serveApprovalHTTP(s, method, path, body, token)
			if got.Code != http.StatusUnauthorized || controller.calls != 0 {
				t.Fatalf("API path escaped API auth: path=%s status=%d calls=%d", path, got.Code, controller.calls)
			}
		}
	}
	for _, call := range []struct{ method, path string }{{"HEAD", "/approvals"}, {"HEAD", "/operator/approvals"}, {"PUT", decisionPath}, {"GET", decisionPath}, {"POST", decisionPath + "/"}} {
		got := serveApprovalHTTP(s, call.method, call.path, `{"decision":"approved"}`, testApprovalOperatorToken)
		if got.Code != http.StatusUnauthorized || controller.calls != 0 {
			t.Fatalf("wrong operator method escaped API auth: %s %s status=%d calls=%d", call.method, call.path, got.Code, controller.calls)
		}
	}
	if got := serveApprovalHTTP(s, "GET", "/operator/approvals", "", testApprovalOperatorToken); got.Code != http.StatusOK {
		t.Fatalf("independent operator list failed: %d", got.Code)
	}
	if got := serveApprovalHTTP(s, "POST", decisionPath, `{"decision":"approved"}`, testApprovalOperatorToken); got.Code != http.StatusOK || controller.calls != 0 {
		t.Fatalf("operator decision invoked browser prechecks: status=%d calls=%d", got.Code, controller.calls)
	}
	current, _ := s.approvals.Get(request.ID)
	if current.Status != "approved" || current.DecisionBy != "operator" {
		t.Fatalf("operator attribution is not server-owned: status=%s actor=%s", current.Status, current.DecisionBy)
	}
	if got := serveApprovalHTTP(s, "GET", "/api/approvals/"+request.ID, "", fixtureAPIApprovalToken); got.Code != http.StatusOK {
		t.Fatalf("API-token status failed: %d", got.Code)
	}
}

func TestApprovalRouteCompositionRetainsHTTPBoundaryChecks(t *testing.T) {
	for _, boundary := range []string{"duplicate authorization", "duplicate origin", "foreign origin", "remote peer", "disabled inbox"} {
		t.Run(boundary, func(t *testing.T) {
			s, request := newApprovalHTTPTest(t)
			s.SetAuthToken(fixtureAPIApprovalToken)
			r := httptest.NewRequest("POST", "http://127.0.0.1:17310/operator/approvals/"+request.ID+"/decision", strings.NewReader(`{"decision":"approved"}`))
			r.RemoteAddr = "127.0.0.1:40001"
			r.Header.Set("Authorization", "Bearer "+testApprovalOperatorToken)
			r.Header.Set("Content-Type", "application/json")
			want := http.StatusForbidden
			switch boundary {
			case "duplicate authorization":
				r.Header.Add("Authorization", "Bearer "+testApprovalOperatorToken)
				want = http.StatusUnauthorized
			case "duplicate origin":
				r.Header.Add("Origin", "http://127.0.0.1:17310")
				r.Header.Add("Origin", "http://127.0.0.1:17310")
			case "foreign origin":
				r.Header.Set("Origin", "https://example.test")
			case "remote peer":
				r.RemoteAddr = "203.0.113.2:40001"
			case "disabled inbox":
				s.approvals = nil
				want = http.StatusUnauthorized
			}
			w := httptest.NewRecorder()
			s.server.Handler.ServeHTTP(w, r)
			if w.Code != want {
				t.Fatalf("boundary status=%d want=%d", w.Code, want)
			}
		})
	}
}
