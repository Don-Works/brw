package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

// observingController is a backend that can say how a wait resolved, which both
// first-party transports can.
type observingController struct {
	*fakeController
	outcome browser.WaitOutcome
}

func (o *observingController) WaitForOutcome(_ context.Context, condition string, _ time.Duration) (browser.WaitOutcome, error) {
	outcome := o.outcome
	outcome.Condition = condition
	outcome.OK = true
	return outcome, nil
}

// The route body is what `brw wait` prints and what a proxying brw reads back,
// so the outcome has to survive the HTTP hop. Writing a bare {ok:true} here is
// what made brw_wait_for's advertised return shape false on the remote
// transport and on the CLI.
func TestWaitRouteCarriesTheOutcome(t *testing.T) {
	ctrl := &observingController{
		fakeController: &fakeController{},
		outcome:        browser.WaitOutcome{ResolvedBy: browser.WaitResolvedByEvent, WaitedMS: 37, Wakeups: 2},
	}
	server := New("", ctrl)

	req := httptest.NewRequest(http.MethodPost, "/api/page/wait_for", strings.NewReader(`{"condition":"dialog","timeout_ms":1000}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got browser.WaitOutcome
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Condition != "dialog" || got.ResolvedBy != browser.WaitResolvedByEvent || got.Wakeups != 2 || got.WaitedMS != 37 {
		t.Fatalf("wait body = %+v, want the backend's outcome", got)
	}
}

// A backend that cannot report an outcome still answers the route: the wait is
// what the caller asked for, and only the reporting is missing.
func TestWaitRouteStillAnswersABackendThatCannotReport(t *testing.T) {
	server := New("", &fakeController{})

	req := httptest.NewRequest(http.MethodPost, "/api/page/wait_for", strings.NewReader(`{"condition":"ready"}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["ok"] != true {
		t.Fatalf("wait body = %+v, want ok", got)
	}
	if _, reported := got["resolved_by"]; reported {
		t.Fatalf("wait body = %+v, want no resolved_by from a backend that cannot report one", got)
	}
}
