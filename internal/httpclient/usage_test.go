package httpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/usagelog"
)

func TestReportUsageUsesExactMetadataAndSharedCorrelation(t *testing.T) {
	var report usagelog.Event
	var requestID, session string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/usage/report" || r.Method != http.MethodPost {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		requestID = r.Header.Get(usagelog.HeaderRequestID)
		session = r.Header.Get(usagelog.HeaderSessionID)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&report); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	controller, err := New(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := browser.WithTabID(usagelog.WithRequestID(context.Background(), "logical-call-1"), "private-tab")
	event := usagelog.Event{Layer: "mcp", Operation: "brw_fill", Outcome: "ok", Scope: "tool", Representation: "mcp_arguments_result", InputBytes: usagelog.Count(0), OutputBytes: usagelog.Count(42)}
	if err := controller.ReportUsage(ctx, event); err != nil {
		t.Fatal(err)
	}
	if requestID != "logical-call-1" || session == "" || report.OutputBytes == nil || *report.OutputBytes != 42 {
		t.Fatalf("report=%+v request=%s session=%s", report, requestID, session)
	}
}

func TestReportUsageHasBoundedDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(250 * time.Millisecond):
		}
	}))
	defer server.Close()
	controller, err := New(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := controller.ReportUsage(context.Background(), usagelog.Event{}); err == nil {
		t.Fatal("expected reporting deadline")
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("reporting exceeded bounded budget")
	}
}
