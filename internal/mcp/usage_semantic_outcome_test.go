package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/usagelog"
)

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
					if rpcErr != nil || result.(map[string]any)["isError"] == true {
						t.Fatal("fixture should use existing normal MCP result contract")
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
