package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/cdp"
	"github.com/Don-Works/brw/internal/readability"
)

// TestManagerPlanClickTextStep covers the plan runner's transport parity: the
// extension bridge has accepted a click_text plan step since private recipes
// landed, while direct CDP answered `unknown action "click_text"`, so the same
// flow succeeded or failed depending on which backend the daemon was started
// with.
func TestManagerPlanClickTextStep(t *testing.T) {
	if _, err := cdp.FindChrome(""); err != nil {
		t.Skipf("Chrome/Chromium not available: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>Start</title><main><a href="/done">Continue</a></main>`))
	})
	mux.HandleFunc("/done", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>Done</title><main>done-document-marker</main>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cases := []struct {
		name       string
		steps      []PlanStep
		wantOK     bool
		wantError  string
		wantInRead string
	}{
		{
			name: "click_text drives the plan to the labelled link",
			steps: []PlanStep{
				{Action: "click_text", Text: "Continue"},
				{Action: "wait", Condition: "text:done-document-marker", TimeoutMS: 8000},
				{Action: "read"},
			},
			wantOK:     true,
			wantInRead: "done-document-marker",
		},
		{
			name:      "click_text without text is refused by name",
			steps:     []PlanStep{{Action: "click_text"}},
			wantError: "click_text requires text",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			m, err := New(ctx, Config{
				Timeout:    20 * time.Second,
				ChromeArgs: []string{"--headless=new", "--disable-gpu", "--no-sandbox"},
			})
			if err != nil {
				t.Skipf("could not launch headless Chrome: %v", err)
			}
			defer m.Close()
			if _, err := m.Open(ctx, srv.URL+"/start"); err != nil {
				t.Fatalf("open start fixture: %v", err)
			}

			result, _ := m.ExecutePlan(ctx, tc.steps)
			if len(result.Steps) == 0 {
				t.Fatalf("plan produced no step results: %+v", result)
			}
			first := result.Steps[0]
			if strings.Contains(first.Error, "unknown action") {
				t.Fatalf("direct CDP does not implement the plan's click_text step: %s", first.Error)
			}
			if result.OK != tc.wantOK {
				t.Fatalf("plan ok = %t (error %q), want %t", result.OK, result.Error, tc.wantOK)
			}
			if tc.wantError != "" && first.Error != tc.wantError {
				t.Fatalf("step error = %q, want %q", first.Error, tc.wantError)
			}
			if tc.wantInRead == "" {
				return
			}
			read, ok := result.Steps[len(result.Steps)-1].Result.(readability.PageRead)
			if !ok {
				t.Fatalf("read result type = %T, want readability.PageRead", result.Steps[len(result.Steps)-1].Result)
			}
			if !strings.Contains(read.Main, tc.wantInRead) {
				t.Fatalf("plan read the wrong document: url=%q main=%q", read.URL, read.Main)
			}
		})
	}
}
