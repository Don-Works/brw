package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
)

type stdioCancelController struct {
	fakeController
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
	contextOnly bool
}

func newStdioCancelController(contextOnly bool) *stdioCancelController {
	return &stdioCancelController{
		started:     make(chan struct{}),
		release:     make(chan struct{}),
		contextOnly: contextOnly,
	}
}

func (c *stdioCancelController) ExecutePlan(ctx context.Context, _ []browser.PlanStep) (browser.PlanResult, error) {
	c.startOnce.Do(func() { close(c.started) })
	if c.contextOnly {
		<-ctx.Done()
	} else {
		select {
		case <-c.release:
		case <-ctx.Done():
		}
	}
	return browser.PlanResult{OK: false, Cancelled: true, Error: "cancelled"}, nil
}

func (c *stdioCancelController) Cancel(context.Context, string) (browser.CancelResult, error) {
	c.releaseOnce.Do(func() { close(c.release) })
	return browser.CancelResult{OK: true, Cancelled: 1}, nil
}

func runPipedServer(t *testing.T, ctrl browser.Controller, writeRequests func(io.Writer)) []map[string]any {
	t.Helper()
	reader, writer := io.Pipe()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- New(ctrl).Serve(context.Background(), reader, &output) }()

	writeRequests(writer)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not finish after cancellation")
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	responses := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var response map[string]any
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("invalid response %q: %v", line, err)
		}
		responses = append(responses, response)
	}
	return responses
}

func TestSameStdioCancelInterruptsRunningPlan(t *testing.T) {
	ctrl := newStdioCancelController(false)
	responses := runPipedServer(t, ctrl, func(w io.Writer) {
		_, _ = io.WriteString(w, lineJSON(t, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "brw_plan", "arguments": map[string]any{"steps": []any{}}},
		}))
		select {
		case <-ctrl.started:
		case <-time.After(time.Second):
			t.Fatal("plan never started")
		}
		_, _ = io.WriteString(w, lineJSON(t, map[string]any{
			"jsonrpc": "2.0", "id": 2, "method": "tools/call",
			"params": map[string]any{"name": "brw_cancel", "arguments": map[string]any{}},
		}))
	})
	if len(responses) != 2 {
		t.Fatalf("responses = %+v, want plan and cancel", responses)
	}
	byID := map[float64]map[string]any{}
	for _, response := range responses {
		byID[response["id"].(float64)] = response
	}
	if byID[1] == nil || byID[2] == nil {
		t.Fatalf("response ids = %+v", byID)
	}
	cancelResult := byID[2]["result"].(map[string]any)["structuredContent"].(map[string]any)
	if cancelResult["cancelled"] != float64(1) {
		t.Fatalf("cancel result = %+v", cancelResult)
	}
}

func TestMCPCancelNotificationCancelsRequestContext(t *testing.T) {
	ctrl := newStdioCancelController(true)
	responses := runPipedServer(t, ctrl, func(w io.Writer) {
		_, _ = io.WriteString(w, lineJSON(t, map[string]any{
			"jsonrpc": "2.0", "id": "slow-1", "method": "tools/call",
			"params": map[string]any{"name": "brw_plan", "arguments": map[string]any{"steps": []any{}}},
		}))
		select {
		case <-ctrl.started:
		case <-time.After(time.Second):
			t.Fatal("plan never started")
		}
		_, _ = io.WriteString(w, lineJSON(t, map[string]any{
			"jsonrpc": "2.0", "method": "notifications/cancelled",
			"params": map[string]any{"requestId": "slow-1", "reason": "test"},
		}))
	})
	if len(responses) != 1 || responses[0]["id"] != "slow-1" {
		t.Fatalf("responses = %+v, cancellation notification must not receive its own response", responses)
	}
}
