package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/navpolicy"
)

// A batch find_act step searches with the tab-aware finder, not the raw in-page
// primitive. The raw primitive is not the same search: it skips the shadow-root
// arming and the WebMCP install that decide which elements are candidates at
// all, and it skips enforceFinalURL — the re-check that refuses a page the
// navigation policy would not allow. All three live in the same finder, and
// this pins the wiring through the one that is observable without a browser
// extension: a page the policy rejects must be refused rather than searched
// and clicked.
func TestBatchFindActSearchesThroughTheGuardedFinder(t *testing.T) {
	var clicked atomic.Bool
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/clicked" {
			clicked.Store(true)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The beacon outlives the policy's about:blank reset, so "did the step
		// actuate" is answerable after the tab has been taken away.
		fmt.Fprint(w, `<!doctype html><html><head><meta charset="utf-8"><title>policy</title></head>`+
			`<body><button id="only" onclick="fetch('/clicked')">Check out</button></body></html>`)
	}))
	defer site.Close()

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)

	// Armed after the page is loaded: the fixture's own origin is now off the
	// allowlist, which is the state a page that redirected itself leaves behind.
	m.SetNavigationPolicy(navpolicy.Parse("example.test", ""))

	result, err := m.ExecuteBatch(tabCtx, []BatchStep{{
		Action: "find_act",
		Find:   &FindAct{Query: "Check out", Role: "button", Action: "click"},
	}})
	if err != nil && !strings.Contains(err.Error(), "navigation policy") {
		t.Fatalf("batch: %v", err)
	}
	if len(result.Steps) != 1 {
		t.Fatalf("batch returned %d step results: %+v", len(result.Steps), result)
	}
	if result.Steps[0].OK {
		t.Fatal("a find_act step searched and acted on a page the navigation policy rejects")
	}
	if !strings.Contains(result.Steps[0].Error, "navigation policy") {
		t.Fatalf("step error = %q, want the navigation policy refusal", result.Steps[0].Error)
	}
	// Give an actuation that should not have happened time to arrive.
	time.Sleep(250 * time.Millisecond)
	if clicked.Load() {
		t.Fatal("the find_act step clicked an element on a page the navigation policy rejects")
	}
}
