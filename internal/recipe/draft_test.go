package recipe

import (
	"encoding/json"
	"strings"
	"testing"
)

func sixStepTrace() []TraceAction {
	return []TraceAction{
		{Action: "navigate_to", URL: "https://chat.example.test/rooms", OK: true},
		{Action: "click", Ref: "e4", Role: "button", Name: "Search", NameIsVisibleText: true, OK: true},
		{Action: "fill", Ref: "e7", Role: "textbox", Name: "Search conversations", Text: "quarterly review", OK: true},
		{Action: "press", Ref: "e7", Text: "Enter", OK: true},
		{Action: "click", Ref: "e9", Role: "link", Name: "Quarterly review", NameIsVisibleText: true, OK: true},
		{Action: "fill", Ref: "e12", Role: "textbox", Name: "Message", Text: "on my way", OK: true},
	}
}

func TestDraftFromTraceProducesAValidatableSkeleton(t *testing.T) {
	drafts, err := DraftFromTrace(sixStepTrace(), DraftOptions{ID: "example.chat.search", Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want 1", len(drafts))
	}
	d := drafts[0]
	if d.SchemaVersion != 1 || d.ID != "example.chat.search" || d.Version != "1.0.0" {
		t.Fatalf("header wrong: %+v", d)
	}
	if len(d.Steps) != 6 {
		t.Fatalf("got %d steps, want 6", len(d.Steps))
	}
	if got := d.Origins; len(got) != 1 || got[0] != "https://chat.example.test" {
		t.Fatalf("origins = %v, want the origin inferred from the trace", got)
	}

	// Every actuation must carry an unresolved effect and postcondition. A
	// generator that guessed these would defeat the field's purpose.
	for _, s := range d.Steps {
		if !writeActions[s.Action] {
			continue
		}
		if !strings.Contains(s.Effect, TodoMarker) {
			t.Fatalf("step %s effect %q was guessed; it must be a TODO", s.ID, s.Effect)
		}
		if s.Postcondition == nil || !strings.Contains(s.Postcondition.Kind, TodoMarker) {
			t.Fatalf("step %s postcondition was guessed or missing", s.ID)
		}
	}

	// Refs are page-state-scoped and must never reach a recipe.
	body, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if refPattern.Match(body) {
		t.Fatalf("draft carried an observation ref:\n%s", body)
	}
}

func TestDraftTargetsUseSemanticIdentityNotRefs(t *testing.T) {
	drafts, _ := DraftFromTrace(sixStepTrace(), DraftOptions{ID: "example.chat.search"})
	steps := drafts[0].Steps

	// Visible-text names can be asserted exactly; the rest must be contains,
	// because an exact match on a name the element does not render will fail.
	if got := steps[1].Target; got == nil || got.Role != "button" || got.Name != "Search" {
		t.Fatalf("visible-text target = %+v, want exact Name", got)
	}
	if got := steps[2].Target; got == nil || got.NameContains != "Search conversations" || got.Name != "" {
		t.Fatalf("non-visible-text target = %+v, want NameContains", got)
	}
}

func TestDraftNeverInlinesARedactedValue(t *testing.T) {
	drafts, err := DraftFromTrace([]TraceAction{
		{Action: "fill", Ref: "e2", Role: "textbox", Name: "Password", Redacted: true, OK: true},
	}, DraftOptions{ID: "example.site.login"})
	if err != nil {
		t.Fatal(err)
	}
	got := drafts[0].Steps[0].Value
	if !strings.HasPrefix(got, "${input:") || !strings.Contains(got, TodoMarker) {
		t.Fatalf("redacted fill value = %q; must point at a declared input, never a literal", got)
	}
}

// Composition and delivery must not become one recipe: that is what stops a
// stored recipe turning prior authorisation into standing permission.
func TestDraftSplitsPrepareFromSend(t *testing.T) {
	trace := append(sixStepTrace(), TraceAction{
		Action: "click", Ref: "e13", Role: "button", Name: "Send", NameIsVisibleText: true, OK: true,
	})
	drafts, err := DraftFromTrace(trace, DraftOptions{ID: "example.chat.message", Name: "Message"})
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 2 {
		t.Fatalf("got %d drafts, want prepare + send", len(drafts))
	}
	if drafts[0].ID != "example.chat.message-prepare" || drafts[1].ID != "example.chat.message-send" {
		t.Fatalf("ids = %q, %q", drafts[0].ID, drafts[1].ID)
	}
	if n := len(drafts[1].Steps); n != 1 {
		t.Fatalf("send draft has %d steps, want only the send itself", n)
	}
	if len(drafts[0].Steps) != 6 {
		t.Fatalf("prepare draft has %d steps, want everything before the send", len(drafts[0].Steps))
	}
}

func TestDraftRejectsUnusableInput(t *testing.T) {
	tests := []struct {
		name    string
		actions []TraceAction
		opts    DraftOptions
	}{
		{"no successful actions", []TraceAction{{Action: "click", OK: false}}, DraftOptions{ID: "a.b.c"}},
		{"empty trace", nil, DraftOptions{ID: "a.b.c"}},
		{"missing id", sixStepTrace(), DraftOptions{}},
		{"bad id", sixStepTrace(), DraftOptions{ID: "NotDotted"}},
		{"bad version", sixStepTrace(), DraftOptions{ID: "a.b.c", Version: "v1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DraftFromTrace(tt.actions, tt.opts); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
