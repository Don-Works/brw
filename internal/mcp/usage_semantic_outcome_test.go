package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/Don-Works/brw/internal/usagelog"
)

type semanticFailureController struct {
	usageForwardingController
	actions int
}

func (c *semanticFailureController) Click(context.Context, string) (browser.ActionResult, error) {
	c.actions++
	return browser.ActionResult{Message: `element ref "e9" not recoverable: no_key PRIVATE_FAILURE_SENTINEL`, TabID: "fixture-tab", URL: "https://fixture.test/"}, nil
}

func (c *semanticFailureController) Press(ctx context.Context, _ string) (browser.ActionResult, error) {
	return c.Click(ctx, "e9")
}

func (c *semanticFailureController) Scroll(ctx context.Context, _ string) (browser.ActionResult, error) {
	return c.Click(ctx, "e9")
}

func (c *semanticFailureController) NavigateTo(ctx context.Context, _ string) (browser.ActionResult, error) {
	return c.Click(ctx, "e9")
}

func (c *semanticFailureController) Find(context.Context, snapshot.FindOptions) (snapshot.FindResult, error) {
	return snapshot.FindResult{Elements: []snapshot.Element{findFixtureElement("e9", "button", "Next")}}, nil
}

func (c *semanticFailureController) FindLive(ctx context.Context, opts snapshot.FindOptions) (snapshot.FindResult, error) {
	return c.Find(ctx, opts)
}

func (*semanticFailureController) Evaluate(context.Context, string) (any, error) {
	return map[string]any{"ok": false, "error": "timed out"}, nil
}

func TestMCPSemanticActionFailuresKeepDetailsAndClassifyUsage(t *testing.T) {
	for _, call := range []struct{ name, args string }{
		{"brw_click", `{"ref":"e9"}`},
		{"brw_press", `{"key":"Enter","repeat":4}`},
		{"brw_scroll", `{"direction":"down","repeat":4}`},
		{"brw_navigate_to", `{"url":"https://fixture.test/"}`},
		{"brw_find", `{"query":"Next","action":"click"}`},
	} {
		for _, level := range []string{"full", "minimal", "none"} {
			t.Run(call.name+"/"+level, func(t *testing.T) {
				controller := &semanticFailureController{}
				args := json.RawMessage(`{"name":` + jsonString(call.name) + `,"arguments":` + withObserve(t, call.args, level) + `}`)
				result, rpcErr := New(controller).handle(context.Background(), "tools/call", args)
				if rpcErr != nil || result.(map[string]any)["isError"] != true || controller.actions != 1 {
					t.Fatalf("rpc=%v actions=%d result=%+v", rpcErr, controller.actions, result)
				}
				body := toolText(t, result.(map[string]any))
				if !strings.Contains(body, "PRIVATE_FAILURE_SENTINEL") || !strings.Contains(body, "fixture-tab") {
					t.Fatalf("failure details dropped: %s", body)
				}
				if len(controller.events) != 1 {
					t.Fatalf("events=%+v", controller.events)
				}
				event := controller.events[0]
				if event.Outcome != "error" || event.ErrorClass != "stale_reference" || event.ErrorFingerprint == "" {
					t.Fatalf("event=%+v", event)
				}
				encoded, _ := json.Marshal(event)
				if strings.Contains(string(encoded), "PRIVATE_FAILURE_SENTINEL") || strings.Contains(string(encoded), "fixture.test") {
					t.Fatalf("private failure details entered usage ledger: %s", encoded)
				}
			})
		}
	}
}

func TestMCPEvaluateFalseOKRemainsPageData(t *testing.T) {
	controller := &semanticFailureController{}
	result, rpcErr := New(controller).handle(context.Background(), "tools/call", json.RawMessage(`{"name":"brw_evaluate","arguments":{"expression":"fixture"}}`))
	if rpcErr != nil || result.(map[string]any)["isError"] == true || len(controller.events) != 1 || controller.events[0].Outcome != "ok" {
		t.Fatalf("result=%+v rpc=%v events=%+v", result, rpcErr, controller.events)
	}
	if text := toolText(t, result.(map[string]any)); text != `{"error":"timed out","ok":false}` {
		t.Fatalf("page data changed: %s", text)
	}
}

func TestUsageRecordsSemanticBatchAndPlanFailures(t *testing.T) {
	for _, level := range []browser.ObserveLevel{browser.ObserveFull, browser.ObserveMinimal, browser.ObserveNone} {
		for _, tc := range []struct {
			message, class string
			cancelled      bool
		}{
			{"timed out waiting for fixture predicate", "timeout", false},
			{"element ref not recoverable: no_key", "stale_reference", false},
			{"cancelled", "canceled", true},
			{"fixture semantic failure", "tool", false},
		} {
			for _, operation := range []string{"brw_batch", "brw_plan"} {
				t.Run(operation+"/"+string(level)+"/"+tc.class, func(t *testing.T) {
					obs := observer{level: level, explicit: true}
					var result any
					var rpcErr *rpcError
					if operation == "brw_batch" {
						result, rpcErr = obs.batch(browser.BatchResult{OK: false, Error: tc.message, Cancelled: tc.cancelled}, nil)
					} else {
						result, rpcErr = obs.plan(browser.PlanResult{OK: false, Error: tc.message, Cancelled: tc.cancelled}, nil)
					}
					result = withSemanticFailure(operation, result)
					if rpcErr != nil || (result.(map[string]any)["isError"] == true) == tc.cancelled {
						t.Fatal("failed sequence status or documented cancellation status changed")
					}
					before, _ := json.Marshal(result)
					controller := &usageForwardingController{}
					New(controller).recordToolUsage(context.Background(), operation, nil, time.Now(), result, nil)
					if len(controller.events) != 1 {
						t.Fatal("missing usage event")
					}
					event := controller.events[0]
					if event.Outcome != "error" || event.ErrorClass != tc.class || event.ErrorFingerprint == "" || event.Retryable != usagelog.Retryable(tc.class) {
						t.Fatalf("outcome=%s class=%s fingerprint=%s retryable=%t", event.Outcome, event.ErrorClass, event.ErrorFingerprint, event.Retryable)
					}
					after, _ := json.Marshal(result)
					if string(before) != string(after) {
						t.Fatal("usage changed MCP payload")
					}
				})
			}
		}
	}
}

func TestUsageSemanticClassificationIgnoresPageDataAndMismatchedTypes(t *testing.T) {
	for _, tc := range []struct {
		operation string
		value     any
	}{
		{"brw_evaluate", map[string]any{"ok": false, "error": "timed out"}},
		{"brw_call_page_tool", map[string]any{"ok": false, "error": "timed out"}},
		{"brw_batch", map[string]any{"ok": false, "error": "timed out"}},
		{"brw_plan", browser.BatchResult{OK: false, Error: "timed out"}},
		{"brw_evaluate", browser.BatchResult{OK: false, Error: "timed out"}},
		{"brw_evaluate", browser.ActionResult{Message: "timed out"}},
		{"brw_find", browser.FindActResult{Result: browser.ActionResult{Message: "timed out"}}},
		{"brw_batch", browser.BatchResult{OK: true}},
		{"brw_plan", browser.PlanResult{OK: true}},
	} {
		result, rpcErr := toolJSON(tc.value, nil)
		controller := &usageForwardingController{}
		New(controller).recordToolUsage(context.Background(), tc.operation, nil, time.Now(), result, rpcErr)
		event := controller.events[0]
		if event.Outcome != "ok" || event.ErrorClass != "" || event.ErrorFingerprint != "" {
			t.Fatalf("operation=%s type=%T misclassified", tc.operation, tc.value)
		}
	}
}

func TestMCPCancelledSequencesRetainPartialProgress(t *testing.T) {
	for _, operation := range []string{"brw_batch", "brw_plan"} {
		t.Run(operation, func(t *testing.T) {
			var result any
			if operation == "brw_batch" {
				result, _ = toolJSON(browser.BatchResult{Cancelled: true, StepsCompleted: 1, Error: "cancelled", Steps: []browser.BatchStepResult{{Index: 0, OK: true}}}, nil)
			} else {
				result, _ = toolJSON(browser.PlanResult{Cancelled: true, StepsCompleted: 1, Error: "cancelled", Steps: []browser.PlanStepResult{{Index: 0, OK: true}}}, nil)
			}
			result = withSemanticFailure(operation, result)
			if result.(map[string]any)["isError"] == true {
				t.Fatal("documented cancellation became a wire error")
			}
			var progress struct {
				Cancelled bool `json:"cancelled"`
				Completed int  `json:"steps_completed"`
				Steps     []struct {
					OK bool `json:"ok"`
				} `json:"steps"`
			}
			if err := json.Unmarshal([]byte(toolText(t, result.(map[string]any))), &progress); err != nil || !progress.Cancelled || progress.Completed != 1 || len(progress.Steps) != 1 || !progress.Steps[0].OK {
				t.Fatalf("progress=%+v err=%v", progress, err)
			}
			outcome, class, _ := mcpUsageOperationOutcome(operation, result, nil)
			if outcome != "error" || class != "canceled" {
				t.Fatalf("outcome=%s class=%s", outcome, class)
			}
		})
	}
}

func TestUsageSemanticClassificationKeepsAssertionAndRPCFailures(t *testing.T) {
	for _, tc := range []struct {
		operation string
		result    any
		rpcErr    *rpcError
		class     string
	}{
		{"brw_assert", toolError(errors.New("fixture assertion failed")), nil, "tool"},
		{"brw_assert_text", toolError(context.DeadlineExceeded), nil, "timeout"},
		{"brw_assert_value", toolError(context.Canceled), nil, "canceled"},
		{"brw_batch", nil, &rpcError{Code: -32602, Message: "fixture invalid argument"}, "rpc"},
	} {
		controller := &usageForwardingController{}
		New(controller).recordToolUsage(context.Background(), tc.operation, nil, time.Now(), tc.result, tc.rpcErr)
		event := controller.events[0]
		if event.Outcome != "error" || event.ErrorClass != tc.class {
			t.Fatalf("operation=%s outcome=%s class=%s", tc.operation, event.Outcome, event.ErrorClass)
		}
	}
}
