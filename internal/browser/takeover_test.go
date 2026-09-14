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
	"unicode/utf8"

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
		// A caller-supplied expression is the most capable page-acting route in
		// the product: brw_evaluate can click, type and navigate in one string.
		{takeoverActionEvaluateScript, func(m *Manager) error {
			_, err := m.Evaluate(ctx, "document.querySelector('button').click()")
			return err
		}},
		// A forged trace label must not buy an exemption. The label crosses HTTP
		// as a request field, so only the expression can decide.
		{takeoverActionEvaluateScript, func(m *Manager) error {
			_, err := m.Evaluate(WithTraceLabel(ctx, TraceActionGet, "text #go"), "document.querySelector('button').click()")
			return err
		}},
		{"open", func(m *Manager) error { _, err := m.Open(ctx, "https://example.test/"); return err }},
		{"open", func(m *Manager) error { _, err := m.OpenIncognito(ctx, "https://example.test/"); return err }},
		{"focus_tab", func(m *Manager) error { return m.FocusTab(ctx, "tab-1") }},
		{"close_tab", func(m *Manager) error { return m.CloseTab(ctx, "tab-1") }},
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
	var files []*ast.File
	names := map[*ast.File]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
		names[file] = name
	}
	// The action is nearly always a package constant rather than a string in the
	// call, so the constants have to be resolved before the call sites are read.
	constants := packageStringConstants(files)

	actions := map[string]recordedAction{}
	// Tracked as the files are read, not derived from the map above: an action
	// both idioms record would otherwise collapse into whichever was seen last.
	seen := map[string]bool{}
	for _, file := range files {
		for action, via := range recordedTraceActions(file, constants) {
			actions[action] = recordedAction{file: names[file], via: via}
			seen[via] = true
		}
	}
	if len(actions) == 0 {
		t.Fatal("found no recorded action with a readable name: the shape this test reads has changed")
	}
	// Both idioms have to be reachable. recordTrace with a TraceEntry literal is
	// how the input actions record; recordObservation is how Open, FocusTab,
	// CloseTab and the reads do. A parser that can only see the first half
	// silently stops covering exactly the call sites this classification exists
	// to catch — which is what it did.
	for _, idiom := range []string{"recordTrace", "recordObservation"} {
		if !seen[idiom] {
			t.Fatalf("no %s call site was read; half the inventory is invisible to this test", idiom)
		}
	}

	var unclassified []string
	for action, recorded := range actions {
		if observationActions[action] || takeoverGuardedActions[action] || takeoverExemptActions[action] {
			continue
		}
		unclassified = append(unclassified, action+" ("+recorded.file+", via "+recorded.via+")")
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("trace actions in no takeover class: %v — add it to takeoverGuardedActions and guard the method, or declare it an observation", unclassified)
	}
}

// recordedAction is where one action name was found and which recording idiom
// carried it, so a failure names the call site to fix.
type recordedAction struct {
	file string
	via  string
}

// packageStringConstants maps every package-level untyped string constant to its
// value, so a call site passing TraceActionOpen reads the same as one passing
// "open".
func packageStringConstants(files []*ast.File) map[string]string {
	values := map[string]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range value.Names {
					if i >= len(value.Values) {
						continue
					}
					lit, ok := value.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					if text, err := strconv.Unquote(lit.Value); err == nil {
						values[name.Name] = text
					}
				}
			}
		}
	}
	return values
}

// recordedTraceActions returns the action names one file records, mapped to the
// idiom that recorded them. A computed action (mouseHalf passes its parameter
// through) has no name to read here and is covered by the refusal table instead.
func recordedTraceActions(file *ast.File, constants map[string]string) map[string]string {
	found := map[string]string{}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		switch sel.Sel.Name {
		case "recordTrace":
			if action, ok := traceEntryAction(call.Args[1], constants); ok {
				found[action] = "recordTrace"
			}
		case "recordObservation":
			// recordObservation(tabID, action, text, start, err): the action is a
			// plain argument, not a field of a struct literal.
			if action, ok := actionName(call.Args[1], constants); ok {
				found[action] = "recordObservation"
			}
		}
		return true
	})
	return found
}

// traceEntryAction digs the Action string out of what recordTrace was handed:
// a TraceEntry literal, possibly wrapped in RedactTraceEntry(ctx, ...), or a
// NewObservationTrace(action, ...) call that builds one.
func traceEntryAction(expr ast.Expr, constants map[string]string) (string, bool) {
	if wrapper, ok := expr.(*ast.CallExpr); ok {
		if fun, ok := wrapper.Fun.(*ast.Ident); ok && fun.Name == "NewObservationTrace" && len(wrapper.Args) > 0 {
			return actionName(wrapper.Args[0], constants)
		}
		for _, arg := range wrapper.Args {
			if action, ok := traceEntryAction(arg, constants); ok {
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
		return actionName(kv.Value, constants)
	}
	return "", false
}

// actionName reads an action argument written either as a string literal or as
// one of the package's own constants.
func actionName(expr ast.Expr, constants map[string]string) (string, bool) {
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return "", false
		}
		action, err := strconv.Unquote(value.Value)
		return action, err == nil
	case *ast.Ident:
		action, ok := constants[value.Name]
		return action, ok
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
		// The tab verbs are observations to a replayer and input to a hold; they
		// have their own case below.
		if takeoverGuardedActions[action] {
			continue
		}
		if err := m.guardTakeover(action); err != nil {
			t.Errorf("observation %q was refused during takeover: %v", action, err)
		}
	}
	for _, action := range []string{TraceActionOpen, TraceActionFocusTab, TraceActionCloseTab} {
		if err := m.guardTakeover(action); err == nil {
			t.Errorf("%q was allowed during a hold; it moves or destroys the tab the human is aiming at", action)
		}
	}
}

// The exemption that keeps brw_get and brw_frame answering through a hold is
// decided from the expression, because the expression is the one part of an
// /api/page/evaluate request a caller cannot forge. It therefore has to accept
// exactly what brw emits — one generated call, the caller controlling its string
// arguments and nothing else. Anything a caller can append to that call runs
// during the hold with the full reach of an evaluate.
func TestOnlyAWholeGeneratedReadScriptIsExemptFromAHold(t *testing.T) {
	get := snapshot.BuildGetExpression("text", "#count", "")
	frame := snapshot.BuildFrameSwitchExpression("main")
	const drive = "document.getElementById('go').click()"

	tests := []struct {
		name       string
		action     string
		expression string
		want       bool
	}{
		{"a generated get", TraceActionGet, get, true},
		{"a generated get with every argument set", TraceActionGet, snapshot.BuildGetExpression("attr", "#field", "value"), true},
		{"a generated get whose target carries quotes and a paren", TraceActionGet, snapshot.BuildGetExpression("text", `[data-x="a)b'c"]`, ""), true},
		{"a generated frame switch", TraceActionFrame, frame, true},
		{"hand-written javascript", TraceActionGet, "document.querySelector('button').click()", false},
		{"hand-written javascript labelled as a frame switch", TraceActionFrame, "document.querySelector('button').click()", false},
		{"a generated get under an ungenerated verb", TraceActionEvaluate, get, false},
		{"a generated get under the other generated verb", TraceActionFrame, get, false},
		{"javascript appended to a generated get", TraceActionGet, get + ";" + drive, false},
		{"javascript appended to a generated frame switch", TraceActionFrame, frame + ";" + drive, false},
		{"javascript appended through the comma operator", TraceActionGet, get + "," + drive, false},
		{"javascript chained onto the generated call", TraceActionGet, get + ".toString()", false},
		{"javascript prepended to a generated get", TraceActionGet, drive + ";" + get, false},
		{"the generated script called with an expression argument", TraceActionGet, snapshot.GetScript + `("text","#count",document.title)`, false},
		{"the generated script called with too few arguments", TraceActionGet, snapshot.GetScript + `("text","#count")`, false},
		{"the generated script called with an extra argument", TraceActionGet, snapshot.GetScript + `("text","#count","","")`, false},
		{"the generated script with no call at all", TraceActionGet, snapshot.GetScript, false},
		{"an empty expression", TraceActionGet, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGeneratedReadExpression(tt.action, tt.expression); got != tt.want {
				t.Fatalf("isGeneratedReadExpression(%q, %d bytes ending %q) = %v, want %v",
					tt.action, len(tt.expression), tail(tt.expression, 48), got, tt.want)
			}
		})
	}
}

// tail is the end of an expression, which is where a forgery lives: the
// generated scripts are ~10 KB of walker and printing one whole fails a test
// unreadably. The cut is moved onto a rune boundary so the diagnostic stays
// printable.
func tail(expression string, n int) string {
	if len(expression) <= n {
		return expression
	}
	cut := len(expression) - n
	for cut < len(expression) && !utf8.RuneStart(expression[cut]) {
		cut++
	}
	return "…" + expression[cut:]
}

// A batch that was already running when the human took over must stop driving
// the page. ExecuteBatch guards once at entry; its steps reach the low-level
// helpers directly, so each step has to be guarded in its own right.
func TestBatchStepsRefuseOnceAHumanHoldsTakeover(t *testing.T) {
	tests := []struct {
		name    string
		step    BatchStep
		refused bool
	}{
		{"click", BatchStep{Action: "click", Ref: "e1"}, true},
		{"click_text", BatchStep{Action: "click_text", Text: "Go"}, true},
		{"type", BatchStep{Action: "type", Ref: "e1", Text: "hello"}, true},
		{"fill", BatchStep{Action: "fill", Ref: "e1", Text: "hello"}, true},
		{"select", BatchStep{Action: "select", Ref: "e1", Value: "one"}, true},
		{"press", BatchStep{Action: "press", Key: "Enter"}, true},
		{"scroll", BatchStep{Action: "scroll", Direction: "down"}, true},
		{"hover", BatchStep{Action: "hover", Ref: "e1"}, true},
		{"open", BatchStep{Action: "open", URL: "https://example.test/"}, true},
		{"navigate_to", BatchStep{Action: "navigate_to", URL: "https://example.test/"}, true},
		{"focus_tab", BatchStep{Action: "focus_tab", ID: "tab-1"}, true},
		// Read-only steps stay allowed, or an agent could not find out what the
		// human did without starting a fresh batch.
		{"wait", BatchStep{Action: "wait", Condition: "idle"}, false},
		{"assert_visible", BatchStep{Action: "assert_visible", Ref: "e1"}, false},
		{"assert_text", BatchStep{Action: "assert_text", Ref: "e1", Text: "Go"}, false},
		{"assert_value", BatchStep{Action: "assert_value", Ref: "e1", Value: "one"}, false},
		{"assert_hidden", BatchStep{Action: "assert_hidden", Ref: "e1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newBrowserlessManager()
			if _, err := m.AcquireTakeover("operator", time.Minute); err != nil {
				t.Fatalf("acquire takeover: %v", err)
			}
			sr := m.executeBatchStep(context.Background(), "tab-1", 3, tt.step)
			refused := strings.Contains(sr.Error, "holds takeover of this browser")
			if refused != tt.refused {
				t.Fatalf("step %q refused = %v (%q), want %v", tt.step.Action, refused, sr.Error, tt.refused)
			}
			if !tt.refused {
				return
			}
			if sr.OK {
				t.Error("a refused step reported OK; the batch would run on to the next one")
			}
			if sr.Index != 3 || sr.Action != tt.step.Action {
				t.Errorf("refusal reported as step %d %q, want 3 %q", sr.Index, sr.Action, tt.step.Action)
			}
		})
	}
}

// Every step verb a batch accepts must be classified, or the fail-closed guard
// refuses a read-only step and a batch stops working during a hold for no reason.
func TestEveryBatchStepVerbIsClassifiedForTakeover(t *testing.T) {
	source, err := os.ReadFile("manager.go")
	if err != nil {
		t.Fatalf("read manager.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "manager.go", source, 0)
	if err != nil {
		t.Fatalf("parse manager.go: %v", err)
	}
	verbs := batchStepVerbs(file)
	if len(verbs) == 0 {
		t.Fatal("found no batch step verbs: the shape this test reads has changed")
	}
	var unclassified []string
	for _, verb := range verbs {
		if takeoverReadOnlySteps[verb] || takeoverGuardedActions[verb] || observationActions[verb] {
			continue
		}
		unclassified = append(unclassified, verb)
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Errorf("batch step verbs in no takeover class: %v — name the read-only ones in takeoverReadOnlySteps, guard the rest", unclassified)
	}
}

// batchStepVerbs returns the case labels of executeBatchStep's action switch.
func batchStepVerbs(file *ast.File) []string {
	var verbs []string
	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.FuncDecl)
		if !ok || decl.Name.Name != "executeBatchStep" {
			return true
		}
		ast.Inspect(decl.Body, func(inner ast.Node) bool {
			clause, ok := inner.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if value, err := strconv.Unquote(lit.Value); err == nil {
						verbs = append(verbs, value)
					}
				}
			}
			return true
		})
		return false
	})
	return verbs
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
