package browser

import (
	"testing"

	"github.com/revitt/agent-browser/internal/snapshot"
)

func TestNewSemanticState_ExtractsFocus(t *testing.T) {
	snap := snapshot.PageSnapshot{
		URL:   "https://example.com",
		Title: "Test",
		Metadata: map[string]interface{}{
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
