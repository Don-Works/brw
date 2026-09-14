package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// observeController answers every action with one fully-populated observation,
// so a trimmed response is visibly smaller rather than accidentally empty.
type observeController struct {
	fakeController
	findElements []snapshot.Element
	// liveElements is what the page holds NOW, when a test wants the cached
	// search and the live one to disagree. Unset, the two answer alike.
	liveElements []snapshot.Element
	liveSearches int
	acted        []string
}

func observeFixtureResult() browser.ActionResult {
	changed := true
	return browser.ActionResult{
		OK:           true,
		Message:      "clicked e4",
		TabID:        "tab1",
		Version:      7,
		URL:          "https://fixture.test/cart",
		Title:        "Cart",
		Focus:        "e9",
		ChangedState: &changed,
		Targets:      []browser.Tab{{ID: "tab1", URL: "https://fixture.test/cart", Title: "Cart"}},
		Changed:      []string{"e9 button \"Checkout\""},
		Elements:     []snapshot.Element{{Ref: "e9", Role: "button", Name: "Checkout", Visible: true, InViewport: true, Source: []string{"dom"}}},
		DurationMS:   42,
	}
}

func (c *observeController) Click(_ context.Context, ref string) (browser.ActionResult, error) {
	c.acted = append(c.acted, "click:"+ref)
	return observeFixtureResult(), nil
}

func (c *observeController) Fill(_ context.Context, opts snapshot.FillOptions) (browser.ActionResult, error) {
	c.acted = append(c.acted, "fill:"+opts.Ref+"="+opts.EffectiveText())
	return observeFixtureResult(), nil
}

func (c *observeController) Type(_ context.Context, ref, text string) (browser.ActionResult, error) {
	c.acted = append(c.acted, "type:"+ref+"="+text)
	return observeFixtureResult(), nil
}

func (c *observeController) Select(_ context.Context, ref, value string) (browser.ActionResult, error) {
	c.acted = append(c.acted, "select:"+ref+"="+value)
	return observeFixtureResult(), nil
}

func (c *observeController) Hover(_ context.Context, ref string) (browser.ActionResult, error) {
	c.acted = append(c.acted, "hover:"+ref)
	return observeFixtureResult(), nil
}

func (c *observeController) Press(_ context.Context, key string) (browser.ActionResult, error) {
	c.acted = append(c.acted, "press:"+key)
	return observeFixtureResult(), nil
}

// Find answers with the one fixture element unless a test sets its own list, so
// a locate-and-act resolves without every caller having to arrange a match.
func (c *observeController) Find(_ context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	c.acted = append(c.acted, "find:"+opts.Query+"/"+opts.Role)
	elements := c.findElements
	if elements == nil {
		elements = observeFixtureResult().Elements
	}
	result := snapshot.FindResult{URL: "https://fixture.test/cart", Title: "Cart", Elements: elements}
	// Applying the limit is what lets a test prove the limit was dropped: a fake
	// that ignored it would answer the same whether or not the caller's limit
	// reached the search.
	if opts.Limit > 0 && opts.Limit < len(elements) {
		result.Elements = elements[:opts.Limit]
		result.Metadata = map[string]any{"truncated": true}
	}
	return result, nil
}

// FindLive is the search a locate-and-act resolves through. It answers from
// liveElements when a test set them, so a call that took the cached path
// resolves a different element list.
func (c *observeController) FindLive(ctx context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	c.liveSearches++
	if c.liveElements != nil {
		cached := c.findElements
		c.findElements = c.liveElements
		defer func() { c.findElements = cached }()
	}
	return c.Find(ctx, opts)
}

func (c *observeController) ExecuteBatch(context.Context, []browser.BatchStep) (browser.BatchResult, error) {
	return browser.BatchResult{
		OK: true, TabID: "tab1", URL: "https://fixture.test/cart", Title: "Cart", Focus: "e9", Version: 7,
		Changed:        []string{"e9 button \"Checkout\""},
		Steps:          []browser.BatchStepResult{{Index: 0, Action: "click", OK: true}},
		StepsCompleted: 1,
	}, nil
}

// ExecutePlan answers each step in the shape that verb's runner produces, so a
// trim keyed on the verb is exercised rather than assumed.
func (c *observeController) ExecutePlan(_ context.Context, steps []browser.PlanStep) (browser.PlanResult, error) {
	result := browser.PlanResult{OK: true, StepsCompleted: len(steps)}
	for i, step := range steps {
		action := step.Action
		if action == "" {
			action = "click"
		}
		stepResult := browser.PlanStepResult{Index: i, Action: action, OK: true, Message: action + " ok"}
		switch action {
		case "snapshot":
			snap := observeFixtureSnapshot()
			stepResult.Snapshot = &snap
			stepResult.Result = snap
		case "find_act":
			stepResult.Result = browser.FindActResult{
				Matched: snapshot.Element{Ref: "e9", Role: "button", Name: "Checkout"},
				Action:  "click",
				Result:  observeFixtureResult(),
			}
		case "navigate_to":
			// The primitive a navigate_to step reuses: a message written from the
			// REQUESTED url, and an observed url that is where the browser landed.
			result := observeFixtureResult()
			result.Message = "navigated to " + step.URL
			result.URL = observeRedirectedURL
			stepResult.Result = result
		default:
			stepResult.Result = observeFixtureResult()
		}
		result.Steps = append(result.Steps, stepResult)
	}
	return result, nil
}

// observeFixtureSnapshot is what a plan snapshot step hands back: the payload
// the step exists to fetch.
func observeFixtureSnapshot() snapshot.PageSnapshot {
	return snapshot.PageSnapshot{
		URL: "https://fixture.test/cart", Title: "Cart",
		Elements: []snapshot.Element{{Ref: "e9", Role: "button", Name: "Checkout", Visible: true}},
	}
}

func observeCallTool(t *testing.T, c browser.Controller, name, args string) map[string]any {
	t.Helper()
	raw, rpcErr := New(c).callTool(context.Background(), name, json.RawMessage(args))
	if rpcErr != nil {
		t.Fatalf("%s %s: %v", name, args, rpcErr)
	}
	out, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s returned %T", name, raw)
	}
	return out
}

func toolText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, ok := result["content"].([]toolContent)
	if !ok || len(content) == 0 {
		t.Fatalf("result has no text content: %v", result)
	}
	return content[0].Text
}

// The exact JSON an action tool returns when observe is absent. Written out by
// hand rather than derived from the code under test: a default that quietly
// starts dropping a field would otherwise agree with whatever it now produces.
const observeDefaultActionJSON = `{"ok":true,"message":"clicked e4","tab_id":"tab1","version":7,` +
	`"url":"https://fixture.test/cart","title":"Cart","focus":"e9","changed_state":true,` +
	`"targets":[{"id":"tab1","url":"https://fixture.test/cart","title":"Cart","type":""}],` +
	`"changed":["e9 button \"Checkout\""],` +
	`"elements":[{"ref":"e9","role":"button","name":"Checkout","tag":"","visible":true,"in_viewport":true,"disabled":false,"source":["dom"]}],` +
	`"duration_ms":42}`

func TestOmittingObserveIsByteIdenticalToTheOldResponse(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool string
		args string
	}{
		{name: "click", tool: "brw_click", args: `{"ref":"e4"}`},
		{name: "type", tool: "brw_type", args: `{"ref":"e4","text":"hi"}`},
		{name: "fill", tool: "brw_fill", args: `{"ref":"e4","text":"hi"}`},
		{name: "select", tool: "brw_select", args: `{"ref":"e4","value":"l"}`},
		{name: "hover", tool: "brw_hover", args: `{"ref":"e4"}`},
		{name: "press", tool: "brw_press", args: `{"key":"Enter"}`},
		// An explicit observe:"full" must produce the same bytes as omitting it.
		{name: "explicit full", tool: "brw_click", args: `{"ref":"e4","observe":"full"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := toolText(t, observeCallTool(t, &observeController{}, tc.tool, tc.args))
			if got != observeDefaultActionJSON {
				t.Fatalf("%s response changed:\n got %s\nwant %s", tc.tool, got, observeDefaultActionJSON)
			}
		})
	}
}

// brw_batch is byte-identical with observe absent as well. brw_plan is the one
// deliberate exception on the branch: with no observe its intermediate steps
// report minimal, which its schema and SKILL.md both state, and which is the
// whole point of the per-step split.
func TestOmittingObserveIsByteIdenticalOnABatch(t *testing.T) {
	const want = `{"ok":true,"steps":[{"index":0,"action":"click","ok":true}],"tab_id":"tab1",` +
		`"url":"https://fixture.test/cart","title":"Cart","focus":"e9",` +
		`"changed":["e9 button \"Checkout\""],"version":7,"steps_completed":1}`
	for _, args := range []string{
		`{"steps":[{"action":"click","ref":"e4"}]}`,
		`{"steps":[{"action":"click","ref":"e4"}],"observe":"full"}`,
		// minimal has nothing to drop on a batch, so it is the same bytes again.
		`{"steps":[{"action":"click","ref":"e4"}],"observe":"minimal"}`,
	} {
		if got := toolText(t, observeCallTool(t, &observeController{}, "brw_batch", args)); got != want {
			t.Fatalf("brw_batch %s response changed:\n got %s\nwant %s", args, got, want)
		}
	}
}

func TestObserveLevelsShrinkTheActionResponse(t *testing.T) {
	tests := []struct {
		name    string
		args    string
		want    string
		smaller bool
	}{
		{
			name: "minimal drops the element list",
			args: `{"ref":"e4","observe":"minimal"}`,
			want: `{"ok":true,"message":"clicked e4","tab_id":"tab1","version":7,` +
				`"url":"https://fixture.test/cart","title":"Cart","focus":"e9","changed_state":true,` +
				`"changed":["e9 button \"Checkout\""],"duration_ms":42}`,
			smaller: true,
		},
		{
			name:    "none is the outcome alone",
			args:    `{"ref":"e4","observe":"none"}`,
			want:    `{"ok":true,"message":"clicked e4","tab_id":"tab1","changed_state":true,"duration_ms":42}`,
			smaller: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolText(t, observeCallTool(t, &observeController{}, "brw_click", tt.args))
			if got != tt.want {
				t.Fatalf("response:\n got %s\nwant %s", got, tt.want)
			}
			if tt.smaller && len(got) >= len(observeDefaultActionJSON) {
				t.Fatalf("observe level did not shrink the response: %d >= %d bytes", len(got), len(observeDefaultActionJSON))
			}
			// The reason none is not free: it still answers "did it work".
			var decoded map[string]any
			if err := json.Unmarshal([]byte(got), &decoded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if decoded["ok"] != true || decoded["changed_state"] != true {
				t.Fatalf("a trimmed response no longer reports the outcome: %s", got)
			}
		})
	}
}

// A value brw does not understand is refused whatever JSON type it arrives as.
// Decoding straight into a string field took every non-string for "absent" and
// widened it back to full, so a caller who asked for fewer tokens got all of
// them with nothing said.
func TestUnknownObserveLevelIsRefused(t *testing.T) {
	tests := []struct {
		name string
		args string
	}{
		{name: "unknown string", args: `{"ref":"e4","observe":"summary"}`},
		{name: "number", args: `{"ref":"e4","observe":123}`},
		{name: "boolean", args: `{"ref":"e4","observe":true}`},
		{name: "array", args: `{"ref":"e4","observe":["none"]}`},
		{name: "object", args: `{"ref":"e4","observe":{"level":"none"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, rpcErr := New(&observeController{}).callTool(context.Background(), "brw_click", json.RawMessage(tt.args))
			if rpcErr == nil {
				t.Fatalf("brw_click accepted %s as an observe level", tt.args)
			}
			if !strings.Contains(rpcErr.Message, "unknown observe level") {
				t.Fatalf("error = %q, want it to name the unknown level", rpcErr.Message)
			}
		})
	}
	// An absent parameter still means "the caller did not ask", including the
	// explicit JSON null a client may send for an unset field.
	for _, args := range []string{`{"ref":"e4"}`, `{"ref":"e4","observe":null}`} {
		got := toolText(t, observeCallTool(t, &observeController{}, "brw_click", args))
		if got != observeDefaultActionJSON {
			t.Fatalf("%s did not report at the default level:\n got %s\nwant %s", args, got, observeDefaultActionJSON)
		}
	}
}

// brw_batch returns one observation at the end, so observe controls that one.
func TestBatchObserveTrimsOnlyTheClosingObservation(t *testing.T) {
	full := toolText(t, observeCallTool(t, &observeController{}, "brw_batch", `{"steps":[{"action":"click","ref":"e4"}]}`))
	none := toolText(t, observeCallTool(t, &observeController{}, "brw_batch", `{"steps":[{"action":"click","ref":"e4"}],"observe":"none"}`))
	if !strings.Contains(full, `"url":"https://fixture.test/cart"`) {
		t.Fatalf("default batch response lost its observation: %s", full)
	}
	if strings.Contains(none, `"url"`) || strings.Contains(none, `"changed"`) {
		t.Fatalf("observe=none kept the closing observation: %s", none)
	}
	for _, response := range []string{full, none} {
		if !strings.Contains(response, `"steps":[{"index":0,"action":"click","ok":true}]`) {
			t.Fatalf("batch lost its per-step record: %s", response)
		}
	}
}

// brw_plan's intermediate steps are where the unread observations are. By
// default they report minimal and the last one reports full.
func TestPlanDefaultsIntermediateStepsToMinimal(t *testing.T) {
	steps := `{"steps":[{"action":"click","ref":"e1"},{"action":"click","ref":"e2"},{"action":"click","ref":"e3"}]}`
	response := toolText(t, observeCallTool(t, &observeController{}, "brw_plan", steps))

	var decoded struct {
		Steps []struct {
			Result map[string]any `json:"result"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(response), &decoded); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	if len(decoded.Steps) != 3 {
		t.Fatalf("plan returned %d steps", len(decoded.Steps))
	}
	for i, step := range decoded.Steps[:2] {
		if _, present := step.Result["elements"]; present {
			t.Fatalf("intermediate step %d kept its element list", i)
		}
		if step.Result["url"] != "https://fixture.test/cart" {
			t.Fatalf("intermediate step %d lost url, which minimal keeps: %v", i, step.Result)
		}
	}
	if _, present := decoded.Steps[2].Result["elements"]; !present {
		t.Fatalf("the last plan step lost its element list: %v", decoded.Steps[2].Result)
	}

	// And an explicit level reaches every step, last one included.
	explicit := toolText(t, observeCallTool(t, &observeController{}, "brw_plan",
		`{"steps":[{"action":"click","ref":"e1"},{"action":"click","ref":"e2"}],"observe":"none"}`))
	if strings.Contains(explicit, `"url"`) || strings.Contains(explicit, `"elements"`) {
		t.Fatalf("observe=none left observation fields on a plan step: %s", explicit)
	}
	if !strings.Contains(explicit, `"ok":true`) {
		t.Fatalf("observe=none lost the per-step outcome: %s", explicit)
	}
}

// The rest of the action surface answers with the same fully-populated
// observation, so a tool that ignores observe is visibly bigger than one that
// honours it rather than accidentally empty.
func (c *observeController) ClickText(_ context.Context, opts snapshot.ClickTextOptions) (browser.ActionResult, error) {
	c.acted = append(c.acted, "click_text:"+opts.Text)
	return observeFixtureResult(), nil
}

func (c *observeController) ClickButton(_ context.Context, opts browser.ClickButtonOptions) (browser.ActionResult, error) {
	c.acted = append(c.acted, "click_button:"+opts.Ref)
	return observeFixtureResult(), nil
}

func (c *observeController) Scroll(_ context.Context, direction string) (browser.ActionResult, error) {
	c.acted = append(c.acted, "scroll:"+direction)
	return observeFixtureResult(), nil
}

func (c *observeController) Navigate(_ context.Context, direction string) (browser.ActionResult, error) {
	c.acted = append(c.acted, "navigate:"+direction)
	result := observeFixtureResult()
	result.Message = "navigated " + direction
	result.URL = observeRedirectedURL
	return result, nil
}

// NavigateTo answers the way the real one does: a message written from the
// REQUESTED url, and an observed url that is where the browser actually landed.
func (c *observeController) NavigateTo(_ context.Context, url string) (browser.ActionResult, error) {
	c.acted = append(c.acted, "navigate_to:"+url)
	result := observeFixtureResult()
	result.Message = "navigated to " + url
	result.URL = observeRedirectedURL
	return result, nil
}

func (c *observeController) Drag(_ context.Context, opts browser.DragOptions) (browser.ActionResult, error) {
	c.acted = append(c.acted, "drag:"+opts.From.Ref)
	return observeFixtureResult(), nil
}

func (c *observeController) MouseDown(_ context.Context, opts browser.MouseButtonOptions) (browser.ActionResult, error) {
	c.acted = append(c.acted, "mouse_down:"+opts.Ref)
	return observeFixtureResult(), nil
}

func (c *observeController) MouseUp(_ context.Context, opts browser.MouseButtonOptions) (browser.ActionResult, error) {
	c.acted = append(c.acted, "mouse_up:"+opts.Ref)
	return observeFixtureResult(), nil
}

func (c *observeController) Focus(_ context.Context, ref string) (browser.ActionResult, error) {
	c.acted = append(c.acted, "focus:"+ref)
	return observeFixtureResult(), nil
}

// observeRedirectedURL is where the fixture's navigation actually lands, which
// is deliberately not the url that was asked for.
const observeRedirectedURL = "https://fixture.test/login?next=%2Fcart"

// observeToolCall is one valid call for a tool that advertises observe, plus
// what that tool's default response contains: brw_batch is the one whose
// closing observation carries no element list at all, which is why its schema
// says minimal cannot trim there.
type observeToolCall struct {
	args            string
	reportsElements bool
}

// observeToolCalls must cover observeToolNames() exactly, so a tool cannot join
// that list without a call that proves it honours the parameter.
func observeToolCalls() map[string]observeToolCall {
	return map[string]observeToolCall{
		"brw_click":       {args: `{"ref":"e4"}`, reportsElements: true},
		"brw_click_text":  {args: `{"text":"Checkout"}`, reportsElements: true},
		"brw_type":        {args: `{"ref":"e4","text":"hi"}`, reportsElements: true},
		"brw_fill":        {args: `{"ref":"e4","text":"hi"}`, reportsElements: true},
		"brw_select":      {args: `{"ref":"e4","value":"l"}`, reportsElements: true},
		"brw_press":       {args: `{"key":"Enter"}`, reportsElements: true},
		"brw_scroll":      {args: `{"direction":"down"}`, reportsElements: true},
		"brw_hover":       {args: `{"ref":"e4"}`, reportsElements: true},
		"brw_navigate":    {args: `{"direction":"back"}`, reportsElements: true},
		"brw_navigate_to": {args: `{"url":"https://fixture.test/cart"}`, reportsElements: true},
		"brw_drag":        {args: `{"from":{"ref":"e4"},"to":{"ref":"e9"}}`, reportsElements: true},
		"brw_mouse_down":  {args: `{"ref":"e4"}`, reportsElements: true},
		"brw_mouse_up":    {args: `{"ref":"e4"}`, reportsElements: true},
		"brw_focus":       {args: `{"ref":"e4"}`, reportsElements: true},
		"brw_find":        {args: `{"query":"Checkout","role":"button","action":"click"}`, reportsElements: true},
		"brw_batch":       {args: `{"steps":[{"action":"click","ref":"e4"}]}`},
		"brw_plan":        {args: `{"steps":[{"action":"click","ref":"e4"}]}`, reportsElements: true},
	}
}

// withObserve splices a level into a call's arguments.
func withObserve(t *testing.T, args, level string) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(args), &decoded); err != nil {
		t.Fatalf("decode %s: %v", args, err)
	}
	decoded["observe"] = level
	out, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(out)
}

// Advertising observe and reading it are checked against each other in both
// directions, because each direction fails silently on its own: a handler that
// stops reading the parameter keeps advertising it (brw_find shipped that way),
// and a handler that reads one nobody advertises is a parameter no agent passes.
func TestObserveIsAdvertisedByExactlyTheToolsThatReadIt(t *testing.T) {
	advertised := map[string]bool{}
	for _, tl := range tools() {
		name, _ := tl["name"].(string)
		schema, _ := tl["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props["observe"]; ok {
			advertised[name] = true
		}
	}
	reads := map[string]bool{}
	for _, name := range observeToolNames() {
		reads[name] = true
		schema, ok := toolProperties(t, name)["observe"].(map[string]any)
		if !ok {
			t.Errorf("%s reads observe but does not advertise it", name)
			continue
		}
		values, _ := schema["enum"].([]string)
		if len(values) != 3 {
			t.Errorf("%s advertises observe with enum %v, want the three levels", name, values)
		}
	}
	for name := range advertised {
		if !reads[name] {
			t.Errorf("%s advertises observe but is not in observeToolNames(), so nothing checks that it honours it", name)
		}
	}
}

// The catalogue test above keys both sides on a hand-written list, so it cannot
// see a handler that quietly stops applying the level. This calls every tool on
// that list and reads the response: observe:"none" must leave no element list
// anywhere in it, and must still say whether the action worked.
func TestEveryAdvertisedObserveToolHonoursNone(t *testing.T) {
	calls := observeToolCalls()
	for _, name := range observeToolNames() {
		if _, ok := calls[name]; !ok {
			t.Errorf("%s advertises observe but no call here proves it honours the level", name)
		}
	}
	for name := range calls {
		if !slices.Contains(observeToolNames(), name) {
			t.Errorf("%s has a call here but does not read observe", name)
		}
	}
	if t.Failed() {
		return
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			full := toolText(t, observeCallTool(t, &observeController{}, name, call.args))
			if call.reportsElements != strings.Contains(full, `"elements"`) {
				t.Fatalf("%s reports an element list = %v by default, want %v; the table and the tool disagree: %s",
					name, !call.reportsElements, call.reportsElements, full)
			}
			none := toolText(t, observeCallTool(t, &observeController{}, name, withObserve(t, call.args, "none")))
			if strings.Contains(none, `"elements"`) {
				t.Fatalf("%s ignored observe:\"none\" and returned the element list: %s", name, none)
			}
			if !strings.Contains(none, `"ok":true`) {
				t.Fatalf("%s stopped reporting its outcome at observe:\"none\": %s", name, none)
			}
			if len(none) >= len(full) {
				t.Fatalf("%s did not shrink: %d bytes at none against %d at full", name, len(none), len(full))
			}
		})
	}
}

// brw_batch is the one tool where minimal has nothing to drop: its single
// closing observation carries no element list. Its schema says so, and this
// pins the schema to the behaviour.
func TestBatchSaysMinimalIsTheSameAsFull(t *testing.T) {
	schema, ok := toolProperties(t, "brw_batch")["observe"].(map[string]any)
	if !ok {
		t.Fatal("brw_batch does not advertise observe")
	}
	description, _ := schema["description"].(string)
	if !strings.Contains(description, "minimal is the SAME as full") {
		t.Fatalf("brw_batch's observe description does not say minimal cannot trim here: %q", description)
	}
	full := toolText(t, observeCallTool(t, &observeController{}, "brw_batch", `{"steps":[{"action":"click","ref":"e4"}]}`))
	minimal := toolText(t, observeCallTool(t, &observeController{}, "brw_batch",
		`{"steps":[{"action":"click","ref":"e4"}],"observe":"minimal"}`))
	if full != minimal {
		t.Fatalf("brw_batch's minimal now differs from full, so the schema is wrong:\n full %s\nminimal %s", full, minimal)
	}
}

// A navigation's outcome IS the destination, and the message names the url that
// was REQUESTED. Dropping the observed url would leave the caller holding a
// claim brw never verified: after a redirect or a login wall the message says
// it arrived somewhere it did not.
func TestNavigationToolsKeepTheCommittedURLAtEveryLevel(t *testing.T) {
	for _, name := range navigationToolNames() {
		if !slices.Contains(observeToolNames(), name) {
			t.Fatalf("%s is treated as a navigation tool but does not read observe", name)
		}
		args := observeToolCalls()[name].args
		for _, level := range []string{"full", "minimal", "none"} {
			t.Run(name+"/"+level, func(t *testing.T) {
				got := toolText(t, observeCallTool(t, &observeController{}, name, withObserve(t, args, level)))
				var decoded map[string]any
				if err := json.Unmarshal([]byte(got), &decoded); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if decoded["url"] != observeRedirectedURL {
					t.Fatalf("%s at observe:%q reported url %v while claiming %q", name, level, decoded["url"], decoded["message"])
				}
			})
		}
	}
	// And every tool whose name says it navigates has to be on that list, or
	// the exception is one tool wide and the next one repeats the defect.
	for _, tl := range tools() {
		name, _ := tl["name"].(string)
		if !strings.HasPrefix(name, "brw_navigate") {
			continue
		}
		if !slices.Contains(navigationToolNames(), name) {
			t.Errorf("%s navigates but is not in navigationToolNames(), so observe may drop the url it did not verify", name)
		}
	}
}

// navigationPlanSteps is one brw_plan step per navigation verb. A navigation
// verb the catalogue advertises as a plan step and this map does not name fails
// the test below rather than going unchecked, which is what the two-tool fix
// missed: brw_navigate_to was corrected while the plan step running the same
// primitive was not.
func navigationPlanSteps() map[string]map[string]any {
	return map[string]map[string]any{
		"navigate_to": {"action": "navigate_to", "url": "https://fixture.test/cart"},
	}
}

// The url a navigation reports is its outcome, and a brw_plan step runs the same
// primitive as the standalone tool. Enumerated over the advertised step enum, so
// the exception cannot be one surface wide: this is the defect the tool-name fix
// left behind on brw_plan.
func TestNavigationPlanStepsKeepTheCommittedURLAtEveryLevel(t *testing.T) {
	steps := navigationPlanSteps()
	checked := 0
	for _, verb := range advertisedPlanStepVerbs(t) {
		if !browser.IsNavigationAction(verb) {
			continue
		}
		step, ok := steps[verb]
		if !ok {
			t.Errorf("brw_plan advertises navigation step verb %q, which has no call here, so nothing checks whether it keeps the url", verb)
			continue
		}
		if !browser.PlanStepVerbKeepsTheCommittedURL(verb) {
			t.Errorf("brw_plan step verb %q navigates but observe trims it as a plain observation", verb)
			continue
		}
		checked++
		for _, level := range []string{"full", "minimal", "none"} {
			t.Run(verb+"/"+level, func(t *testing.T) {
				body, err := json.Marshal(map[string]any{"steps": []any{step}, "observe": level})
				if err != nil {
					t.Fatalf("encode: %v", err)
				}
				var decoded struct {
					Steps []struct {
						Action string `json:"action"`
						Result struct {
							URL     string `json:"url"`
							Message string `json:"message"`
						} `json:"result"`
					} `json:"steps"`
				}
				got := toolText(t, observeCallTool(t, &observeController{}, "brw_plan", string(body)))
				if err := json.Unmarshal([]byte(got), &decoded); err != nil {
					t.Fatalf("decode plan: %v", err)
				}
				if len(decoded.Steps) != 1 {
					t.Fatalf("plan returned %d steps: %s", len(decoded.Steps), got)
				}
				if decoded.Steps[0].Result.URL != observeRedirectedURL {
					t.Fatalf("brw_plan %s step at observe:%q reported url %q while claiming %q",
						verb, level, decoded.Steps[0].Result.URL, decoded.Steps[0].Result.Message)
				}
			})
		}
	}
	if checked == 0 {
		t.Fatal("no advertised plan step verb is a navigation, so this test checked nothing")
	}
	// A verb the plan advertises whose name says it navigates has to BE a
	// navigation action, or the classification is a list that the next sibling
	// is simply left off.
	for _, verb := range advertisedPlanStepVerbs(t) {
		if strings.HasPrefix(verb, "navigate") && !browser.IsNavigationAction(verb) {
			t.Errorf("brw_plan step verb %q navigates but browser.NavigationActions() does not name it, so observe drops the url it did not verify", verb)
		}
	}
}

// snapshot:true and observe minimal/none contradict each other: one asks for the
// page, the other deletes it on the way out. Picking one silently leaves the
// caller unable to see which. The pairing is read off the catalogue so a new
// tool carrying both parameters is covered the day it is added.
func TestSnapshotTrueIsRefusedWithATrimmingLevel(t *testing.T) {
	calls := observeToolCalls()
	checked := 0
	for _, tl := range tools() {
		name, _ := tl["name"].(string)
		schema, _ := tl["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props["observe"]; !ok {
			continue
		}
		if _, ok := props["snapshot"]; !ok {
			continue
		}
		call, ok := calls[name]
		if !ok {
			t.Errorf("%s advertises snapshot and observe but has no call here", name)
			continue
		}
		args := call.args
		checked++
		for _, level := range []string{"minimal", "none"} {
			t.Run(name+"/"+level, func(t *testing.T) {
				withSnapshot := withObserve(t, args, level)
				var decoded map[string]any
				if err := json.Unmarshal([]byte(withSnapshot), &decoded); err != nil {
					t.Fatalf("decode: %v", err)
				}
				decoded["snapshot"] = true
				body, err := json.Marshal(decoded)
				if err != nil {
					t.Fatalf("encode: %v", err)
				}
				_, rpcErr := New(&observeController{}).callTool(context.Background(), name, json.RawMessage(body))
				if rpcErr == nil {
					t.Fatalf("%s accepted snapshot:true with observe:%q and silently dropped one of them", name, level)
				}
				if !strings.Contains(rpcErr.Message, "snapshot:true cannot be combined") {
					t.Fatalf("%s refused with %q, which does not name the conflict", name, rpcErr.Message)
				}
			})
		}
		// The combination that does not conflict still works.
		t.Run(name+"/full", func(t *testing.T) {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(args), &decoded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			decoded["snapshot"] = true
			decoded["observe"] = "full"
			body, _ := json.Marshal(decoded)
			if _, rpcErr := New(&observeController{}).callTool(context.Background(), name, json.RawMessage(body)); rpcErr != nil {
				t.Fatalf("%s refused snapshot:true with observe:\"full\": %v", name, rpcErr)
			}
		})
	}
	if checked == 0 {
		t.Fatal("no tool advertises both snapshot and observe, so this test checked nothing")
	}
}

// brw_find's read-only path returns the match list, which is the whole answer:
// there is nothing for minimal or none to trim. It used to accept them and
// ignore them, which is the same silent no-op shape as a dropped option.
func TestReadOnlyFindRefusesALevelItCannotHonour(t *testing.T) {
	tests := []struct {
		name    string
		args    string
		wantErr string
	}{
		{name: "minimal", args: `{"query":"Check","observe":"minimal"}`, wantErr: "brw_find without action"},
		{name: "none", args: `{"query":"Check","observe":"none"}`, wantErr: "brw_find without action"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, rpcErr := New(&observeController{}).callTool(context.Background(), "brw_find", json.RawMessage(tt.args))
			if rpcErr == nil {
				t.Fatal("a read-only brw_find accepted a level it cannot apply")
			}
			if !strings.Contains(rpcErr.Message, tt.wantErr) {
				t.Fatalf("error = %q, want it to say why the level cannot apply", rpcErr.Message)
			}
		})
	}
	// full is what a read-only find already does, so it is accepted.
	full := toolText(t, observeCallTool(t, &observeController{findElements: observeFixtureResult().Elements},
		"brw_find", `{"query":"Check","observe":"full"}`))
	if !strings.Contains(full, `"elements"`) {
		t.Fatalf("a read-only brw_find with observe:\"full\" lost the match list: %s", full)
	}
}

// With an action the parameter is real: it trims the post-action observation
// and leaves matched, which is the answer to "which element did you act on".
func TestFindActHonoursObserveAndKeepsTheMatch(t *testing.T) {
	controller := &observeController{findElements: []snapshot.Element{{Ref: "e9", Role: "button", Name: "Checkout"}}}
	got := toolText(t, observeCallTool(t, controller, "brw_find",
		`{"query":"Checkout","role":"button","action":"click","observe":"none"}`))
	var decoded struct {
		Matched snapshot.Element `json:"matched"`
		Action  string           `json:"action"`
		Result  map[string]any   `json:"result"`
	}
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Matched.Ref != "e9" || decoded.Action != "click" {
		t.Fatalf("observe:\"none\" trimmed away which element was acted on: %s", got)
	}
	if _, present := decoded.Result["elements"]; present {
		t.Fatalf("observe:\"none\" left the observation on a locate-and-act: %s", got)
	}
	if decoded.Result["ok"] != true {
		t.Fatalf("observe:\"none\" lost the outcome: %s", got)
	}
}

// The step verbs observe classifies and the step verbs brw_plan advertises have
// to be the same set. An advertised verb missing from the table reports in full
// whatever the caller asked for; a table entry for a verb nobody can send is a
// classification of nothing.
func TestEveryPlanStepVerbIsClassifiedForObserve(t *testing.T) {
	advertised := advertisedPlanStepVerbs(t)
	for _, verb := range advertised {
		if !browser.PlanStepVerbIsClassified(verb) {
			t.Errorf("brw_plan advertises step verb %q, which observe does not classify", verb)
		}
	}
	for _, verb := range browser.ClassifiedPlanStepVerbs() {
		if !slices.Contains(advertised, verb) {
			t.Errorf("observe classifies step verb %q, which brw_plan does not advertise", verb)
		}
	}
}

// advertisedPlanStepVerbs reads the step enum off the catalogue, which is the
// set of verbs a caller can actually send.
func advertisedPlanStepVerbs(t *testing.T) []string {
	t.Helper()
	steps, ok := toolProperties(t, "brw_plan")["steps"].(map[string]any)
	if !ok {
		t.Fatal("brw_plan does not advertise steps")
	}
	items, _ := steps["items"].(map[string]any)
	props, _ := items["properties"].(map[string]any)
	action, _ := props["action"].(map[string]any)
	advertised, _ := action["enum"].([]string)
	if len(advertised) == 0 {
		t.Fatal("brw_plan's step action advertises no verbs")
	}
	return advertised
}

// A plan step that fetched a snapshot keeps it at every level. SKILL.md sends
// agents to brw_plan for a mid-flow snapshot, and the default trim used to
// delete the one thing the step was written to return.
func TestPlanSnapshotStepSurvivesEveryLevel(t *testing.T) {
	controller := &observeController{}
	for _, args := range []string{
		`{"steps":[{"action":"snapshot"},{"action":"click","ref":"e1"},{"action":"click","ref":"e2"}]}`,
		`{"steps":[{"action":"snapshot"},{"action":"click","ref":"e1"},{"action":"click","ref":"e2"}],"observe":"none"}`,
	} {
		response := toolText(t, observeCallTool(t, controller, "brw_plan", args))
		var decoded struct {
			Steps []struct {
				Action   string                 `json:"action"`
				Snapshot *snapshot.PageSnapshot `json:"snapshot"`
			} `json:"steps"`
		}
		if err := json.Unmarshal([]byte(response), &decoded); err != nil {
			t.Fatalf("decode plan: %v", err)
		}
		if len(decoded.Steps) != 3 || decoded.Steps[0].Action != "snapshot" {
			t.Fatalf("plan returned %d steps: %s", len(decoded.Steps), response)
		}
		if decoded.Steps[0].Snapshot == nil || len(decoded.Steps[0].Snapshot.Elements) == 0 {
			t.Fatalf("a mid-plan snapshot step returned no snapshot for %s: %s", args, response)
		}
	}
}
