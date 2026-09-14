package browser

import (
	"strconv"
	"testing"
)

// A bounded wait on a WebMCP page tool runs one Evaluate per poll — close to six
// hundred at the ten-minute cap — and each one records a trace entry. The trace
// is a 500-entry ring, so one blocking wait would evict every real action a
// session performed and leave nothing but identical poll rows. A repeat of a
// collapsible action folds into the row it repeats instead.
func TestRecordTraceCollapsesRepeatedPollEntries(t *testing.T) {
	tests := []struct {
		name        string
		entries     []TraceEntry
		wantRows    int
		wantRepeat  int
		wantDuraMS  int64
		wantLastTS  string
		wantActions []string
	}{
		{
			name: "identical page_tool polls fold into one row",
			entries: []TraceEntry{
				{Action: TraceActionPageTool, Text: "result abc-1", OK: true, DurationMS: 3, Timestamp: "t1"},
				{Action: TraceActionPageTool, Text: "result abc-1", OK: true, DurationMS: 4, Timestamp: "t2"},
				{Action: TraceActionPageTool, Text: "result abc-1", OK: true, DurationMS: 5, Timestamp: "t3"},
			},
			wantRows:    1,
			wantRepeat:  2,
			wantDuraMS:  12,
			wantLastTS:  "t3",
			wantActions: []string{TraceActionPageTool},
		},
		{
			name: "a different invocation is a different row",
			entries: []TraceEntry{
				{Action: TraceActionPageTool, Text: "result abc-1", OK: true, Timestamp: "t1"},
				{Action: TraceActionPageTool, Text: "result abc-2", OK: true, Timestamp: "t2"},
			},
			wantRows:    2,
			wantRepeat:  0,
			wantLastTS:  "t2",
			wantActions: []string{TraceActionPageTool, TraceActionPageTool},
		},
		{
			name: "a poll that starts failing is a distinct event",
			entries: []TraceEntry{
				{Action: TraceActionPageTool, Text: "result abc-1", OK: true, Timestamp: "t1"},
				{Action: TraceActionPageTool, Text: "result abc-1", OK: false, Error: "tab closed", Timestamp: "t2"},
			},
			wantRows:    2,
			wantRepeat:  0,
			wantLastTS:  "t2",
			wantActions: []string{TraceActionPageTool, TraceActionPageTool},
		},
		{
			name: "repeated user actions are never folded away",
			entries: []TraceEntry{
				{Action: "click", Text: "Submit", OK: true, Timestamp: "t1"},
				{Action: "click", Text: "Submit", OK: true, Timestamp: "t2"},
				{Action: "click", Text: "Submit", OK: true, Timestamp: "t3"},
			},
			wantRows:    3,
			wantRepeat:  0,
			wantLastTS:  "t3",
			wantActions: []string{"click", "click", "click"},
		},
		{
			name: "a poll run after another action starts a new row",
			entries: []TraceEntry{
				{Action: TraceActionPageTool, Text: "result abc-1", OK: true, Timestamp: "t1"},
				{Action: "click", Text: "Submit", OK: true, Timestamp: "t2"},
				{Action: TraceActionPageTool, Text: "result abc-1", OK: true, Timestamp: "t3"},
			},
			wantRows:    3,
			wantRepeat:  0,
			wantLastTS:  "t3",
			wantActions: []string{TraceActionPageTool, "click", TraceActionPageTool},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manager{}
			for _, entry := range tt.entries {
				m.recordTrace("tab1", entry)
			}
			got := m.GetTrace()
			if got.Count != tt.wantRows {
				t.Fatalf("recorded %d rows, want %d: %+v", got.Count, tt.wantRows, got.Entries)
			}
			for i, want := range tt.wantActions {
				if got.Entries[i].Action != want {
					t.Fatalf("row %d is %q, want %q", i, got.Entries[i].Action, want)
				}
			}
			last := got.Entries[len(got.Entries)-1]
			if last.Repeat != tt.wantRepeat {
				t.Fatalf("last row repeat = %d, want %d", last.Repeat, tt.wantRepeat)
			}
			if tt.wantDuraMS != 0 && last.DurationMS != tt.wantDuraMS {
				t.Fatalf("last row duration = %dms, want the whole run of repeats (%dms)", last.DurationMS, tt.wantDuraMS)
			}
			if last.Timestamp != tt.wantLastTS {
				t.Fatalf("last row timestamp = %q, want %q", last.Timestamp, tt.wantLastTS)
			}
		})
	}
}

// The ring is what the collapse protects: a session's real actions have to
// survive a wait that polls more times than the ring can hold.
func TestCollapsedPollsDoNotEvictTheRing(t *testing.T) {
	m := &Manager{}
	m.recordTrace("tab1", TraceEntry{Action: "click", Text: "Submit", OK: true, Timestamp: "t0"})
	for i := 0; i < 600; i++ {
		m.recordTrace("tab1", TraceEntry{Action: TraceActionPageTool, Text: "result abc-1", OK: true, Timestamp: "t" + strconv.Itoa(i+1)})
	}
	got := m.GetTrace()
	if got.Count != 2 {
		t.Fatalf("600 polls recorded %d rows, want the click plus one collapsed poll row", got.Count)
	}
	if got.Entries[0].Action != "click" {
		t.Fatalf("the click was evicted by the polls: %+v", got.Entries)
	}
	if got.Entries[1].Repeat != 599 {
		t.Fatalf("collapsed row repeat = %d, want 599", got.Entries[1].Repeat)
	}
}
