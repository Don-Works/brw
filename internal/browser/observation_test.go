package browser

import (
	"context"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

func TestNewSemanticState_ExtractsFocus(t *testing.T) {
	snap := snapshot.PageSnapshot{
		URL:   "https://example.com",
		Title: "Test",
		Metadata: map[string]any{
			"focused_ref": "e5",
		},
		Elements: []snapshot.Element{
			{Ref: "e1", Role: "button", Name: "OK", Visible: true},
		},
	}
	state := NewSemanticState(snap)
	if state.Focus != "e5" {
		t.Fatalf("expected focus e5, got %q", state.Focus)
	}
	if state.URL != "https://example.com" {
		t.Fatalf("expected URL, got %q", state.URL)
	}
}

func TestNewSemanticState_NilMetadata(t *testing.T) {
	snap := snapshot.PageSnapshot{
		URL:   "https://example.com",
		Title: "Test",
	}
	state := NewSemanticState(snap)
	if state.Focus != "" {
		t.Fatalf("expected empty focus, got %q", state.Focus)
	}
}

func TestNewSemanticState_SignatureChanges(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }
	snap1 := snapshot.PageSnapshot{
		Elements: []snapshot.Element{
			{Ref: "e1", Role: "button", Name: "OK", Visible: true, Selected: boolPtr(true)},
		},
	}
	snap2 := snapshot.PageSnapshot{
		Elements: []snapshot.Element{
			{Ref: "e1", Role: "button", Name: "OK", Visible: true, Selected: boolPtr(false)},
		},
	}
	s1 := NewSemanticState(snap1)
	s2 := NewSemanticState(snap2)
	if s1.Signature == s2.Signature {
		t.Fatal("expected different signatures for different Selected values")
	}
}

func TestApplyStateDiff_NilBefore(t *testing.T) {
	result := &ActionResult{OK: true}
	ApplyStateDiff(result, nil, SemanticState{})
	if result.ChangedState != nil {
		t.Fatal("expected no ChangedState when before is nil")
	}
}

func TestApplyStateDiff_SameState(t *testing.T) {
	state := SemanticState{URL: "https://example.com", Title: "Test", Focus: "e1", Signature: "sig"}
	result := &ActionResult{OK: true}
	ApplyStateDiff(result, &state, state)
	if result.ChangedState == nil || *result.ChangedState {
		t.Fatal("expected ChangedState=false for same state")
	}
	if result.Warning == "" {
		t.Fatal("expected warning for no change")
	}
}

func TestApplyStateDiff_DifferentURL(t *testing.T) {
	before := SemanticState{URL: "https://a.com", Title: "Test", Signature: "sig"}
	after := SemanticState{URL: "https://b.com", Title: "Test", Signature: "sig"}
	result := &ActionResult{OK: true}
	ApplyStateDiff(result, &before, after)
	if result.ChangedState == nil || !*result.ChangedState {
		t.Fatal("expected ChangedState=true for different URL")
	}
}

func TestAdvanceObservationStateVersionsOnlySemanticChanges(t *testing.T) {
	first := SemanticState{URL: "https://example.test", Signature: "one"}
	state, version, changed := AdvanceObservationState(nil, 0, first)
	if !changed || version != 1 || state == nil || !state.Equal(first) {
		t.Fatalf("first observation: state=%+v version=%d changed=%v", state, version, changed)
	}
	state, version, changed = AdvanceObservationState(state, version, first)
	if changed || version != 1 {
		t.Fatalf("unchanged observation: version=%d changed=%v", version, changed)
	}
	second := first
	second.Signature = "two"
	state, version, changed = AdvanceObservationState(state, version, second)
	if !changed || version != 2 || !state.Equal(second) {
		t.Fatalf("changed observation: state=%+v version=%d changed=%v", state, version, changed)
	}
}

func TestManagerObserveReportsOnlyChangesSincePreviousObserve(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	if _, err := m.Evaluate(tabCtx, `document.body.innerHTML='<button id="status">One</button>'; true`); err != nil {
		t.Fatal(err)
	}

	first, err := m.Observe(tabCtx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || len(first.Changed) == 0 {
		t.Fatalf("first observe = %+v, want initial frontier at version 1", first)
	}
	second, err := m.Observe(tabCtx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != first.Version || len(second.Changed) != 0 {
		t.Fatalf("unchanged observe = %+v, want same version and no changes", second)
	}
	if _, err := m.Evaluate(tabCtx, `document.querySelector('#status').textContent='Two'; true`); err != nil {
		t.Fatal(err)
	}
	third, err := m.Observe(tabCtx)
	if err != nil {
		t.Fatal(err)
	}
	if third.Version != 2 || len(third.Changed) == 0 {
		t.Fatalf("changed observe = %+v, want version 2 and changed frontier", third)
	}
}
