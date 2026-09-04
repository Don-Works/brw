package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/recipe"
)

type capabilityController struct {
	fakeController
	artifactCalls int
	recipeCalls   int
	runRequest    recipe.RunRequest
	artifactTabID string
	recipeTabID   string
}

func (c *capabilityController) CaptureArtifact(ctx context.Context, _ artifact.CaptureOptions) (artifact.Meta, error) {
	c.artifactCalls++
	c.artifactTabID = browser.TabIDFromContext(ctx)
	return artifact.Meta{ID: "art_0123456789abcdef0123456789abcdef", Kind: "text", MIMEType: "text/plain", SizeBytes: 5000000}, nil
}
func (c *capabilityController) ArtifactInfo(context.Context, string) (artifact.Meta, error) {
	return artifact.Meta{}, nil
}
func (c *capabilityController) ReadArtifact(context.Context, string, int64, int) (artifact.Chunk, error) {
	return artifact.Chunk{}, nil
}
func (c *capabilityController) SearchArtifact(context.Context, string, string, int) ([]artifact.TextHit, error) {
	return nil, nil
}
func (c *capabilityController) DeleteArtifact(context.Context, string) error { return nil }
func (c *capabilityController) SearchRecipes(context.Context, string, string, int) ([]recipe.Match, error) {
	return nil, nil
}
func (c *capabilityController) RunRecipe(ctx context.Context, request recipe.RunRequest) (recipe.RunResult, error) {
	c.recipeCalls++
	c.runRequest = request
	c.recipeTabID = browser.TabIDFromContext(ctx)
	return recipe.RunResult{RecipeID: request.ID, RecipeVersion: request.Version, RecipeDigest: request.Digest, Status: "done"}, nil
}

type fallbackArtifactAPI struct{ calls int }

func (f *fallbackArtifactAPI) CaptureArtifact(context.Context, artifact.CaptureOptions) (artifact.Meta, error) {
	f.calls++
	return artifact.Meta{}, nil
}
func (*fallbackArtifactAPI) ArtifactInfo(context.Context, string) (artifact.Meta, error) {
	return artifact.Meta{}, nil
}
func (*fallbackArtifactAPI) ReadArtifact(context.Context, string, int64, int) (artifact.Chunk, error) {
	return artifact.Chunk{}, nil
}
func (*fallbackArtifactAPI) SearchArtifact(context.Context, string, string, int) ([]artifact.TextHit, error) {
	return nil, nil
}
func (*fallbackArtifactAPI) DeleteArtifact(context.Context, string) error { return nil }

func TestMCPPrefersBrowserHostCapabilitiesAndNeverEchoesRecipeInputs(t *testing.T) {
	controller := &capabilityController{}
	fallback := &fallbackArtifactAPI{}
	server := &Server{manager: controller, artifacts: fallback, toolProfile: "all"}
	artifactResult, rpcErr := server.callTool(context.Background(), "brw_artifact_capture", json.RawMessage(`{"kind":"text","tab_id":"tab-artifact"}`))
	if rpcErr != nil || controller.artifactCalls != 1 || fallback.calls != 0 || controller.artifactTabID != "tab-artifact" {
		t.Fatalf("artifact rpc=%v remote=%d fallback=%d tab=%q", rpcErr, controller.artifactCalls, fallback.calls, controller.artifactTabID)
	}
	encoded, _ := json.Marshal(artifactResult)
	if strings.Contains(string(encoded), "payload") || len(encoded) > 2048 {
		t.Fatalf("capture returned a large/payload-bearing response (%d bytes): %s", len(encoded), encoded)
	}

	digest := strings.Repeat("d", 64)
	runResult, rpcErr := server.callTool(context.Background(), "brw_recipe_run", json.RawMessage(`{"id":"billing.invoice.download","version":"1","digest":"`+digest+`","inputs":{"credential":"never-echo-me"},"tab_id":"tab-recipe"}`))
	if rpcErr != nil || controller.recipeCalls != 1 || controller.runRequest.Inputs["credential"] != "never-echo-me" || controller.recipeTabID != "tab-recipe" {
		t.Fatalf("recipe rpc=%v calls=%d request=%+v tab=%q", rpcErr, controller.recipeCalls, controller.runRequest, controller.recipeTabID)
	}
	encoded, _ = json.Marshal(runResult)
	if strings.Contains(string(encoded), "never-echo-me") {
		t.Fatalf("recipe result echoed sensitive input: %s", encoded)
	}
	if _, rpcErr := server.callTool(context.Background(), "brw_recipe_run", json.RawMessage(`{"id":"billing.invoice.download","version":"1","digest":"`+digest+`","ignored":true}`)); rpcErr == nil {
		t.Fatal("recipe tool accepted an unknown field")
	}
	if _, rpcErr := server.callTool(context.Background(), "brw_artifact_read", json.RawMessage(`{"artifact_id":"art_0123456789abcdef0123456789abcdef","ignored":true}`)); rpcErr == nil {
		t.Fatal("artifact tool accepted an unknown field")
	}
}

func TestArtifactAndRecipeToolsAreDiscoverable(t *testing.T) {
	want := map[string]bool{
		"brw_artifact_capture": false, "brw_artifact_info": false,
		"brw_artifact_read": false, "brw_artifact_search": false,
		"brw_artifact_delete": false,
		"brw_recipe_search":   false, "brw_recipe_run": false,
	}
	for _, definition := range tools() {
		name, _ := definition["name"].(string)
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("%s missing from tool catalogue", name)
		}
	}
}

func TestArtifactSearchDisclosureCannotBeMistakenForArtifactListing(t *testing.T) {
	for _, definition := range tools() {
		if definition["name"] != "brw_artifact_search" {
			continue
		}
		description, _ := definition["description"].(string)
		if !strings.Contains(description, "does not list or discover artifacts") {
			t.Fatalf("artifact search disclosure is ambiguous: %q", description)
		}
		schema, _ := definition["inputSchema"].(map[string]any)
		required, _ := schema["required"].([]string)
		if len(required) != 2 || required[0] != "artifact_id" || required[1] != "query" {
			t.Fatalf("artifact search required arguments = %v, want artifact_id and query", required)
		}
		return
	}
	t.Fatal("brw_artifact_search missing from tool catalogue")
}
