package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
)

type tracingController struct {
	fakeController
	trace browser.TraceResult
}

func (c *tracingController) GetTrace() browser.TraceResult { return c.trace }

func TestTraceIsScopedToTheSessionThatProducedIt(t *testing.T) {
	ctrl := &tracingController{trace: browser.TraceResult{Entries: []browser.TraceEntry{
		{Action: "navigate_to", Text: "https://example.com/mine", TabID: "tab-mine", OK: true},
		{Action: "navigate_to", Text: "https://private.example/theirs/secret-id", TabID: "tab-theirs", OK: true},
		{Action: "press", Value: "Enter", OK: true},
	}}}
	ctrl.trace.Count = len(ctrl.trace.Entries)

	server := New("", ctrl)
	if err := server.leases.bind("owner-mine", "tab-mine", true); err != nil {
		t.Fatalf("bind own tab: %v", err)
	}
	if err := server.leases.bind("owner-theirs", "tab-theirs", true); err != nil {
		t.Fatalf("bind other tab: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/page/trace", nil)
	req = req.WithContext(context.WithValue(req.Context(), leaseContextKey{}, "owner-mine"))
	server.trace(rec, req)

	body := rec.Body.String()
	var got browser.TraceResult
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(body, "secret-id") {
		t.Fatalf("the trace leaked another session's URL:\n%s", body)
	}
	if !strings.Contains(body, "example.com/mine") {
		t.Fatalf("the caller's own action was filtered out:\n%s", body)
	}

	if len(got.Entries) != 2 {
		t.Fatalf("entries = %+v, want the caller's action plus the tab-less one", got.Entries)
	}
	if got.Withheld != 1 {
		t.Fatalf("withheld = %d, want 1 so a filtered trace is not mistaken for an empty one", got.Withheld)
	}
}

func TestTraceWithoutAnOwnerReturnsOnlyTablessEntries(t *testing.T) {
	ctrl := &tracingController{trace: browser.TraceResult{Entries: []browser.TraceEntry{
		{Action: "navigate_to", Text: "https://private.example/theirs", TabID: "tab-theirs", OK: true},
		{Action: "press", Value: "Enter", OK: true},
	}}}
	server := New("", ctrl)
	if err := server.leases.bind("owner-theirs", "tab-theirs", true); err != nil {
		t.Fatalf("bind: %v", err)
	}

	rec := httptest.NewRecorder()
	server.trace(rec, httptest.NewRequest(http.MethodGet, "/api/page/trace", nil))

	if strings.Contains(rec.Body.String(), "private.example") {
		t.Fatalf("an unidentified caller received a leased tab's action:\n%s", rec.Body.String())
	}
}
