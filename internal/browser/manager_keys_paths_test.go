package browser

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// targetButtonRef resolves the fixture's button the way an agent would.
func targetButtonRef(t *testing.T, m *Manager, ctx context.Context) string {
	t.Helper()
	snap, err := m.Snapshot(ctx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, el := range snap.Elements {
		if el.Name == "Target Button" {
			return el.Ref
		}
	}
	t.Fatalf("no ref for the target button; snapshot returned %d elements", len(snap.Elements))
	return ""
}

// TestHeldModifierReachesEveryActuationPath covers the paths a held key has to
// survive beyond the four that dispatch through CDP directly. Each of these
// used to resolve its target in page script and dispatch a synthesised
// MouseEvent or a keystroke built from the key string alone, both of which
// report shiftKey false whatever the tab is holding — so the action succeeded,
// the modifier was dropped, and nothing in the result said so.
//
// Every case asserts on what the PAGE saw, not on which CDP call was made: a
// test that checks the call cannot tell a held Shift from no Shift at all.
func TestHeldModifierReachesEveryActuationPath(t *testing.T) {
	tests := []struct {
		name string
		// eventType is the event that must carry shiftKey for this path.
		eventType string
		act       func(t *testing.T, m *Manager, ctx context.Context, ref string)
	}{
		{
			name:      "click_text",
			eventType: "click",
			act: func(t *testing.T, m *Manager, ctx context.Context, _ string) {
				if _, err := m.ClickText(ctx, snapshot.ClickTextOptions{Text: "Target Button"}); err != nil {
					t.Fatalf("click_text with Shift held: %v", err)
				}
			},
		},
		{
			name:      "batch click step",
			eventType: "click",
			act: func(t *testing.T, m *Manager, ctx context.Context, ref string) {
				result, err := m.ExecuteBatch(ctx, []BatchStep{{Action: "click", Ref: ref}})
				assertBatchOK(t, result, err)
			},
		},
		{
			name:      "batch click_text step",
			eventType: "click",
			act: func(t *testing.T, m *Manager, ctx context.Context, _ string) {
				result, err := m.ExecuteBatch(ctx, []BatchStep{{Action: "click_text", Text: "Target Button"}})
				assertBatchOK(t, result, err)
			},
		},
		{
			name:      "press",
			eventType: "keydown",
			act: func(t *testing.T, m *Manager, ctx context.Context, _ string) {
				if _, err := m.Press(ctx, "Tab"); err != nil {
					t.Fatalf("press with Shift held: %v", err)
				}
			},
		},
		{
			name:      "batch press step",
			eventType: "keydown",
			act: func(t *testing.T, m *Manager, ctx context.Context, _ string) {
				result, err := m.ExecuteBatch(ctx, []BatchStep{{Action: "press", Key: "Tab"}})
				assertBatchOK(t, result, err)
			},
		},
		{
			name:      "hover",
			eventType: "mousemove",
			act: func(t *testing.T, m *Manager, ctx context.Context, ref string) {
				if _, err := m.Hover(ctx, ref); err != nil {
					t.Fatalf("hover with Shift held: %v", err)
				}
			},
		},
		{
			name:      "batch hover step",
			eventType: "mousemove",
			act: func(t *testing.T, m *Manager, ctx context.Context, ref string) {
				result, err := m.ExecuteBatch(ctx, []BatchStep{{Action: "hover", Ref: ref}})
				assertBatchOK(t, result, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newHeadlessManager(t)
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			tabCtx := openModifierFixture(t, m, ctx)
			ref := targetButtonRef(t, m, tabCtx)

			if _, err := m.KeyDown(tabCtx, KeyHoldOptions{Key: "Shift"}); err != nil {
				t.Fatalf("hold shift: %v", err)
			}
			resetRecordedEvents(t, m, tabCtx)
			tt.act(t, m, tabCtx, ref)

			events := recordedEvents(t, m, tabCtx)
			seen := 0
			for _, e := range events {
				if e.Type != tt.eventType {
					continue
				}
				seen++
				if !e.Shift {
					t.Fatalf("%s reported shiftKey false during a held Shift: the modifier was dropped on this path", e.Type)
				}
			}
			if seen == 0 {
				t.Fatalf("the page saw no %s at all; events = %+v", tt.eventType, events)
			}
		})
	}
}

// TestPressTypesTheCharacterTheModifiersProduce: Chrome inserts the text of the
// key event exactly as it is sent and applies no keyboard layout of its own, so
// the modifier mask alone decides nothing about what appears in the field. A
// held Shift used to type the UNSHIFTED character with shiftKey true, and the
// chord form typed nothing at all — two different wrong answers for the same
// keystroke. The assertion is on the value the field ends up holding, because
// that is the thing an agent is wrong about.
func TestPressTypesTheCharacterTheModifiersProduce(t *testing.T) {
	tests := []struct {
		name string
		hold string
		key  string
		want string
	}{
		{name: "plain letter", key: "a", want: "a"},
		{name: "held shift", hold: "Shift", key: "a", want: "A"},
		{name: "shift chord", key: "shift+a", want: "A"},
		{name: "held shift on a digit", hold: "Shift", key: "1", want: "!"},
		{name: "shift chord on a digit", key: "shift+1", want: "!"},
		// Chrome suppresses insertion for a real accelerator, so an empty field
		// is the right answer here rather than a stray character.
		{name: "held control", hold: "Control", key: "a", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newHeadlessManager(t)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			opened, err := m.Open(ctx, `data:text/html,<input id="f">`)
			if err != nil {
				t.Fatalf("open fixture: %v", err)
			}
			tabCtx := WithTabID(ctx, opened.Tab.ID)
			if _, err := m.Evaluate(tabCtx, `document.querySelector('#f').focus(); true`); err != nil {
				t.Fatalf("focus the field: %v", err)
			}
			if tt.hold != "" {
				if _, err := m.KeyDown(tabCtx, KeyHoldOptions{Key: tt.hold}); err != nil {
					t.Fatalf("hold %s: %v", tt.hold, err)
				}
				defer func() {
					if _, err := m.KeyUp(tabCtx, KeyHoldOptions{Key: releaseAllKeyword}); err != nil {
						t.Fatalf("release held keys: %v", err)
					}
				}()
			}
			if _, err := m.Press(tabCtx, tt.key); err != nil {
				t.Fatalf("press %q: %v", tt.key, err)
			}
			value, err := m.Evaluate(tabCtx, `document.querySelector('#f').value`)
			if err != nil {
				t.Fatalf("read the field: %v", err)
			}
			if value != tt.want {
				t.Fatalf("press %q with %q held typed %#v, want %q", tt.key, tt.hold, value, tt.want)
			}
		})
	}
}

func assertBatchOK(t *testing.T, result BatchResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	for _, step := range result.Steps {
		if !step.OK {
			t.Fatalf("batch step %d (%s) failed: %s", step.Index, step.Action, step.Error)
		}
	}
	if len(result.Steps) == 0 {
		t.Fatal("batch ran no steps")
	}
}

// TestActionResultWarnsWhileKeysAreHeld: a held modifier is brw state that the
// page reports nowhere, so a flow that dies between KeyDown and KeyUp silently
// modifies every later click. The action result is the only place that can say so.
func TestActionResultWarnsWhileKeysAreHeld(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tabCtx := openModifierFixture(t, m, ctx)
	ref := targetButtonRef(t, m, tabCtx)

	clean, err := m.Click(tabCtx, ref)
	if err != nil {
		t.Fatalf("click with nothing held: %v", err)
	}
	if strings.Contains(clean.Warning, "held") {
		t.Fatalf("a click with no keys held warned about holds: %q", clean.Warning)
	}

	if _, err := m.KeyDown(tabCtx, KeyHoldOptions{Key: "Shift"}); err != nil {
		t.Fatalf("hold shift: %v", err)
	}
	if _, err := m.KeyDown(tabCtx, KeyHoldOptions{Key: "Control"}); err != nil {
		t.Fatalf("hold control: %v", err)
	}
	held, err := m.Click(tabCtx, ref)
	if err != nil {
		t.Fatalf("click with keys held: %v", err)
	}
	for _, want := range []string{"Control", "Shift", "key_up"} {
		if !strings.Contains(held.Warning, want) {
			t.Fatalf("click warning %q should name %q", held.Warning, want)
		}
	}

	if _, err := m.KeyUp(tabCtx, KeyHoldOptions{Key: "all"}); err != nil {
		t.Fatalf("release all: %v", err)
	}
	after, err := m.Click(tabCtx, ref)
	if err != nil {
		t.Fatalf("click after release: %v", err)
	}
	if strings.Contains(after.Warning, "held") {
		t.Fatalf("the hold warning outlived the release: %q", after.Warning)
	}
}

// TestKeyDownRejectsAChord: DescribeKey folds "ctrl+shift" into ONE descriptor
// with both bits, so holding it would dispatch a single Shift keydown the page
// never sees a Control keydown for, stamp ctrlKey onto later mouse events
// anyway, and leave KeyUp "Control" with nothing under that name to release.
func TestKeyDownRejectsAChord(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr string
	}{
		{name: "two modifiers", key: "ctrl+shift", wantErr: "chord"},
		{name: "modifier and letter", key: "shift+a", wantErr: "chord"},
		{name: "padded chord", key: " Meta+Enter ", wantErr: "chord"},
		{name: "plain modifier is fine", key: "Shift"},
		// "+" alone is not a chord. DescribeKey has never been able to name it
		// (it splits on "+" and is left with two empty parts), so the honest
		// refusal is "unrecognized", not "chord".
		{name: "the plus key alone is not a chord", key: "+", wantErr: "unrecognized"},
		{name: "ordinary key is fine", key: "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := describeHoldKey(tt.key)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("describeHoldKey(%q) = %v, want no error", tt.key, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("describeHoldKey(%q) was accepted, want a refusal naming a %s", tt.key, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("describeHoldKey(%q) = %q, want it to mention %q", tt.key, err, tt.wantErr)
			}
		})
	}
}
