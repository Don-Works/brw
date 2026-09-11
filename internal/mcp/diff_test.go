package mcp

import (
	"testing"

	"github.com/Don-Works/brw/internal/snapshot"
)

func snap(url, title string, elements ...snapshot.Element) snapshot.PageSnapshot {
	return snapshot.PageSnapshot{URL: url, Title: title, Elements: elements}
}

func el(ref, role, name string) snapshot.Element {
	return snapshot.Element{Ref: ref, Role: role, Name: name}
}

func TestDiffSnapshots(t *testing.T) {
	tests := []struct {
		name         string
		before       snapshot.PageSnapshot
		after        snapshot.PageSnapshot
		wantChanged  bool
		wantAdded    int
		wantRemoved  int
		wantUpdated  int
		wantURLMoved bool
	}{
		{
			name:        "identical pages report no change",
			before:      snap("https://x/", "X", el("e1", "button", "Save")),
			after:       snap("https://x/", "X", el("e1", "button", "Save")),
			wantChanged: false,
		},
		{
			name:        "a new element is added",
			before:      snap("https://x/", "X", el("e1", "button", "Save")),
			after:       snap("https://x/", "X", el("e1", "button", "Save"), el("e2", "alert", "Saved")),
			wantChanged: true,
			wantAdded:   1,
		},
		{
			name:        "a vanished element is removed",
			before:      snap("https://x/", "X", el("e1", "button", "Save"), el("e2", "alert", "Saved")),
			after:       snap("https://x/", "X", el("e1", "button", "Save")),
			wantChanged: true,
			wantRemoved: 1,
		},
		{
			name:        "a renamed element is updated, not replaced",
			before:      snap("https://x/", "X", el("e1", "button", "Save")),
			after:       snap("https://x/", "X", el("e1", "button", "Saving...")),
			wantChanged: true,
			wantUpdated: 1,
		},
		{
			name:         "navigation is reported as a url change",
			before:       snap("https://x/", "X", el("e1", "button", "Save")),
			after:        snap("https://x/done", "Done", el("e1", "button", "Save")),
			wantChanged:  true,
			wantURLMoved: true,
		},
		{
			name:        "title-only change still counts as changed",
			before:      snap("https://x/", "X", el("e1", "button", "Save")),
			after:       snap("https://x/", "X (2)", el("e1", "button", "Save")),
			wantChanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := diffSnapshots(baselineFrom(tt.before), tt.after, "")
			if got.Changed != tt.wantChanged {
				t.Errorf("changed = %v, want %v", got.Changed, tt.wantChanged)
			}
			if got.AddedCount != tt.wantAdded {
				t.Errorf("added = %d, want %d", got.AddedCount, tt.wantAdded)
			}
			if got.RemovedCount != tt.wantRemoved {
				t.Errorf("removed = %d, want %d", got.RemovedCount, tt.wantRemoved)
			}
			if got.UpdatedCount != tt.wantUpdated {
				t.Errorf("updated = %d, want %d", got.UpdatedCount, tt.wantUpdated)
			}
			if got.URLChanged != tt.wantURLMoved {
				t.Errorf("url_changed = %v, want %v", got.URLChanged, tt.wantURLMoved)
			}
			if !tt.wantChanged && got.Note == "" {
				t.Error("an unchanged diff should say so plainly")
			}
		})
	}
}

// A re-render that keeps element identity must not read as a wholesale replace;
// that is the failure mode that makes a naive diff useless on a real SPA.
func TestDiffMatchesOnIdentityNotPosition(t *testing.T) {
	before := snap("https://x/", "X", el("a", "listitem", "One"), el("b", "listitem", "Two"), el("c", "listitem", "Three"))
	after := snap("https://x/", "X", el("c", "listitem", "Three"), el("a", "listitem", "One"), el("b", "listitem", "Two"))
	got := diffSnapshots(baselineFrom(before), after, "")
	if got.Changed {
		t.Fatalf("reordering the same elements should not report a change: %+v", got)
	}
}

// The counts must stay exact even when the enumerated lists are capped, or a
// large diff would under-report what happened.
func TestDiffCapsListsButKeepsExactCounts(t *testing.T) {
	var after []snapshot.Element
	for i := 0; i < maxDiffEntries*3; i++ {
		after = append(after, el(string(rune('a'+i%26))+string(rune('0'+i/26)), "listitem", "row"))
	}
	got := diffSnapshots(baselineFrom(snap("https://x/", "X")), snap("https://x/", "X", after...), "")
	if got.AddedCount != len(after) {
		t.Fatalf("added_count = %d, want the exact %d", got.AddedCount, len(after))
	}
	if len(got.Added) != maxDiffEntries {
		t.Fatalf("enumerated %d entries, want them capped at %d", len(got.Added), maxDiffEntries)
	}
	if !got.Truncated {
		t.Error("a capped diff must say it was truncated")
	}
}

func TestDiffSummaryIsOnTheResult(t *testing.T) {
	unchanged := diffSnapshots(baselineFrom(snap("https://x/", "X")), snap("https://x/", "X"), "")
	if unchanged.Summary != "unchanged" {
		t.Errorf("summary = %q, want unchanged", unchanged.Summary)
	}
	changed := diffSnapshots(baselineFrom(snap("https://x/", "X")), snap("https://x/", "X", el("e1", "alert", "hi")), "")
	if changed.Summary != "+1" {
		t.Errorf("summary = %q, want +1", changed.Summary)
	}
}
