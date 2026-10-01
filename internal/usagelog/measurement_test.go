package usagelog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMeasurementExcludesBinaryAndCountsUnicode(t *testing.T) {
	input := []byte(`{"text":"雪","bytes_base64":"c2VjcmV0","nested":{"type":"image","data":"YWJj"}}`)
	chars, binary := MeasureJSON(input)
	if binary != 12 || chars != int64(len(string(input))-2)-12 {
		t.Fatalf("chars=%d binary=%d", chars, binary)
	}
	if EstimateTokens(5) != 2 || EstimateTokens(0) != 0 {
		t.Fatal("bad estimate rounding")
	}
	ctx := WithRequestID(context.Background(), "request-1")
	if RequestID(ctx) != "request-1" || RequestID(WithRequestID(ctx, "password secret")) != "" {
		t.Fatal("unsafe correlation")
	}
}

func TestMeasurementRecorderPreservesKnownZeroAndDropsUnknownEnums(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	r, err := New(Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Record(Event{Layer: "mcp", Operation: "brw_fill", Outcome: "ok", Scope: "SECRET_SCOPE", Representation: "SECRET_REP", InputBytes: Count(0)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SECRET") {
		t.Fatalf("retained enums: %s", data)
	}
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	if event.SchemaVersion != 1 || event.InputBytes == nil || *event.InputBytes != 0 || event.OutputBytes != nil {
		t.Fatalf("known vs unknown: %+v", event)
	}
}
