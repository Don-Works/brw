package browser

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// TestGeneratedScriptVerbsTraceAsThemselves covers what brw_trace shows for the
// two verbs that run machine-written walker expressions through Evaluate.
//
// brw_get and brw_frame exist so an agent does not have to hand-write JavaScript
// for a simple read or a frame switch. Recorded as "evaluate" entries carrying
// the generated script, they were indistinguishable in the trace from a
// hand-written brw_evaluate — so a reviewer reading a session's trace could not
// tell a typed read from arbitrary page script, which is the one thing the trace
// is for.
func TestGeneratedScriptVerbsTraceAsThemselves(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tabCtx := openModifierFixture(t, m, ctx)

	tests := []struct {
		name       string
		ctx        func() context.Context
		expression string
		wantAction string
		wantText   string
	}{
		{
			name:       "get",
			ctx:        func() context.Context { return WithTraceLabel(tabCtx, TraceActionGet, "text #pad") },
			expression: snapshot.BuildGetExpression("text", "#pad", ""),
			wantAction: TraceActionGet,
			wantText:   "text #pad",
		},
		{
			name:       "frame",
			ctx:        func() context.Context { return WithTraceLabel(tabCtx, TraceActionFrame, "main") },
			expression: snapshot.BuildFrameSwitchExpression("main"),
			wantAction: TraceActionFrame,
			wantText:   "main",
		},
		{
			name:       "an unlabelled evaluation is still an evaluate",
			ctx:        func() context.Context { return tabCtx },
			expression: "1 + 1",
			wantAction: TraceActionEvaluate,
			wantText:   "1 + 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m.ClearTrace()
			if _, err := m.Evaluate(tt.ctx(), tt.expression); err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			entries := m.GetTrace().Entries
			if len(entries) == 0 {
				t.Fatal("the evaluation recorded no trace entry at all")
			}
			last := entries[len(entries)-1]
			if last.Action != tt.wantAction {
				t.Fatalf("trace action = %q, want %q", last.Action, tt.wantAction)
			}
			if last.Text != tt.wantText {
				t.Fatalf("trace text = %q, want %q", last.Text, tt.wantText)
			}
			if strings.Contains(last.Text, "__abRootList") {
				t.Fatalf("trace text carries the generated walker script: %q", last.Text)
			}
			if !IsObservationAction(last.Action) {
				t.Fatalf("action %q is not classified as an observation, so replay would list it back as unknown", last.Action)
			}
		})
	}
}
