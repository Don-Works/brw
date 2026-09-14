package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// gettersController answers the getters script with canned page values, so a
// brw_assert call exercises the real dispatch, argument decoding and evaluation
// path without a browser.
type gettersController struct {
	fakeController
	values map[string]any
	seen   []string
}

func (c *gettersController) Evaluate(_ context.Context, expression string) (any, error) {
	c.seen = append(c.seen, expression)
	if value, ok := c.values[expression]; ok {
		return value, nil
	}
	return map[string]any{"value": nil}, nil
}

func newGettersController() *gettersController {
	return &gettersController{values: map[string]any{
		snapshot.BuildGetExpression("url", "", ""):       map[string]any{"value": "https://example.test/dashboard"},
		snapshot.BuildGetExpression("status", "", ""):    map[string]any{"value": float64(200)},
		snapshot.BuildGetExpression("count", ".row", ""): map[string]any{"value": float64(3)},
	}}
}

func TestBrwAssertIsAdvertisedAndCallable(t *testing.T) {
	catalogue := (&Server{toolProfile: "all"}).advertisedTools()
	found := false
	for _, entry := range catalogue {
		if name, _ := entry["name"].(string); name == "brw_assert" {
			found = true
		}
	}
	if !found {
		t.Fatal("brw_assert is not in the advertised tool catalogue")
	}

	controller := newGettersController()
	server := New(controller)

	passing := callToolJSON(t, server, "brw_assert", `{"assertion":"url","mode":"prefix","expected":"https://example.test"}`)
	if passing["ok"] != true || passing["assertion"] != "url" {
		t.Fatalf("passing assertion returned %#v", passing)
	}

	result, rpcErr := server.callTool(context.Background(), "brw_assert",
		json.RawMessage(`{"assertion":"url","expected":"https://example.test/settings"}`))
	if rpcErr != nil {
		t.Fatalf("rpc error: %v", rpcErr)
	}
	payload, _ := result.(map[string]any)
	if payload["isError"] != true {
		t.Fatalf("failing assertion was not a tool error: %#v", payload)
	}
	content, _ := payload["content"].([]toolContent)
	if len(content) == 0 {
		t.Fatalf("failing assertion returned no content: %#v", payload)
	}
	want := `url assertion failed: expected exact "https://example.test/settings", actual "https://example.test/dashboard"`
	if content[0].Text != want {
		t.Fatalf("message = %q, want %q", content[0].Text, want)
	}
}

func TestBrwAssertRejectsUnknownArguments(t *testing.T) {
	server := New(newGettersController())
	if _, rpcErr := server.callTool(context.Background(), "brw_assert",
		json.RawMessage(`{"assertion":"http_status","status":200,"typo":true}`)); rpcErr == nil {
		t.Fatal("brw_assert accepted an unknown argument")
	}
}

func TestBrwAssertReportsAnInvalidRequest(t *testing.T) {
	server := New(newGettersController())
	result, rpcErr := server.callTool(context.Background(), "brw_assert",
		json.RawMessage(`{"assertion":"element_count","selector":".row"}`))
	if rpcErr != nil {
		t.Fatalf("rpc error: %v", rpcErr)
	}
	payload, _ := result.(map[string]any)
	content, _ := payload["content"].([]toolContent)
	if payload["isError"] != true || len(content) == 0 {
		t.Fatalf("invalid request was not a tool error: %#v", payload)
	}
	if !strings.Contains(content[0].Text, "element count assertion requires count, min or max") {
		t.Fatalf("message = %q", content[0].Text)
	}
}

// TestBatchAcceptsAnAssertStep proves the assertion vocabulary reaches brw_batch
// through the MCP surface: the step decodes into the controller's BatchStep with
// its assertion intact.
func TestBatchAcceptsAnAssertStep(t *testing.T) {
	controller := &batchRecordingController{gettersController: newGettersController()}
	server := New(controller)

	callToolJSON(t, server, "brw_batch",
		`{"steps":[{"action":"assert","assertion":{"assertion":"element_count","selector":".row","count":3}}]}`)

	if len(controller.steps) != 1 {
		t.Fatalf("controller saw %d steps, want 1", len(controller.steps))
	}
	step := controller.steps[0]
	if step.Action != "assert" || step.Assertion == nil {
		t.Fatalf("step = %+v, want an assert step carrying an assertion", step)
	}
	if step.Assertion.Assertion != browser.AssertionElementCount || step.Assertion.Selector != ".row" {
		t.Fatalf("assertion = %+v", *step.Assertion)
	}
	if step.Assertion.Count == nil || *step.Assertion.Count != 3 {
		t.Fatalf("count = %v, want 3", step.Assertion.Count)
	}
}

type batchRecordingController struct {
	*gettersController
	steps []browser.BatchStep
}

func (c *batchRecordingController) ExecuteBatch(_ context.Context, steps []browser.BatchStep) (browser.BatchResult, error) {
	c.steps = steps
	return browser.BatchResult{OK: true, StepsCompleted: len(steps)}, nil
}
