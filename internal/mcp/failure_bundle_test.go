package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/recipe"
)

const mcpFailureBundleID = "art_00112233445566778899aabbccddeeff"

// failingRecipeController is a browser host whose run fails and collected
// evidence, which is the only shape that carries failure_bundle_artifact_id.
type failingRecipeController struct {
	fakeController
	bundle string
}

func (c *failingRecipeController) CaptureArtifact(context.Context, artifact.CaptureOptions) (artifact.Meta, error) {
	return artifact.Meta{}, nil
}
func (c *failingRecipeController) ArtifactInfo(context.Context, string) (artifact.Meta, error) {
	return artifact.Meta{}, nil
}
func (c *failingRecipeController) ReadArtifact(context.Context, string, int64, int) (artifact.Chunk, error) {
	return artifact.Chunk{}, nil
}
func (c *failingRecipeController) SearchArtifact(context.Context, string, string, int) ([]artifact.TextHit, error) {
	return nil, nil
}
func (c *failingRecipeController) DeleteArtifact(context.Context, string) error { return nil }
func (c *failingRecipeController) SearchRecipes(context.Context, string, string, int) ([]recipe.Match, error) {
	return nil, nil
}

func (c *failingRecipeController) RunRecipe(_ context.Context, request recipe.RunRequest) (recipe.RunResult, error) {
	result := recipe.RunResult{
		RecipeID: request.ID, RecipeVersion: request.Version, RecipeDigest: request.Digest,
		Status: "failed", FailureBundle: c.bundle,
		Steps: []recipe.StepResult{{ID: "pay", Status: "failed"}},
	}
	failure := `step "pay": element detached before the click landed`
	if c.bundle != "" {
		failure += "; failure evidence bundle " + c.bundle
	}
	return result, errors.New(failure)
}

// TestFailedRecipeRunReportsTheEvidenceBundleOverMCP is the client-visible half
// of the bundle. brw_recipe_run's description tells an agent a failed run
// reports failure_bundle_artifact_id; an error result that carries only the
// message would make that untrue on this transport, and the manifest — and so
// every part it names — unreachable.
func TestFailedRecipeRunReportsTheEvidenceBundleOverMCP(t *testing.T) {
	digest := strings.Repeat("d", 64)
	args := json.RawMessage(`{"id":"billing.pay","version":"1.0.0","digest":"` + digest + `"}`)

	tests := []struct {
		name       string
		bundle     string
		wantBundle bool
	}{
		{name: "host collected evidence", bundle: mcpFailureBundleID, wantBundle: true},
		{name: "host collected nothing", bundle: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := &failingRecipeController{bundle: test.bundle}
			server := &Server{manager: controller, toolProfile: "all"}
			raw, rpcErr := server.callTool(context.Background(), "brw_recipe_run", args)
			if rpcErr != nil {
				t.Fatalf("rpc error = %v", rpcErr)
			}
			result, ok := raw.(map[string]any)
			if !ok || result["isError"] != true {
				t.Fatalf("result = %#v, want a tool error", raw)
			}
			text := toolResultText(t, result)
			if !strings.Contains(text, "element detached") {
				t.Fatalf("error text = %q, want the real failure preserved", text)
			}

			structured, _ := result["structuredContent"].(map[string]any)
			if structured == nil {
				t.Fatalf("failed run returned no structured result: %#v", result)
			}
			if structured["status"] != "failed" {
				t.Fatalf("structured result = %#v, want the run detail", structured)
			}
			bundle, _ := structured["failure_bundle_artifact_id"].(string)
			if test.wantBundle {
				if bundle != mcpFailureBundleID {
					t.Fatalf("failure_bundle_artifact_id = %q, want %q", bundle, mcpFailureBundleID)
				}
				if !strings.Contains(text, mcpFailureBundleID) {
					t.Fatalf("error text %q does not name the bundle", text)
				}
				return
			}
			// Nothing collected: the field is absent rather than empty, and the
			// error says nothing about an artifact that does not exist.
			if _, present := structured["failure_bundle_artifact_id"]; present {
				t.Fatalf("a run with no evidence reported a bundle: %#v", structured)
			}
			if strings.Contains(text, "art_") {
				t.Fatalf("error text %q mentions an artifact", text)
			}
		})
	}
}

// TestSuccessfulRecipeRunResultShapeIsUnchanged keeps the failure path from
// having quietly rewritten the ordinary one.
func TestSuccessfulRecipeRunResultShapeIsUnchanged(t *testing.T) {
	controller := &capabilityController{}
	server := &Server{manager: controller, toolProfile: "all"}
	raw, rpcErr := server.callTool(context.Background(), "brw_recipe_run",
		json.RawMessage(`{"id":"billing.pay","version":"1.0.0","digest":"`+strings.Repeat("d", 64)+`"}`))
	if rpcErr != nil {
		t.Fatalf("rpc error = %v", rpcErr)
	}
	result, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("result = %#v", raw)
	}
	if _, isError := result["isError"]; isError {
		t.Fatalf("successful run reported an error: %#v", result)
	}
	if !strings.Contains(toolResultText(t, result), `"status":"done"`) {
		t.Fatalf("successful run text = %q", toolResultText(t, result))
	}
}

func toolResultText(t *testing.T, result map[string]any) string {
	t.Helper()
	content, ok := result["content"].([]toolContent)
	if !ok || len(content) == 0 {
		t.Fatalf("result has no text content: %#v", result)
	}
	return content[0].Text
}
