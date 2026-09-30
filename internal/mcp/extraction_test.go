package mcp

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/artifact"
)

type extractionCapability struct {
	capabilityController
	capture artifact.CaptureOptions
}

func (c *extractionCapability) CaptureArtifact(_ context.Context, opts artifact.CaptureOptions) (artifact.Meta, error) {
	c.capture = opts
	return artifact.Meta{ID: "art_0123456789abcdef0123456789abcdef", Kind: "extraction_json"}, nil
}

func TestExtractionMCPCaptureSchemaAndHandlerPreserveUnion(t *testing.T) {
	properties := toolProperties(t, "brw_artifact_capture")
	kind, _ := properties["kind"].(map[string]any)
	data, _ := json.Marshal(kind["enum"])
	if !strings.Contains(string(data), "extraction_json") || properties["name"] == nil || properties["extract"] == nil {
		t.Fatalf("extraction schema unavailable: %+v", properties)
	}
	controller := &extractionCapability{}
	server := &Server{manager: controller}
	result, rpcErr := server.callTool(context.Background(), "brw_artifact_capture", json.RawMessage(`{"kind":"extraction_json","name":"summary","extract":{"source":"section","section":"Summary","max_chars":100,"max_bytes":4096}}`))
	if rpcErr != nil || controller.capture.Name != "summary" || !reflect.DeepEqual(controller.capture.Extract, &artifact.ExtractionSpec{Source: "section", Section: "Summary", MaxChars: 100, MaxBytes: 4096}) {
		t.Fatalf("capture=%+v rpc=%v", controller.capture, rpcErr)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "provenance") || strings.Contains(string(encoded), `"data"`) {
		t.Fatalf("capture returned extraction bytes: %s", encoded)
	}
}
