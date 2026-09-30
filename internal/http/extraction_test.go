package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Don-Works/brw/internal/artifact"
)

func TestExtractionHTTPForwardsNestedBoundsWithoutPayload(t *testing.T) {
	server := New("", &fakeController{})
	api := &artifactAPIFake{}
	server.SetArtifactAPI(api)
	response := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/artifacts/capture", bytes.NewBufferString(`{"kind":"extraction_json","name":"product","extract":{"source":"structured","expected_source":"json_ld","fields":["price","currency"],"max_bytes":4096},"tab_id":"chosen-tab"}`)))
	if response.Code != http.StatusOK || api.capture.Kind != "extraction_json" || api.capture.Name != "product" || api.tabID != "chosen-tab" || !reflect.DeepEqual(api.capture.Extract, &artifact.ExtractionSpec{Source: "structured", ExpectedSource: "json_ld", Fields: []string{"price", "currency"}, MaxBytes: 4096}) {
		t.Fatalf("response=%d opts=%+v body=%s", response.Code, api.capture, response.Body.String())
	}
	bad := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(bad, httptest.NewRequest(http.MethodPost, "/api/artifacts/capture", bytes.NewBufferString(`{"kind":"extraction_json","name":"product","extract":{"source":"structured","raw":true,"max_bytes":4096}}`)))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("unknown extraction field accepted: %s", bad.Body.String())
	}
}
