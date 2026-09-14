package mcp

import (
	"context"
	"encoding/json"
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

func (c *observeController) Find(_ context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	c.acted = append(c.acted, "find:"+opts.Query+"/"+opts.Role)
	return snapshot.FindResult{URL: "https://fixture.test/cart", Title: "Cart", Elements: c.findElements}, nil
}

func (c *observeController) ExecuteBatch(context.Context, []browser.BatchStep) (browser.BatchResult, error) {
	return browser.BatchResult{
		OK: true, TabID: "tab1", URL: "https://fixture.test/cart", Title: "Cart", Focus: "e9", Version: 7,
		Changed:        []string{"e9 button \"Checkout\""},
		Steps:          []browser.BatchStepResult{{Index: 0, Action: "click", OK: true}},
		StepsCompleted: 1,
	}, nil
}

func (c *observeController) ExecutePlan(_ context.Context, steps []browser.PlanStep) (browser.PlanResult, error) {
	result := browser.PlanResult{OK: true, StepsCompleted: len(steps)}
	for i := range steps {
		result.Steps = append(result.Steps, browser.PlanStepResult{
			Index: i, Action: "click", OK: true, Message: "click ok", Result: observeFixtureResult(),
		})
	}
	return result, nil
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

func TestUnknownObserveLevelIsRefused(t *testing.T) {
	_, rpcErr := New(&observeController{}).callTool(context.Background(), "brw_click", json.RawMessage(`{"ref":"e4","observe":"summary"}`))
	if rpcErr == nil {
		t.Fatal("brw_click accepted an unknown observe level")
	}
	if !strings.Contains(rpcErr.Message, "unknown observe level") {
		t.Fatalf("error = %q, want it to name the unknown level", rpcErr.Message)
	}
}

// Every tool whose handler reads observe must advertise it, or no agent will
// ever pass it.
func TestObserveIsAdvertisedByEveryToolThatReadsIt(t *testing.T) {
	for _, name := range observeToolNames() {
		props := toolProperties(t, name)
		schema, ok := props["observe"].(map[string]any)
		if !ok {
			t.Errorf("%s reads observe but does not advertise it", name)
			continue
		}
		values, _ := schema["enum"].([]string)
		if len(values) != 3 {
			t.Errorf("%s advertises observe with enum %v, want the three levels", name, values)
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
