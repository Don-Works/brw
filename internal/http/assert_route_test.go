package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// gettersController answers the getters script with canned page values so the
// route test covers decoding, tab routing and evaluation rather than a stub
// that simply returns success.
type gettersController struct {
	fakeController
	values map[string]any
	tabIDs []string
}

func (c *gettersController) Evaluate(ctx context.Context, expression string) (any, error) {
	c.tabIDs = append(c.tabIDs, browser.TabIDFromContext(ctx))
	if value, ok := c.values[expression]; ok {
		return value, nil
	}
	return map[string]any{"value": nil}, nil
}

func newGettersController() *gettersController {
	return &gettersController{values: map[string]any{
		snapshot.BuildGetExpression("url", "", ""): map[string]any{"value": "https://example.test/dashboard"},
		snapshot.BuildGetExpression("state", "e4", ""): map[string]any{
			"value": map[string]any{"found": true, "enabled": false, "editable": false, "checked": false, "focused": false},
		},
	}}
}

func postAssert(t *testing.T, server *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/page/assert", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)
	return rec
}

func TestAssertRouteEvaluatesAndReportsExpectedAgainstActual(t *testing.T) {
	ctrl := newGettersController()
	server := New("", ctrl)

	rec := postAssert(t, server, `{"assertion":"url","mode":"prefix","expected":"https://example.test","tab_id":"tab-7"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var passed browser.AssertResult
	if err := json.NewDecoder(rec.Body).Decode(&passed); err != nil {
		t.Fatal(err)
	}
	if !passed.OK || passed.Assertion != browser.AssertionURL {
		t.Fatalf("result = %+v", passed)
	}
	if len(ctrl.tabIDs) == 0 || ctrl.tabIDs[0] != "tab-7" {
		t.Fatalf("tab ids = %v, want the route to carry tab-7 into the controller", ctrl.tabIDs)
	}

	rec = postAssert(t, server, `{"assertion":"url","expected":"https://example.test/settings"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	var failed struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&failed); err != nil {
		t.Fatal(err)
	}
	want := `url assertion failed: expected exact "https://example.test/settings", actual "https://example.test/dashboard"`
	if failed.Error != want {
		t.Fatalf("error = %q, want %q", failed.Error, want)
	}
}

func TestAssertRouteCarriesElementStateFailures(t *testing.T) {
	server := New("", newGettersController())

	rec := postAssert(t, server, `{"assertion":"element_state","ref":"e4","state":"enabled"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	var failed struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&failed); err != nil {
		t.Fatal(err)
	}
	want := `element state assertion failed: expected ref "e4" to be enabled, actual not enabled`
	if failed.Error != want {
		t.Fatalf("error = %q, want %q", failed.Error, want)
	}

	rec = postAssert(t, server, `{"assertion":"element_state","ref":"e4","state":"enabled","negate":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("negated assertion status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestAssertRouteRejectsAnInvalidRequest(t *testing.T) {
	server := New("", newGettersController())

	rec := postAssert(t, server, `{"assertion":"http_status","status":42}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "http status assertion requires status between 100 and 599") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}
