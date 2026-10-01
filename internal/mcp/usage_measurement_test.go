package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
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

func TestUsageObservationMetadataCoversCompactAndJSONSnapshots(t *testing.T) {
	for _, format := range []string{"compact", "json"} {
		controller := &usageForwardingController{}
		server := New(controller)
		args := json.RawMessage(`{"name":"brw_snapshot","arguments":{"format":"` + format + `","since":7}}`)
		_, rpcErr := server.handle(context.Background(), "tools/call", args)
		if rpcErr != nil || len(controller.events) != 1 {
			t.Fatalf("rpc=%v events=%+v", rpcErr, controller.events)
		}
		event := controller.events[0]
		if event.SnapshotMode != "frontier" || event.OutputFormat != format || event.ElementLimit == nil || *event.ElementLimit != 40 || event.ReturnedElements == nil || event.DeltaRequested == nil || !*event.DeltaRequested || event.DeltaReturned == nil {
			t.Fatalf("event=%+v", event)
		}
	}
}

type tabUsageController struct{ usageForwardingController }

func (c *tabUsageController) ListTabs(context.Context) ([]browser.Tab, error) {
	return tabProjectionFixture(3), nil
}

func TestUsageRecordsActualTabProjection(t *testing.T) {
	for _, format := range []string{"json", "compact"} {
		controller := &tabUsageController{}
		server := New(controller)
		arguments := map[string]any{"format": format}
		if format == "compact" {
			arguments["limit"] = 1
		}
		args, _ := json.Marshal(map[string]any{"name": "brw_list_tabs", "arguments": arguments})
		result, rpcErr := server.handle(context.Background(), "tools/call", args)
		if rpcErr != nil || len(controller.events) != 1 {
			t.Fatalf("rpc=%v events=%+v", rpcErr, controller.events)
		}
		event := controller.events[0]
		encoded, _ := json.Marshal(result)
		if event.OutputFormat != format || event.OutputBytes == nil || *event.OutputBytes != int64(len(encoded)) || event.SnapshotMode != "" || event.ElementLimit != nil {
			t.Fatalf("event=%+v", event)
		}
		if format == "compact" && (event.ResultTruncated == nil || !*event.ResultTruncated) {
			t.Fatalf("missing truncation: %+v", event)
		}
		if format == "json" && event.ResultTruncated != nil {
			t.Fatalf("legacy truncation inferred: %+v", event)
		}
	}
}
