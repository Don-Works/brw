package httpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/recipe"
)

func TestExtractionClientForwardsCompleteSelectorsAndNamedHandles(t *testing.T) {
	opts := artifact.CaptureOptions{Kind: "extraction_json", Name: "quarterly", Extract: &artifact.ExtractionSpec{Source: "table", Caption: "Quarterly", Headers: []string{"Quarter", "Count"}, MaxRows: 4, MaxColumns: 2, MaxCellChars: 100, MaxBytes: 8192}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/artifacts/capture", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			artifact.CaptureOptions
			TabID string `json:"tab_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(request.CaptureOptions, opts) || request.TabID != "chosen-tab" {
			t.Errorf("forwarded=%+v", request)
		}
		json.NewEncoder(w).Encode(artifact.Meta{ID: "art_0123456789abcdef0123456789abcdef", Kind: "extraction_json"})
	})
	mux.HandleFunc("POST /api/recipes/run", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(recipe.RunResult{Status: "done", Outputs: map[string]artifact.Meta{"quarterly": {ID: "art_0123456789abcdef0123456789abcdef", Kind: "extraction_json"}}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := New(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := browser.WithTabID(context.Background(), "chosen-tab")
	meta, err := client.CaptureArtifact(ctx, opts)
	if err != nil || meta.Kind != "extraction_json" {
		t.Fatalf("capture=%+v err=%v", meta, err)
	}
	result, err := client.RunRecipe(ctx, recipe.RunRequest{ID: "example.extract", Version: "1.0.0", Digest: "digest"})
	if err != nil || result.Outputs["quarterly"].ID != meta.ID {
		t.Fatalf("run=%+v err=%v", result, err)
	}
}
