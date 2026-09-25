package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeUpstream struct {
	*fakeController
	version string
	err     error
	calls   int
}

func (f *fakeUpstream) UpstreamVersion(context.Context) (string, error) {
	f.calls++
	return f.version, f.err
}

func TestVersionSkewNote(t *testing.T) {
	saved := Version
	t.Cleanup(func() { Version = saved })
	Version = "0.14.5"

	tests := []struct {
		name     string
		daemon   string
		err      error
		wantNote bool
	}{
		{name: "same build", daemon: "0.14.5"},
		{name: "newer daemon", daemon: "0.15.1", wantNote: true},
		{name: "daemon too old to report", daemon: ""},
		{name: "unreachable daemon", err: errors.New("refused")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := &fakeUpstream{fakeController: &fakeController{}, version: tt.daemon, err: tt.err}
			s := New(up)
			note := s.versionSkewNote(context.Background())
			if (note != "") != tt.wantNote {
				t.Fatalf("note = %q, want note: %v", note, tt.wantNote)
			}
			if tt.wantNote && !strings.Contains(note, "0.15.1") {
				t.Fatalf("note %q does not name the daemon build", note)
			}
			s.versionSkewNote(context.Background())
			if up.calls != 1 {
				t.Fatalf("daemon asked %d times, want once per recheck interval", up.calls)
			}
		})
	}
}

func TestWithSkewNoteAppendsWithoutTouchingStructuredContent(t *testing.T) {
	result, _ := toolJSON(map[string]any{"ok": true}, nil)
	got := withSkewNote(result, "skew").(map[string]any)
	content := got["content"].([]toolContent)
	if len(content) != 2 || content[1].Text != "skew" {
		t.Fatalf("content = %+v, want the payload then the note", content)
	}
	if len(result.(map[string]any)["content"].([]toolContent)) != 1 {
		t.Fatal("the original result was mutated")
	}
	if withSkewNote("plain", "skew") != "plain" {
		t.Fatal("a non-map result must pass through unchanged")
	}
}
