package browser

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/Don-Works/brw/internal/store"
)

// newBrowserlessManager builds a Manager with no Chrome behind it. The takeover
// guard runs before any browser round trip, so an action that gets past it fails
// with "no browser" instead — which is what makes the two outcomes tellable
// apart without launching Chrome for every row of the table.
func newBrowserlessManager() *Manager {
	// An already-cancelled browser context: every CDP round trip fails
	// immediately rather than waiting on a connection that will never exist.
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	return &Manager{
		browserCtx:    dead,
		browserCancel: cancel,
		tabContexts:   map[string]tabContext{},
		refs:          store.New(),
		timeout:       time.Second,
		lastState:     map[string]*SemanticState{},
		observedState: map[string]*SemanticState{},
		versions:      map[string]int64{},
		cancels:       newCancelRegistry(),
	}
}

// Every agent input action must refuse while a human holds the browser. The
// guard runs before the method touches the browser at all, so this table needs
// no Chrome: a method that reached CDP would fail with a transport error
// instead, which is exactly the distinction being asserted.
func TestTakeoverRefusesEveryAgentInputAction(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		action string
		call   func(m *Manager) error
	}{
		{"click", func(m *Manager) error { _, err := m.Click(ctx, "e1"); return err }},
		{"click_text", func(m *Manager) error {
			_, err := m.ClickText(ctx, snapshot.ClickTextOptions{Text: "Go"})
			return err
		}},
		{"click_button", func(m *Manager) error {
			_, err := m.ClickButton(ctx, ClickButtonOptions{MousePoint: MousePoint{Ref: "e1"}})
			return err
		}},
		{"click_xy", func(m *Manager) error { _, err := m.ClickXY(ctx, 10, 10); return err }},
		{"drag", func(m *Manager) error {
			_, err := m.Drag(ctx, DragOptions{From: MousePoint{Ref: "e1"}, To: MousePoint{Ref: "e2"}})
			return err
		}},
		{"mouse_down", func(m *Manager) error {
			_, err := m.MouseDown(ctx, MouseButtonOptions{MousePoint: MousePoint{Ref: "e1"}})
			return err
		}},
		{"mouse_up", func(m *Manager) error {
			_, err := m.MouseUp(ctx, MouseButtonOptions{MousePoint: MousePoint{Ref: "e1"}})
			return err
		}},
		{"hover", func(m *Manager) error { _, err := m.Hover(ctx, "e1"); return err }},
		{"type", func(m *Manager) error { _, err := m.Type(ctx, "e1", "hello"); return err }},
		{"fill", func(m *Manager) error {
			_, err := m.Fill(ctx, snapshot.FillOptions{Ref: "e1", Text: "hello"})
			return err
		}},
		{"select", func(m *Manager) error { _, err := m.Select(ctx, "e1", "one"); return err }},
		{"press", func(m *Manager) error { _, err := m.Press(ctx, "Enter"); return err }},
		{"key_down", func(m *Manager) error { _, err := m.KeyDown(ctx, KeyHoldOptions{Key: "Control"}); return err }},
		{"key_up", func(m *Manager) error { _, err := m.KeyUp(ctx, KeyHoldOptions{Key: "Control"}); return err }},
		{"scroll", func(m *Manager) error { _, err := m.Scroll(ctx, "down"); return err }},
		{"focus", func(m *Manager) error { _, err := m.Focus(ctx, "e1"); return err }},
		{"focus", func(m *Manager) error { return m.FocusRef(ctx, "e1") }},
		{"clipboard", func(m *Manager) error {
			_, err := m.Clipboard(ctx, ClipboardOptions{Action: "paste"})
			return err
		}},
		{"navigate", func(m *Manager) error { _, err := m.Navigate(ctx, "back"); return err }},
		{"navigate_to", func(m *Manager) error { _, err := m.NavigateTo(ctx, "https://example.test/"); return err }},
		{"pushstate", func(m *Manager) error {
			_, err := m.PushState(ctx, HistoryStateOptions{URL: "/next"})
			return err
		}},
		{"upload_file", func(m *Manager) error {
			_, err := m.UploadFile(ctx, snapshot.UploadOptions{Ref: "e1", Paths: []string{"/dev/null"}})
			return err
		}},
		{"commit", func(m *Manager) error { return m.CommitField(ctx, "e1") }},
		{"plan", func(m *Manager) error {
			_, err := m.ExecutePlan(ctx, []PlanStep{{Action: "click", Ref: "e1"}})
			return err
		}},
		{"batch", func(m *Manager) error {
			_, err := m.ExecuteBatch(ctx, []BatchStep{{Action: "click", Ref: "e1"}})
			return err
		}},
	}

	covered := map[string]bool{}
	for _, tt := range tests {
		covered[tt.action] = true
		t.Run(tt.action, func(t *testing.T) {
			m := newBrowserlessManager()
			grant, err := m.AcquireTakeover("operator", time.Minute)
			if err != nil {
				t.Fatalf("acquire takeover: %v", err)
			}
			err = tt.call(m)
			var refused *TakeoverRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("%s returned %v, want a *TakeoverRefusedError", tt.action, err)
			}
			if refused.Action != tt.action {
				t.Errorf("refusal names %q, want %q", refused.Action, tt.action)
			}
			if !strings.Contains(refused.Error(), "operator") {
				t.Errorf("refusal should name the holder; got %q", refused.Error())
			}

			// Releasing must let the action through again, or takeover would be
			// a one-way door rather than a handshake. It no longer refuses; it
			// fails on the browser this Manager does not have.
			if err := m.ReleaseTakeover(grant.Token); err != nil {
				t.Fatalf("release takeover: %v", err)
			}
			if err := tt.call(m); errors.As(err, &refused) {
				t.Fatalf("%s still refused after release: %v", tt.action, err)
			}
		})
	}

	var missing []string
	for action := range takeoverGuardedActions {
		if !covered[action] {
			missing = append(missing, action)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("guarded actions with no refusal case: %v", missing)
	}
}

// Every action that reaches the trace is either something the agent did to the
// page (and must be refused during takeover), something it merely observed, or
// the human's own forwarded input. A new action in none of those three sets is
// an input path that ignores takeover, which is how a handshake quietly stops
// being one.
func TestEveryRecordedTraceActionIsClassified(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	actions := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for action := range recordedTraceActions(file) {
			actions[action] = name
		}
	}
	if len(actions) == 0 {
		t.Fatal("found no recordTrace call with a literal action: the shape this test reads has changed")
	}

	var unclassified []string
	for action, file := range actions {
		if observationActions[action] || takeoverGuardedActions[action] || takeoverExemptActions[action] {
			continue
		}
		unclassified = append(unclassified, action+" ("+file+")")
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("trace actions in no takeover class: %v — add it to takeoverGuardedActions and guard the method, or declare it an observation", unclassified)
	}
}

// recordedTraceActions returns the literal Action values passed to recordTrace
// in one file. A computed action (mouseHalf passes its parameter through) has no
// literal to read and is covered by the refusal table instead.
func recordedTraceActions(file *ast.File) map[string]bool {
	found := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "recordTrace" || len(call.Args) < 2 {
			return true
		}
		if action, ok := traceEntryAction(call.Args[1]); ok {
			found[action] = true
		}
		return true
	})
	return found
}

// traceEntryAction digs the Action string out of a TraceEntry literal, which may
// be wrapped in RedactTraceEntry(ctx, TraceEntry{...}).
func traceEntryAction(expr ast.Expr) (string, bool) {
	if wrapper, ok := expr.(*ast.CallExpr); ok {
		for _, arg := range wrapper.Args {
			if action, ok := traceEntryAction(arg); ok {
				return action, true
			}
		}
		return "", false
	}
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return "", false
	}
	for _, element := range lit.Elts {
		kv, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Action" {
			continue
		}
		value, ok := kv.Value.(*ast.BasicLit)
		if !ok || value.Kind != token.STRING {
			return "", false
		}
		action, err := strconv.Unquote(value.Value)
		if err != nil {
			return "", false
		}
		return action, true
	}
	return "", false
}

// Reads are the agent's way of finding out what the human did, so takeover must
// not refuse them. A hold that blinded the agent would force it to start over
// rather than resume.
func TestTakeoverDoesNotRefuseObservations(t *testing.T) {
	m := newBrowserlessManager()
	if _, err := m.AcquireTakeover("operator", time.Minute); err != nil {
		t.Fatalf("acquire takeover: %v", err)
	}
	for action := range observationActions {
		if err := m.guardTakeover(action); err != nil {
			t.Errorf("observation %q was refused during takeover: %v", action, err)
		}
	}
}

func TestTakeoverGrantLifecycle(t *testing.T) {
	t.Run("a second session cannot steal a live hold", func(t *testing.T) {
		m := newBrowserlessManager()
		if _, err := m.AcquireTakeover("first", time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := m.AcquireTakeover("second", time.Minute); !errors.Is(err, ErrTakeoverBusy) {
			t.Fatalf("second acquire = %v, want ErrTakeoverBusy", err)
		}
	})

	t.Run("the wrong token cannot release someone else's hold", func(t *testing.T) {
		m := newBrowserlessManager()
		if _, err := m.AcquireTakeover("first", time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := m.ReleaseTakeover("not-the-token"); !errors.Is(err, ErrTakeoverNotHeld) {
			t.Fatalf("release with a wrong token = %v, want ErrTakeoverNotHeld", err)
		}
		if !m.TakeoverState().Held {
			t.Fatal("the hold was released by a session that did not own it")
		}
	})

	t.Run("an unrenewed hold expires and the agent resumes", func(t *testing.T) {
		m := newBrowserlessManager()
		grant, err := m.AcquireTakeover("first", time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
		if m.TakeoverState().Held {
			t.Fatal("an expired hold still reports as held")
		}
		if err := m.guardTakeover("click"); err != nil {
			t.Fatalf("an expired hold still refuses agent actions: %v", err)
		}
		if err := m.DispatchTakeoverInput(context.Background(), grant.Token,
			TakeoverInput{Kind: "mouse", Type: "mousePressed"}); !errors.Is(err, ErrTakeoverNotHeld) {
			t.Fatalf("input on an expired grant = %v, want ErrTakeoverNotHeld", err)
		}
	})

	t.Run("a renewed hold keeps refusing", func(t *testing.T) {
		m := newBrowserlessManager()
		grant, err := m.AcquireTakeover("first", 20*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.RenewTakeover(grant.Token, time.Minute); err != nil {
			t.Fatalf("renew: %v", err)
		}
		time.Sleep(30 * time.Millisecond)
		if err := m.guardTakeover("click"); err == nil {
			t.Fatal("a renewed hold stopped refusing at the original expiry")
		}
	})

	t.Run("input without a grant never reaches dispatch", func(t *testing.T) {
		m := newBrowserlessManager()
		err := m.DispatchTakeoverInput(context.Background(), "",
			TakeoverInput{Kind: "mouse", Type: "mousePressed", X: 5, Y: 5})
		if !errors.Is(err, ErrTakeoverNotHeld) {
			t.Fatalf("ungranted input = %v, want ErrTakeoverNotHeld", err)
		}
	})
}

// A keystroke's character must never reach the trace: a human types passwords
// through takeover and the trace is served over the control plane.
func TestTakeoverInputLabelOmitsTypedCharacters(t *testing.T) {
	tests := []struct {
		name  string
		event TakeoverInput
		want  string
	}{
		{"mouse reports its point", TakeoverInput{Kind: "mouse", Type: "mousePressed", X: 12, Y: 34}, "human mousePressed at (12,34)"},
		{"a character key reports the event only", TakeoverInput{Kind: "key", Type: "keyDown", Key: "s", Text: "s"}, "human keyDown"},
		{"a named key may be reported", TakeoverInput{Kind: "key", Type: "keyDown", Key: "Enter"}, "human keyDown Enter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			label := takeoverInputLabel(tt.event)
			if label != tt.want {
				t.Fatalf("label = %q, want %q", label, tt.want)
			}
			if len(tt.event.Text) == 1 && strings.Contains(label, tt.event.Text) {
				t.Errorf("label leaked the typed character: %q", label)
			}
		})
	}
}

func TestTakeoverInputValidation(t *testing.T) {
	tests := []struct {
		name    string
		event   TakeoverInput
		wantErr string
	}{
		{"mouse press is accepted", TakeoverInput{Kind: "mouse", Type: "mousePressed"}, ""},
		{"key down is accepted", TakeoverInput{Kind: "key", Type: "keyDown", Key: "a", Text: "a"}, ""},
		{"an unknown kind is refused", TakeoverInput{Kind: "touch", Type: "touchStart"}, "unsupported takeover input kind"},
		{"an unknown mouse type is refused", TakeoverInput{Kind: "mouse", Type: "mouseTeleported"}, "unsupported mouse event type"},
		{"an unknown key type is refused", TakeoverInput{Kind: "key", Type: "keySmashed"}, "unsupported key event type"},
		{"a pasted block is refused", TakeoverInput{Kind: "key", Type: "keyDown", Text: "a whole sentence"}, "too long for a single keystroke"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.event.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}
