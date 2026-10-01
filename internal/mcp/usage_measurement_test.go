package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/usagelog"
)

type usageForwardingController struct {
	fakeController
	events []usagelog.Event
}

func (c *usageForwardingController) ReportUsage(_ context.Context, event usagelog.Event) error {
	c.events = append(c.events, event)
	return nil
}

func TestUsageMeasurementCountsRenderedTextAndBinarySeparately(t *testing.T) {
	controller := &usageForwardingController{}
	server := New(controller)
	result := map[string]any{"content": []toolContent{{Type: "text", Text: "雪abc"}, {Type: "image", Data: "c2VjcmV0", MIMEType: "image/png"}}, "structuredContent": map[string]any{"duplicate": "雪abc"}}
	input := json.RawMessage(`{"bytes_base64":"YWJj","text":"雪"}`)
	ctx := usagelog.WithRequestID(context.Background(), "request-safe")
	server.recordToolUsage(ctx, "brw_fill", input, time.Now().Add(-1234*time.Microsecond), result, nil)
	if len(controller.events) != 1 {
		t.Fatalf("events=%+v", controller.events)
	}
	event := controller.events[0]
	encoded, _ := json.Marshal(result)
	structured, _ := json.Marshal(result["structuredContent"])
	if event.OutputBytes == nil || *event.OutputBytes != int64(len(encoded)) || *event.OutputTextChars != 4 || *event.EstimatedOutputTokensChars4 != 1 || *event.BinaryOutputBytes != 8 || *event.StructuredOutputBytes != int64(len(structured)) || *event.InputBytes != int64(len(input)) || *event.BinaryInputBytes != 4 || event.RequestID != "request-safe" || event.DurationUS < 1234 {
		t.Fatalf("event=%+v", event)
	}
	if !reflect.DeepEqual(result["content"], []toolContent{{Type: "text", Text: "雪abc"}, {Type: "image", Data: "c2VjcmV0", MIMEType: "image/png"}}) {
		t.Fatal("measurement altered result")
	}
}

func TestUsageMeasurementIncludesCatalogueAndCanonicalizesUnknownTool(t *testing.T) {
	controller := &usageForwardingController{}
	server := New(controller)
	result, rpcErr := server.handle(context.Background(), "tools/list", nil)
	if rpcErr != nil || len(controller.events) != 1 {
		t.Fatalf("result=%v error=%v", result, rpcErr)
	}
	event := controller.events[0]
	encoded, _ := json.Marshal(result)
	if event.Operation != "tools_list" || event.Scope != "catalogue" || event.Representation != "mcp_catalogue" || *event.OutputBytes != int64(len(encoded)) || *event.OutputTextChars == 0 {
		t.Fatalf("event=%+v", event)
	}
	_, _ = server.handle(context.Background(), "tools/call", json.RawMessage(`{"name":"SENSITIVE_OPERATION","arguments":{}}`))
	if len(controller.events) != 2 || controller.events[1].Operation != "unknown_tool" || controller.events[1].Outcome != "error" {
		t.Fatalf("events=%+v", controller.events)
	}
}
