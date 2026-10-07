package httpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

func TestWaitForOutcomeCarriesTheUpstreamOutcome(t *testing.T) {
	tests := []struct {
		name           string
		body           map[string]any
		wantResolvedBy string
		wantWakeups    int
	}{
		{
			name: "an upstream that reports its outcome",
			body: map[string]any{
				"ok": true, "condition": "dialog", "resolved_by": "poll",
				"waited_ms": 120, "wakeups": 4,
			},
			wantResolvedBy: browser.WaitResolvedByPoll,
			wantWakeups:    4,
		},
		{
			name: "an upstream too old to report one",
			body: map[string]any{"ok": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotCondition string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Condition string `json:"condition"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				gotCondition = req.Condition
				w.Header().Set("content-type", "application/json")
				_ = json.NewEncoder(w).Encode(tt.body)
			}))
			defer srv.Close()

			c, err := New(srv.URL, 5*time.Second)
			if err != nil {
				t.Fatalf("new controller: %v", err)
			}

			outcome, err := c.WaitForOutcome(context.Background(), "dialog", time.Second)
			if err != nil {
				t.Fatalf("wait: %v", err)
			}
			if gotCondition != "dialog" {
				t.Fatalf("upstream was asked for condition %q, want %q", gotCondition, "dialog")
			}
			if !outcome.OK {
				t.Fatalf("outcome = %+v, want ok", outcome)
			}

			if outcome.Condition != "dialog" {
				t.Fatalf("condition = %q, want %q", outcome.Condition, "dialog")
			}
			if outcome.ResolvedBy != tt.wantResolvedBy {
				t.Fatalf("resolved_by = %q, want %q", outcome.ResolvedBy, tt.wantResolvedBy)
			}
			if outcome.Wakeups != tt.wantWakeups {
				t.Fatalf("wakeups = %d, want %d", outcome.Wakeups, tt.wantWakeups)
			}
		})
	}
}

func TestControllerImplementsWaitObserver(t *testing.T) {
	var _ browser.WaitObserver = (*Controller)(nil)
}

func TestWaitForOutcomeReportsAnUpstreamFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": `timed out waiting for "dialog"`})
	}))
	defer srv.Close()

	c, err := New(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	outcome, err := c.WaitForOutcome(context.Background(), "dialog", time.Second)
	if err == nil {
		t.Fatalf("want the upstream timeout, got outcome %+v", outcome)
	}
	if outcome.OK {
		t.Fatalf("outcome = %+v, want ok=false", outcome)
	}
	if waitErr := c.WaitFor(context.Background(), "dialog", time.Second); waitErr == nil {
		t.Fatal("WaitFor must report the same failure as WaitForOutcome")
	}
}
