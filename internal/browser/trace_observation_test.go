package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/navpolicy"
)

func traceActions(result TraceResult) []string {
	actions := make([]string, 0, len(result.Entries))
	for _, entry := range result.Entries {
		actions = append(actions, entry.Action)
	}
	return actions
}

func findTrace(t *testing.T, result TraceResult, action string) TraceEntry {
	t.Helper()
	for _, entry := range result.Entries {
		if entry.Action == action {
			return entry
		}
	}
	t.Fatalf("no %q entry in trace; got %v", action, traceActions(result))
	return TraceEntry{}
}

// A session that opens a page and reads it performs no input action at all, so
// before observations were traced the whole visit was invisible to anyone
// watching the daemon — the trace reported zero entries for it.
func TestTraceRecordsNavigationAndReads(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>Fixture</title></head>
<body><h1>Headline</h1><p>Body text that readability can extract from the page.</p></body></html>`))
	}))
	defer site.Close()

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	if _, err := m.Read(tabCtx); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseTab(ctx, opened.Tab.ID); err != nil {
		t.Fatal(err)
	}

	trace := m.GetTrace()
	for _, action := range []string{TraceActionOpen, TraceActionRead, TraceActionCloseTab} {
		entry := findTrace(t, trace, action)
		if !entry.OK {
			t.Errorf("%s recorded as failed: %s", action, entry.Error)
		}
		if entry.Timestamp == "" {
			t.Errorf("%s has no timestamp", action)
		}
	}
	if got := findTrace(t, trace, TraceActionOpen).Text; !strings.HasPrefix(got, site.URL) {
		t.Errorf("open recorded url %q, want the opened address %q", got, site.URL)
	}
	if got := findTrace(t, trace, TraceActionRead).Text; !strings.HasPrefix(got, site.URL) {
		t.Errorf("read recorded url %q, want the page it read %q", got, site.URL)
	}
}

// An entry with no tab id is unscoped: scopedTrace and the session stream both
// hand a tab-less entry to EVERY caller of the shared daemon. An observation
// carries a URL, so one recorded without a tab would leak another session's
// browsing.
func TestObservationTraceAlwaysCarriesTabID(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><p>scoped</p></body></html>`))
	}))
	defer site.Close()

	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	opened, err := m.Open(ctx, site.URL)
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	if _, err := m.Read(tabCtx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Evaluate(tabCtx, "document.title"); err != nil {
		t.Fatal(err)
	}
	if err := m.FocusTab(ctx, opened.Tab.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseTab(ctx, opened.Tab.ID); err != nil {
		t.Fatal(err)
	}

	trace := m.GetTrace()
	if len(trace.Entries) == 0 {
		t.Fatal("no trace entries recorded")
	}
	for _, entry := range trace.Entries {
		if entry.TabID == "" {
			t.Errorf("action %q recorded with no tab id (text %q): it would be visible to every session on this daemon",
				entry.Action, entry.Text)
		}
	}
}

// A refused open is the entry an operator most wants: it names where a redirect
// actually went, which is the whole reason the open was refused.
func TestTraceRecordsRefusedOpenWithFinalURL(t *testing.T) {
	offlimits := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><p>off limits</p></body></html>`))
	}))
	defer offlimits.Close()
	// Same listener, different host spelling: the allowlist matches on host, so
	// "localhost" is off the allowlist while "127.0.0.1" is on it.
	elsewhere := strings.Replace(offlimits.URL, "127.0.0.1", "localhost", 1)

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere, http.StatusFound)
	}))
	defer entry.Close()

	m := newHeadlessManager(t)
	m.SetNavigationPolicy(&navpolicy.Policy{Allowed: []string{"127.0.0.1"}})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := m.Open(ctx, entry.URL); err == nil {
		t.Fatal("open that redirects off the allowlist should fail")
	}

	recorded := findTrace(t, m.GetTrace(), TraceActionOpen)
	if recorded.OK {
		t.Error("refused open recorded as OK")
	}
	if recorded.Error == "" {
		t.Error("refused open recorded no reason")
	}
	if recorded.TabID == "" {
		t.Error("refused open recorded with no tab id")
	}
	if !strings.Contains(recorded.Text, "localhost") {
		t.Errorf("refused open recorded url %q, want the destination the redirect reached (%q)", recorded.Text, elsewhere)
	}
}

func TestBoundedTraceTextTruncatesOnRuneBoundary(t *testing.T) {
	short := "https://example.test/page"
	if got := boundedTraceText(short); got != short {
		t.Errorf("boundedTraceText(%q) = %q, want it unchanged", short, got)
	}

	long := strings.Repeat("é", maxTraceTextBytes)
	got := boundedTraceText(long)
	if len(got) > maxTraceTextBytes+len("…") {
		t.Errorf("bounded text is %d bytes, want at most %d", len(got), maxTraceTextBytes+len("…"))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("truncated text is not marked as truncated")
	}
	if strings.ContainsRune(got, '�') {
		t.Error("truncation split a rune")
	}
}

func TestObservationsAreGroupedInReplaySkipReasons(t *testing.T) {
	trace := TraceResult{Entries: []TraceEntry{
		{Action: TraceActionOpen, Text: "https://example.test", TabID: "1", OK: true},
		{Action: TraceActionRead, Text: "https://example.test", TabID: "1", OK: true},
		{Action: "click", Ref: "e1", Name: "Submit", Role: "button", NameIsVisibleText: true, TabID: "1", OK: true},
	}}

	result := TraceToBatch(trace, ReplayOptions{})
	const reason = "observation, not an action to replay"
	if result.Reasons[reason] != 2 {
		t.Errorf("skipped_reasons[%q] = %d, want 2; got %v", reason, result.Reasons[reason], result.Reasons)
	}
	for key := range result.Reasons {
		if strings.HasPrefix(key, "not a replayable action") {
			t.Errorf("observation listed as an unknown action: %q", key)
		}
	}
	if result.Actions != 1 {
		t.Errorf("exported %d actions, want the single click", result.Actions)
	}
}

// Every trace label brw puts on a generated script is an evaluate under another
// name, so replay has to group all of them with the observations. page_tool was
// the one that was not: relabelling the page-tool polls moved them out of the
// group and brw_trace format="batch" started reporting them as "not a
// replayable action: page_tool", which reads as a recording defect an agent
// could act on rather than as something there was never anything to replay.
func TestGeneratedScriptVerbsReplayAsObservations(t *testing.T) {
	for _, action := range []string{TraceActionEvaluate, TraceActionGet, TraceActionFrame, TraceActionPageTool} {
		t.Run(action, func(t *testing.T) {
			if !IsObservationAction(action) {
				t.Fatalf("IsObservationAction(%q) = false, want every generated-script label grouped with the observations", action)
			}
			trace := TraceResult{Entries: []TraceEntry{
				{Action: action, Text: "result 0a1b2c3d-1", TabID: "1", OK: true},
				{Action: "click", Ref: "e1", Name: "Submit", Role: "button", NameIsVisibleText: true, TabID: "1", OK: true},
			}}
			result := TraceToBatch(trace, ReplayOptions{})
			const reason = "observation, not an action to replay"
			if result.Reasons[reason] != 1 {
				t.Fatalf("skipped_reasons = %v, want one %q", result.Reasons, reason)
			}
			for key := range result.Reasons {
				if strings.HasPrefix(key, "not a replayable action") {
					t.Fatalf("%q listed as an unknown action: %q", action, key)
				}
			}
		})
	}
}
