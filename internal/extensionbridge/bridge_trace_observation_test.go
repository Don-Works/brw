package extensionbridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

func bridgeTraceFor(t *testing.T, b *Bridge, action string) browser.TraceEntry {
	t.Helper()
	trace := b.GetTrace()
	actions := make([]string, 0, len(trace.Entries))
	for _, entry := range trace.Entries {
		if entry.Action == action {
			return entry
		}
		actions = append(actions, entry.Action)
	}
	t.Fatalf("no %q entry in bridge trace; got %v", action, actions)
	return browser.TraceEntry{}
}

// The extension bridge drives the user's own signed-in Chrome and keeps its own
// trace, which is the only activity record a bridge daemon has — it implements
// no push stream, so a watcher polls this. Recording open on the direct-CDP
// manager alone would have left every bridge deployment exactly as blind as
// before.
func TestBridgeOpenIsTraced(t *testing.T) {
	b := New("", 5*time.Second, "")
	fe := &groupAwareExtension{
		focusedWindow: 1,
		nextTabID:     700,
		groups:        map[int]*gaGroup{},
		tabs: []*gaTab{
			{id: 1, windowID: 1, groupID: -1, active: true, url: "https://x.test", title: "x"},
		},
	}
	cleanup := connectGroupAwareExtension(t, b, fe)
	defer cleanup()

	const target = "http://127.0.0.1:13333/reading"
	res, err := b.Open(context.Background(), target)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	entry := bridgeTraceFor(t, b, browser.TraceActionOpen)
	if !entry.OK {
		t.Errorf("open recorded as failed: %s", entry.Error)
	}
	if entry.TabID != res.Tab.ID {
		t.Errorf("open recorded tab %q, want the opened tab %q", entry.TabID, res.Tab.ID)
	}
	if !strings.Contains(entry.Text, "13333") {
		t.Errorf("open recorded url %q, want the opened address %q", entry.Text, target)
	}
	if entry.Timestamp == "" {
		t.Error("open recorded no timestamp")
	}
}

// Default-group opens go through OpenInGroup, which is the path a real bridge
// daemon takes for every brw_open — it corrals agent tabs into one group.
func TestBridgeDefaultGroupOpenIsTraced(t *testing.T) {
	b := New("", 5*time.Second, "")
	b.SetDefaultGroup("brw")
	fe := &groupAwareExtension{
		focusedWindow: 1,
		nextTabID:     800,
		groups:        map[int]*gaGroup{},
		tabs: []*gaTab{
			{id: 1, windowID: 1, groupID: -1, active: true, url: "https://x.test", title: "x"},
		},
	}
	cleanup := connectGroupAwareExtension(t, b, fe)
	defer cleanup()

	res, err := b.Open(context.Background(), "http://127.0.0.1:13333/grouped")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if entry := bridgeTraceFor(t, b, browser.TraceActionOpen); entry.TabID != res.Tab.ID {
		t.Errorf("grouped open recorded tab %q, want %q", entry.TabID, res.Tab.ID)
	}
}

// Same rule as the direct-CDP manager: a tab-less entry is unscoped and reaches
// every caller of the shared daemon, so an observation that cannot name its tab
// is dropped rather than broadcast with its URL.
func TestBridgeObservationWithoutTabIDIsDropped(t *testing.T) {
	b := &Bridge{}
	b.recordObservation("", browser.TraceActionRead, "https://private.test/inbox", time.Now(), nil)
	b.recordObservation("   ", browser.TraceActionOpen, "https://private.test/secret", time.Now(), nil)

	if trace := b.GetTrace(); len(trace.Entries) != 0 {
		t.Fatalf("recorded %d unscoped entries, want none: %+v", len(trace.Entries), trace.Entries)
	}

	b.recordObservation("42", browser.TraceActionRead, "https://example.test/page", time.Now(), nil)
	trace := b.GetTrace()
	if len(trace.Entries) != 1 || trace.Entries[0].TabID != "42" {
		t.Fatalf("scoped observation not recorded: %+v", trace.Entries)
	}
}

// The trace is a bounded ring buffer served over the HTTP control plane, so an
// evaluate expression is recorded by its prefix rather than in full.
func TestBridgeObservationTextIsBounded(t *testing.T) {
	b := &Bridge{}
	long := strings.Repeat("a", 4096)
	b.recordObservation("7", browser.TraceActionEvaluate, long, time.Now(), nil)

	entry := b.GetTrace().Entries[0]
	if len(entry.Text) >= len(long) {
		t.Fatalf("recorded %d bytes of expression, want a bounded prefix", len(entry.Text))
	}
	if !strings.HasSuffix(entry.Text, "…") {
		t.Error("bounded text is not marked as truncated")
	}
}
