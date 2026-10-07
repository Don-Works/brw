package siteconsent

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/stepscan"
)

var stepRunners = []struct {
	file     string
	function string
}{
	{"../browser/manager.go", "executePlanStep"},
	{"../browser/manager.go", "executeBatchStep"},
	{"../extensionbridge/bridge_plan.go", "executePlanStep"},
	{"../extensionbridge/bridge_batch.go", "executeBatchStep"},
}

func TestEveryPlanAndBatchStepActionIsClassified(t *testing.T) {
	implemented := map[string]bool{}
	for _, runner := range stepRunners {
		labels, err := stepscan.SwitchCases(runner.file, runner.function, "Action")
		if err != nil {
			t.Fatal(err)
		}
		for _, label := range labels {
			implemented[label] = true
		}
	}
	for action := range implemented {
		if _, classified := StepActions[action]; !classified {
			t.Errorf("plan/batch step %q is implemented but StepActions does not classify it, so consent decides nothing for it", action)
		}
	}
	for action := range StepActions {
		if !implemented[action] {
			t.Errorf("StepActions classifies %q, which no runner implements; a rule for a verb that does not exist is a rule nobody can test", action)
		}
	}
}

func TestBothBackendsRunTheSameSteps(t *testing.T) {
	for _, function := range []string{"executePlanStep", "executeBatchStep"} {
		direct := stepSet(t, "../browser/manager.go", function)
		bridge := stepSet(t, map[string]string{"executePlanStep": "../extensionbridge/bridge_plan.go", "executeBatchStep": "../extensionbridge/bridge_batch.go"}[function], function)
		if strings.Join(direct, ",") != strings.Join(bridge, ",") {
			t.Errorf("%s: direct CDP runs %v, the extension bridge runs %v", function, direct, bridge)
		}
	}
}

func stepSet(t *testing.T, file, function string) []string {
	t.Helper()
	labels, err := stepscan.SwitchCases(file, function, "Action")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(labels)
	return labels
}

func args(t *testing.T, value map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func pageAt(origin string) PageOriginFunc {
	return func(string) (string, error) { return origin, nil }
}

func TestEveryDestinationArgumentIsChecked(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args map[string]any
		want Scope
	}{
		{name: "open by url", tool: "brw_open", args: map[string]any{"url": "https://ungranted.test/x"}, want: ScopeRead},
		{name: "authenticate by origin", tool: "brw_authenticate", args: map[string]any{"origin": "https://ungranted.test", "username": "u", "password": "fixture-not-a-password"}, want: ScopeAct},
		{name: "authenticate by url", tool: "brw_authenticate", args: map[string]any{"url": "https://ungranted.test/x", "username": "u", "password": "fixture-not-a-password"}, want: ScopeAct},
		{name: "cookies by domain", tool: "brw_cookies", args: map[string]any{"action": "list", "domain": "ungranted.test"}, want: ScopeRead},
		{name: "cookies by url", tool: "brw_cookies", args: map[string]any{"action": "list", "url": "https://ungranted.test/x"}, want: ScopeRead},
		{name: "cookies that write", tool: "brw_cookies", args: map[string]any{"action": "set", "domain": "ungranted.test", "name": "a", "value": "b"}, want: ScopeAct},
		{name: "cookies that write a domain cookie", tool: "brw_cookies", args: map[string]any{"action": "set", "domain": ".ungranted.test", "name": "a", "value": "b"}, want: ScopeAct},
		{name: "extra headers by origins", tool: "brw_set_extra_headers", args: map[string]any{"origins": []any{map[string]any{"origin": "https://ungranted.test", "headers": map[string]any{"X-Test": "1"}}}}, want: ScopeAct},
		{name: "replay a GET", tool: "brw_replay_request", args: map[string]any{"method": "GET", "url": "https://ungranted.test/x"}, want: ScopeRead},
		{name: "replay a POST", tool: "brw_replay_request", args: map[string]any{"method": "POST", "url": "https://ungranted.test/x"}, want: ScopeAct},
		{name: "upload from a url", tool: "brw_upload_file", args: map[string]any{"ref": "e1", "url": "https://ungranted.test/f.pdf"}, want: ScopeRead},
		{name: "read_url", tool: "brw_read_url", args: map[string]any{"url": "https://ungranted.test/x"}, want: ScopeRead},
		{name: "navigate_to step in a plan", tool: "brw_plan", args: map[string]any{"steps": []any{map[string]any{"action": "navigate_to", "url": "https://ungranted.test/x"}}}, want: ScopeRead},
		{name: "navigate_to step in a batch", tool: "brw_batch", args: map[string]any{"steps": []any{map[string]any{"action": "navigate_to", "url": "https://ungranted.test/x"}}}, want: ScopeRead},
		{name: "open step in a batch", tool: "brw_batch", args: map[string]any{"steps": []any{map[string]any{"action": "open", "url": "https://ungranted.test/x"}}}, want: ScopeRead},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			guard := newTestGuard(t, AdminConfig{})
			err := guard.CheckTool(c.tool, args(t, c.args), pageAt("https://granted.test/"), nil)
			var refusal *NotGrantedError
			if !asNotGranted(err, &refusal) {
				t.Fatalf("%s reached https://ungranted.test with no grant: %v", c.tool, err)
			}
			if refusal.Origin != "https://ungranted.test" {
				t.Fatalf("the refusal names %q, not the origin the call reaches", refusal.Origin)
			}
			if refusal.Scope != c.want {
				t.Fatalf("%s needs scope %q, the gate asked for %q", c.tool, c.want, refusal.Scope)
			}

			if _, err := guard.Allow(GrantOptions{Origin: "https://ungranted.test", Scope: c.want, Actor: "fixture-user"}); err != nil {
				t.Fatal(err)
			}
			if _, err := guard.Allow(GrantOptions{Origin: "https://granted.test", Scope: ScopeAct, Actor: "fixture-user"}); err != nil {
				t.Fatal(err)
			}
			if err := guard.CheckTool(c.tool, args(t, c.args), pageAt("https://granted.test/"), nil); err != nil {
				t.Fatalf("the granted call was still refused: %v", err)
			}
		})
	}
}

func asNotGranted(err error, target **NotGrantedError) bool {
	refusal, ok := err.(*NotGrantedError)
	if ok {
		*target = refusal
	}
	return ok
}

func TestEveryDeclaredDestinationFieldProducesACheck(t *testing.T) {
	for name, rule := range ToolRules {
		if rule.Target != TargetURL {
			continue
		}
		if len(rule.Fields) == 0 {
			t.Errorf("%s is TargetURL but names no destination field, so it can never produce a check", name)
			continue
		}
		for _, field := range rule.Fields {
			probe := Probe{}
			switch field {
			case FieldURL:
				probe.URL = "https://ungranted.test/x"
			case FieldOrigin:
				probe.Origin = "https://ungranted.test"
			case FieldDomain:
				probe.Domain = "ungranted.test"
			case FieldOrigins:
				probe.Origins = []OriginEntry{{Origin: "https://ungranted.test"}}
			default:
				t.Errorf("%s names field %q, which the probe does not read", name, field)
				continue
			}
			checks, err := Checks(name, probe)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			found := false
			for _, check := range checks {
				if check.URL == "https://ungranted.test/x" || check.URL == "https://ungranted.test" || check.URL == "ungranted.test" {
					found = true
				}
			}
			if !found {
				t.Errorf("%s addressed by %q produced no check for the origin it reaches", name, field)
			}
		}
	}
}

func TestPersonalDataIsClassifiedThroughEverySurface(t *testing.T) {
	label := func(ref string) string {
		if ref == "e9" {
			return "Card number"
		}
		return "Search"
	}
	cases := []struct {
		name   string
		tool   string
		args   map[string]any
		refuse bool
	}{
		{name: "fill by query", tool: "brw_fill", args: map[string]any{"query": "Card number", "text": "4111"}, refuse: true},
		{name: "fill by ref", tool: "brw_fill", args: map[string]any{"ref": "e9", "text": "4111"}, refuse: true},
		{name: "fill an ordinary field by ref", tool: "brw_fill", args: map[string]any{"ref": "e2", "text": "shoes"}, refuse: false},
		{name: "batch fill step", tool: "brw_batch", args: map[string]any{"steps": []any{map[string]any{"action": "fill", "ref": "e9", "text": "4111"}}}, refuse: true},
		{name: "plan fill step", tool: "brw_plan", args: map[string]any{"steps": []any{map[string]any{"action": "fill", "ref": "e9", "text": "4111"}}}, refuse: true},
		{name: "batch type step", tool: "brw_batch", args: map[string]any{"steps": []any{map[string]any{"action": "type", "ref": "e9", "text": "4111"}}}, refuse: true},
		{name: "batch fill of an ordinary field", tool: "brw_batch", args: map[string]any{"steps": []any{map[string]any{"action": "fill", "ref": "e2", "text": "shoes"}}}, refuse: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			guard := newTestGuard(t, AdminConfig{ConfirmActions: true})
			if _, err := guard.Allow(GrantOptions{Origin: "https://shop.test", Scope: ScopeAct, Actor: "fixture-user"}); err != nil {
				t.Fatal(err)
			}
			err := guard.CheckTool(c.tool, args(t, c.args), pageAt("https://shop.test/checkout"), label)
			if c.refuse != (err != nil) {
				t.Fatalf("%s err=%v, want refusal=%v", c.tool, err, c.refuse)
			}
			if !c.refuse {
				return
			}
			if !strings.Contains(err.Error(), "personal-data") {
				t.Fatalf("the refusal does not name the class: %v", err)
			}

			if strings.Contains(err.Error(), "4111") {
				t.Fatalf("the refusal echoes the typed value: %v", err)
			}
		})
	}
}

func TestASequenceIsGatedWhereItLands(t *testing.T) {
	steps := []any{
		map[string]any{"action": "navigate_to", "url": "https://elsewhere.test/form"},
		map[string]any{"action": "click", "ref": "e1"},
	}
	guard := newTestGuard(t, AdminConfig{})
	if _, err := guard.Allow(GrantOptions{Origin: "https://start.test", Scope: ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Allow(GrantOptions{Origin: "https://elsewhere.test", Scope: ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	err := guard.CheckTool("brw_batch", args(t, map[string]any{"steps": steps}), pageAt("https://start.test/"), nil)
	var refusal *NotGrantedError
	if !asNotGranted(err, &refusal) {
		t.Fatalf("a batch acted on https://elsewhere.test holding only read there: %v", err)
	}
	if refusal.Origin != "https://elsewhere.test" || refusal.Scope != ScopeAct {
		t.Fatalf("the refusal names %s (%s); it must name the destination and the act scope", refusal.Origin, refusal.Scope)
	}
	if _, err := guard.Allow(GrantOptions{Origin: "https://elsewhere.test", Scope: ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	if err := guard.CheckTool("brw_batch", args(t, map[string]any{"steps": steps}), pageAt("https://start.test/"), nil); err != nil {
		t.Fatalf("the granted sequence was refused: %v", err)
	}
}

func TestASequenceThatRetargetsIsCheckedAgainstTheTabItMovesTo(t *testing.T) {
	steps := []any{
		map[string]any{"action": "focus_tab", "id": "tab-9"},
		map[string]any{"action": "click", "ref": "e1"},
	}
	guard := newTestGuard(t, AdminConfig{})
	if _, err := guard.Allow(GrantOptions{Origin: "https://start.test", Scope: ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	origins := func(tabID string) (string, error) {
		if tabID == "tab-9" {
			return "https://other.test/page", nil
		}
		return "https://start.test/", nil
	}
	err := guard.CheckTool("brw_batch", args(t, map[string]any{"steps": steps}), origins, nil)
	var refusal *NotGrantedError
	if !asNotGranted(err, &refusal) {
		t.Fatalf("a batch acted on the tab it focused with no grant there: %v", err)
	}
	if refusal.Origin != "https://other.test" {
		t.Fatalf("the refusal names %q, not the tab the sequence moved to", refusal.Origin)
	}

	unnamed := []any{
		map[string]any{"action": "focus_tab"},
		map[string]any{"action": "click", "ref": "e1"},
	}
	err = guard.CheckTool("brw_batch", args(t, map[string]any{"steps": unnamed}), origins, nil)
	if _, ok := err.(*CannotDecideError); !ok {
		t.Fatalf("a sequence that moves to an unnamed tab was answered with %v; it must refuse as undecidable", err)
	}
}

func TestReadIsEnforcedOnThePageItself(t *testing.T) {
	for _, tool := range []string{"brw_read", "brw_snapshot", "brw_screenshot", "brw_find", "brw_get", "brw_read_data", "brw_console", "brw_observe"} {
		t.Run(tool, func(t *testing.T) {
			guard := newTestGuard(t, AdminConfig{})
			err := guard.CheckTool(tool, args(t, map[string]any{}), pageAt("https://private-bank.test/statements"), nil)
			var refusal *NotGrantedError
			if !asNotGranted(err, &refusal) {
				t.Fatalf("%s read an un-granted page: %v", tool, err)
			}
			if refusal.Origin != "https://private-bank.test" || refusal.Scope != ScopeRead {
				t.Fatalf("%s refused with %s (%s)", tool, refusal.Origin, refusal.Scope)
			}
			if _, err := guard.Allow(GrantOptions{Origin: "https://private-bank.test", Scope: ScopeRead, Actor: "fixture-user"}); err != nil {
				t.Fatal(err)
			}
			if err := guard.CheckTool(tool, args(t, map[string]any{}), pageAt("https://private-bank.test/statements"), nil); err != nil {
				t.Fatalf("%s was refused with a read grant: %v", tool, err)
			}
		})
	}
}

func TestScriptedWaitNeedsAct(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{})
	if _, err := guard.Allow(GrantOptions{Origin: "https://shop.test", Scope: ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	if err := guard.CheckTool("brw_wait_for", args(t, map[string]any{"condition": "ready"}), pageAt("https://shop.test/x"), nil); err != nil {
		t.Fatalf("an ordinary wait needed more than read: %v", err)
	}
	err := guard.CheckTool("brw_wait_for", args(t, map[string]any{"condition": "fn:document.title.length > 0"}), pageAt("https://shop.test/x"), nil)
	var refusal *NotGrantedError
	if !asNotGranted(err, &refusal) || refusal.Scope != ScopeAct {
		t.Fatalf("a scripted wait ran on a read grant: %v", err)
	}
	err = guard.CheckTool("brw_batch", args(t, map[string]any{"steps": []any{map[string]any{"action": "wait", "condition": "fn:1"}}}), pageAt("https://shop.test/x"), nil)
	if !asNotGranted(err, &refusal) || refusal.Scope != ScopeAct {
		t.Fatalf("a scripted wait step ran on a read grant: %v", err)
	}
}

func TestAnUnclassifiedStepIsRefused(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{})
	if _, err := guard.Allow(GrantOptions{Origin: "https://shop.test", Scope: ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	err := guard.CheckTool("brw_batch", args(t, map[string]any{"steps": []any{map[string]any{"action": "teleport"}}}), pageAt("https://shop.test/x"), nil)
	if _, ok := err.(*CannotDecideError); !ok {
		t.Fatalf("an unknown step verb was answered with %v", err)
	}
}

func TestLocalTargetsAreRefused(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{})
	local := []string{
		"file:///etc/hosts",
		"FILE:///etc/hosts",
		"filesystem:https://x.test/temporary/f",
		"view-source:https://ungranted.test/",
		"chrome://settings",
		"javascript:fetch('/x')",
	}
	for _, target := range local {
		t.Run(target, func(t *testing.T) {
			err := guard.CheckTool("brw_read_url", args(t, map[string]any{"url": target}), pageAt("https://shop.test/"), nil)
			if _, ok := err.(*LocalTargetError); !ok {
				t.Fatalf("%s was answered with %v", target, err)
			}
		})
	}

	for _, target := range []string{"about:blank", "data:text/html,hi", "/relative/path"} {
		if err := guard.CheckTool("brw_read_url", args(t, map[string]any{"url": target}), pageAt("https://shop.test/"), nil); err != nil {
			t.Fatalf("%s was refused: %v", target, err)
		}
	}
}

func TestUngatedToolsCarryAReason(t *testing.T) {
	for name, reason := range UngatedTools {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s is ungated with no reason", name)
		}
		if _, gated := ToolRules[name]; gated {
			t.Errorf("%s is in both ToolRules and UngatedTools", name)
		}
	}
}

func TestEveryRunnerConsultsTheStepGate(t *testing.T) {
	for _, runner := range stepRunners {
		if !functionCalls(t, runner.file, runner.function, "GateSequenceStep") {
			t.Errorf("%s in %s never calls GateSequenceStep, so its steps after the first navigation are gated against the origin the sequence left",
				runner.function, runner.file)
		}
	}
}

func functionCalls(t *testing.T, path, function, name string) bool {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == function {
			found = fn
			break
		}
	}
	if found == nil {
		t.Fatalf("%s declares no function %s", path, function)
	}
	calls := false
	ast.Inspect(found, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			calls = calls || fn.Name == name
		case *ast.SelectorExpr:
			calls = calls || fn.Sel.Name == name
		}
		return true
	})
	return calls
}

func TestAStepAfterAnActingStepIsGatedWhereItLanded(t *testing.T) {
	steps := map[string]any{"steps": []any{
		map[string]any{"action": "click", "ref": "e1"},
		map[string]any{"action": "snapshot"},
		map[string]any{"action": "read"},
	}}
	guard := newTestGuard(t, AdminConfig{})
	if _, err := guard.Allow(GrantOptions{Origin: "https://start.test", Scope: ScopeAct, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	payload := args(t, steps)

	if err := guard.CheckTool("brw_batch", payload, pageAt("https://start.test/"), nil); err != nil {
		t.Fatalf("the preflight refused a batch that starts on a granted origin: %v", err)
	}

	gate := guard.NewStepGate("brw_batch", payload)
	if err := gate.Check(0, StepProbe{Action: "click", Ref: "e1"}, pageAt("https://start.test/"), nil); err != nil {
		t.Fatalf("the click on the granted origin was refused: %v", err)
	}

	for index, step := range []StepProbe{{Action: "snapshot"}, {Action: "read"}} {
		err := gate.Check(index+1, step, pageAt("https://elsewhere.test/inbox"), nil)
		var refusal *NotGrantedError
		if !asNotGranted(err, &refusal) {
			t.Fatalf("%s read https://elsewhere.test after the click: %v", step.Action, err)
		}
		if refusal.Origin != "https://elsewhere.test" || refusal.Scope != ScopeRead {
			t.Fatalf("%s refused with %s (%s); it must name where the step landed", step.Action, refusal.Origin, refusal.Scope)
		}
	}

	if _, err := guard.Allow(GrantOptions{Origin: "https://elsewhere.test", Scope: ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	if err := gate.Check(1, StepProbe{Action: "snapshot"}, pageAt("https://elsewhere.test/inbox"), nil); err != nil {
		t.Fatalf("the granted read was still refused: %v", err)
	}
}

func TestAnActingStepAfterAnotherIsGatedWhereItLanded(t *testing.T) {
	payload := args(t, map[string]any{"steps": []any{
		map[string]any{"action": "click", "ref": "e1"},
		map[string]any{"action": "fill", "ref": "e2", "text": "hello"},
	}})
	guard := newTestGuard(t, AdminConfig{})
	for _, grant := range []GrantOptions{
		{Origin: "https://start.test", Scope: ScopeAct, Actor: "fixture-user"},
		{Origin: "https://elsewhere.test", Scope: ScopeRead, Actor: "fixture-user"},
	} {
		if _, err := guard.Allow(grant); err != nil {
			t.Fatal(err)
		}
	}
	gate := guard.NewStepGate("brw_batch", payload)
	err := gate.Check(1, StepProbe{Action: "fill", Ref: "e2", Text: "hello"}, pageAt("https://elsewhere.test/form"), nil)
	var refusal *NotGrantedError
	if !asNotGranted(err, &refusal) || refusal.Origin != "https://elsewhere.test" || refusal.Scope != ScopeAct {
		t.Fatalf("a fill ran on https://elsewhere.test with only read there: %v", err)
	}
}

func TestADeferredActionIsClassifiedAtTheStep(t *testing.T) {
	label := func(ref string) string {
		if ref == "e9" {
			return "Card number"
		}
		return "Search"
	}
	payload := args(t, map[string]any{"steps": []any{
		map[string]any{"action": "click", "ref": "e1"},
		map[string]any{"action": "fill", "ref": "e9", "text": "4111"},
	}})
	guard := newTestGuard(t, AdminConfig{ConfirmActions: true})
	for _, origin := range []string{"https://start.test", "https://checkout.test"} {
		if _, err := guard.Allow(GrantOptions{Origin: origin, Scope: ScopeAct, Actor: "fixture-user"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := guard.CheckTool("brw_batch", payload, pageAt("https://start.test/"), label); err != nil {
		t.Fatalf("the preflight confirmed a step whose origin it cannot know: %v", err)
	}
	gate := guard.NewStepGate("brw_batch", payload)
	err := gate.Check(1, StepProbe{Action: "fill", Ref: "e9", Text: "4111"}, pageAt("https://checkout.test/pay"), label)
	if err == nil || !strings.Contains(err.Error(), "personal-data") {
		t.Fatalf("the deferred fill was not classified at the step: %v", err)
	}
	if strings.Contains(err.Error(), "4111") {
		t.Fatalf("the refusal echoes the typed value: %v", err)
	}
}

func TestCookiesAreCheckedAgainstTheTabTheyAreReadFrom(t *testing.T) {
	guard := newTestGuard(t, AdminConfig{})
	if _, err := guard.Allow(GrantOptions{Origin: "https://notbank.test", Scope: ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	call := args(t, map[string]any{"action": "list", "domain": "notbank.test"})
	err := guard.CheckTool("brw_cookies", call, pageAt("https://bank.test/accounts"), nil)
	var refusal *NotGrantedError
	if !asNotGranted(err, &refusal) {
		t.Fatalf("cookies were listed out of https://bank.test on a grant for notbank.test: %v", err)
	}
	if refusal.Origin != "https://bank.test" || refusal.Scope != ScopeRead {
		t.Fatalf("the refusal names %s (%s), not the tab the cookies come out of", refusal.Origin, refusal.Scope)
	}
	if _, err := guard.Allow(GrantOptions{Origin: "https://bank.test", Scope: ScopeRead, Actor: "fixture-user"}); err != nil {
		t.Fatal(err)
	}
	if err := guard.CheckTool("brw_cookies", call, pageAt("https://bank.test/accounts"), nil); err != nil {
		t.Fatalf("the granted listing was still refused: %v", err)
	}

	write := args(t, map[string]any{"action": "set", "domain": "notbank.test", "name": "a", "value": "b"})
	err = guard.CheckTool("brw_cookies", write, pageAt("https://bank.test/accounts"), nil)
	if !asNotGranted(err, &refusal) || refusal.Origin != "https://notbank.test" || refusal.Scope != ScopeAct {
		t.Fatalf("a domain-addressed cookie write was answered with %v", err)
	}
}

func TestEveryCookieActionIsClassified(t *testing.T) {
	actions, err := stepscan.SwitchCases("../browser/manager_cookies.go", "Validate", "Action")
	if err != nil {
		t.Fatal(err)
	}
	named := 0
	for _, action := range actions {
		if strings.TrimSpace(action) == "" {
			continue
		}
		named++
		if _, classified := cookieDomainAddressesTheCookie[action]; !classified {
			t.Errorf("brw_cookies accepts %q but cookieDomainAddressesTheCookie does not say what its domain argument means, so the gate cannot tell whether it reaches the tab", action)
		}
	}
	if named == 0 {
		t.Fatal("no cookie verbs were read out of CookieParams.Validate; the source this table is checked against has moved")
	}
	for action := range cookieDomainAddressesTheCookie {
		if !slices.Contains(actions, action) {
			t.Errorf("cookieDomainAddressesTheCookie classifies %q, which brw_cookies does not accept", action)
		}
	}
}
