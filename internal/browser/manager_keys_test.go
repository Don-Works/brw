package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

// modifierRecorderFixture records the modifier state of every mouse and key
// event it sees, which is the only way to prove a key was actually HELD: a test
// that asserts "the CDP call was made" cannot tell a held Shift from a Shift
// pressed and released before the drag started.
const modifierRecorderFixture = `<!doctype html><html><head><meta charset="utf-8"><title>modifier recorder</title></head>
<body style="margin:0">
<div id="pad" style="position:absolute;left:20px;top:20px;width:320px;height:220px;background:#eee"></div>
<button id="target" style="position:absolute;left:20px;top:260px;width:160px;height:40px">Target Button</button>
<script>
window.__events = [];
['mousedown','mousemove','mouseup','click','keydown','keyup'].forEach(function(type){
  window.addEventListener(type, function(e){
    window.__events.push({
      type: type,
      key: e.key || '',
      shift: !!e.shiftKey,
      ctrl: !!e.ctrlKey,
      alt: !!e.altKey,
      meta: !!e.metaKey
    });
  }, true);
});
</script>
</body></html>`

type recordedEvent struct {
	Type  string `json:"type"`
	Key   string `json:"key"`
	Shift bool   `json:"shift"`
	Ctrl  bool   `json:"ctrl"`
	Alt   bool   `json:"alt"`
	Meta  bool   `json:"meta"`
}

func openModifierFixture(t *testing.T, m *Manager, ctx context.Context) context.Context {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, modifierRecorderFixture)
	}))
	t.Cleanup(srv.Close)
	opened, err := m.Open(ctx, srv.URL)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	return WithTabID(ctx, opened.Tab.ID)
}

func recordedEvents(t *testing.T, m *Manager, ctx context.Context) []recordedEvent {
	t.Helper()
	value, err := m.Evaluate(ctx, "JSON.stringify(window.__events)")
	if err != nil {
		t.Fatalf("read recorded events: %v", err)
	}
	raw, ok := value.(string)
	if !ok {
		t.Fatalf("recorded events came back as %T, want a JSON string", value)
	}
	var events []recordedEvent
	if err := json.Unmarshal([]byte(raw), &events); err != nil {
		t.Fatalf("decode recorded events %q: %v", raw, err)
	}
	return events
}

func resetRecordedEvents(t *testing.T, m *Manager, ctx context.Context) {
	t.Helper()
	if _, err := m.Evaluate(ctx, "window.__events.length = 0"); err != nil {
		t.Fatalf("reset recorded events: %v", err)
	}
}

func pointAt(x, y float64) MousePoint {
	return MousePoint{X: &x, Y: &y}
}

// TestKeyHoldAppliesModifierThroughDrag is the proof that KeyDown really holds:
// the page must see shiftKey true on the mousemoves BETWEEN the keydown and the
// keyup, and false once the key is released.
func TestKeyHoldAppliesModifierThroughDrag(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tabCtx := openModifierFixture(t, m, ctx)

	held, err := m.KeyDown(tabCtx, KeyHoldOptions{Key: "Shift"})
	if err != nil {
		t.Fatalf("key down: %v", err)
	}
	if len(held.Held) != 1 || held.Held[0] != "Shift" {
		t.Fatalf("held keys after key down = %v, want [Shift]", held.Held)
	}

	events := recordedEvents(t, m, tabCtx)
	var sawKeyDown bool
	for _, e := range events {
		if e.Type == "keydown" {
			sawKeyDown = true
			if e.Key != "Shift" {
				t.Fatalf("keydown key = %q, want Shift", e.Key)
			}
			if !e.Shift {
				t.Fatal("the Shift keydown itself reported shiftKey false")
			}
		}
		if e.Type == "keyup" {
			t.Fatal("a keyup arrived while the key was supposed to be held")
		}
	}
	if !sawKeyDown {
		t.Fatalf("page saw no keydown at all; events = %+v", events)
	}

	resetRecordedEvents(t, m, tabCtx)
	if _, err := m.Drag(tabCtx, DragOptions{From: pointAt(40, 40), To: pointAt(300, 200), Steps: 8}); err != nil {
		t.Fatalf("drag with Shift held: %v", err)
	}
	moves := 0
	for _, e := range recordedEvents(t, m, tabCtx) {
		if e.Type != "mousemove" && e.Type != "mousedown" && e.Type != "mouseup" {
			continue
		}
		if e.Type == "mousemove" {
			moves++
		}
		if !e.Shift {
			t.Fatalf("%s during a Shift-held drag reported shiftKey false: the modifier was not carried on the mouse event", e.Type)
		}
	}
	if moves < 3 {
		t.Fatalf("recorded %d mousemove events during the drag, want the interpolated sequence", moves)
	}

	resetRecordedEvents(t, m, tabCtx)
	released, err := m.KeyUp(tabCtx, KeyHoldOptions{Key: "Shift"})
	if err != nil {
		t.Fatalf("key up: %v", err)
	}
	if len(released.Held) != 0 {
		t.Fatalf("held keys after key up = %v, want none", released.Held)
	}
	var sawKeyUp bool
	for _, e := range recordedEvents(t, m, tabCtx) {
		if e.Type == "keyup" {
			sawKeyUp = true
			if e.Key != "Shift" {
				t.Fatalf("keyup key = %q, want Shift", e.Key)
			}
			if e.Shift {
				t.Fatal("the Shift keyup reported shiftKey still true")
			}
		}
	}
	if !sawKeyUp {
		t.Fatal("page saw no keyup after KeyUp")
	}

	// The release has to actually release: the same drag must now be modifier-free.
	resetRecordedEvents(t, m, tabCtx)
	if _, err := m.Drag(tabCtx, DragOptions{From: pointAt(40, 40), To: pointAt(300, 200), Steps: 8}); err != nil {
		t.Fatalf("drag after release: %v", err)
	}
	for _, e := range recordedEvents(t, m, tabCtx) {
		if e.Type == "mousemove" && e.Shift {
			t.Fatal("a mousemove after KeyUp still reported shiftKey true: the hold leaked past its release")
		}
	}
}

// TestKeyHoldAppliesModifierToRefClick covers Shift+click on a semantic ref —
// the click-range gesture. The default ref click builds its MouseEvent in page
// script, where shiftKey is always false, so this also guards the routing to the
// trusted CDP path.
//
// Shift rather than Control because Chromium on macOS maps Ctrl+left-click to a
// context menu and emits no click event at all; the mousedown still carries
// ctrlKey, which is asserted below.
func TestKeyHoldAppliesModifierToRefClick(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tabCtx := openModifierFixture(t, m, ctx)

	snap, err := m.Snapshot(tabCtx, snapshot.SnapshotOptions{Mode: "all"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	ref := ""
	for _, el := range snap.Elements {
		if el.Name == "Target Button" {
			ref = el.Ref
			break
		}
	}
	if ref == "" {
		t.Fatalf("no ref for the target button; snapshot returned %d elements", len(snap.Elements))
	}

	if _, err := m.KeyDown(tabCtx, KeyHoldOptions{Key: "Shift"}); err != nil {
		t.Fatalf("hold shift: %v", err)
	}
	resetRecordedEvents(t, m, tabCtx)
	if _, err := m.Click(tabCtx, ref); err != nil {
		t.Fatalf("shift+click: %v", err)
	}
	var sawClick bool
	events := recordedEvents(t, m, tabCtx)
	for _, e := range events {
		if e.Type != "click" {
			continue
		}
		sawClick = true
		if !e.Shift {
			t.Fatal("click during a held Shift reported shiftKey false")
		}
	}
	if !sawClick {
		t.Fatalf("the page saw no click at all; events = %+v", events)
	}

	// Two held keys must OR into one mask on every later event.
	if _, err := m.KeyDown(tabCtx, KeyHoldOptions{Key: "Control"}); err != nil {
		t.Fatalf("hold control: %v", err)
	}
	resetRecordedEvents(t, m, tabCtx)
	if _, err := m.MouseDown(tabCtx, MouseButtonOptions{MousePoint: pointAt(40, 40)}); err != nil {
		t.Fatalf("mouse down with two modifiers held: %v", err)
	}
	var sawDown bool
	for _, e := range recordedEvents(t, m, tabCtx) {
		if e.Type != "mousedown" {
			continue
		}
		sawDown = true
		if !e.Shift || !e.Ctrl {
			t.Fatalf("mousedown carried shift=%v ctrl=%v, want both held modifiers", e.Shift, e.Ctrl)
		}
	}
	if !sawDown {
		t.Fatal("the page saw no mousedown at all")
	}
	if _, err := m.MouseUp(tabCtx, MouseButtonOptions{MousePoint: pointAt(40, 40)}); err != nil {
		t.Fatalf("mouse up: %v", err)
	}

	// key "all" is the recovery for a flow that does not remember what it holds.
	released, err := m.KeyUp(tabCtx, KeyHoldOptions{Key: "all"})
	if err != nil {
		t.Fatalf("release all: %v", err)
	}
	if len(released.Held) != 0 {
		t.Fatalf("held keys after releasing all = %v, want none", released.Held)
	}
}

func TestKeyHoldRejectsEmptyKey(t *testing.T) {
	m := &Manager{}
	if _, err := m.KeyDown(context.Background(), KeyHoldOptions{}); err == nil {
		t.Fatal("KeyDown with no key should fail")
	}
	if _, err := m.KeyUp(context.Background(), KeyHoldOptions{}); err == nil {
		t.Fatal("KeyUp with no key should fail")
	}
}
