package recipe

import (
	"context"
	"strings"
	"testing"
)

func TestInlineRecipeWithoutProviderReportsIdentityAndDoesNotInstall(t *testing.T) {
	surface := newFakeSurface()
	service, err := NewService(nil, Runner{Surface: surface})
	if err != nil {
		t.Fatal(err)
	}
	value := validRecipe(surface.origin)
	value.Steps = value.Steps[:1]
	digest, _ := Digest(value)
	result, err := service.RunRecipe(context.Background(), RunRequest{Recipe: &value, Inputs: map[string]string{"month": "private-month"}})
	if err != nil || result.Status != "done" || result.RecipeID != value.ID || result.RecipeVersion != value.Version || result.RecipeDigest != digest || surface.lastFill != "private-month" {
		t.Fatalf("result=%+v err=%v fills=%d", result, err, surface.fills)
	}
	matches, err := service.SearchRecipes(context.Background(), "invoices", "", 10)
	if err != nil || matches == nil || len(matches) != 0 {
		t.Fatalf("inline recipe appeared in search: %v, %v", matches, err)
	}
	_, err = service.RunRecipe(context.Background(), RunRequest{ID: value.ID, Version: value.Version, Digest: digest})
	if err == nil || !strings.Contains(err.Error(), "provider is not configured") {
		t.Fatalf("stored run without provider: %v", err)
	}
}

func TestInlineRecipeRefusesInvalidRequestsBeforeActuation(t *testing.T) {
	for _, name := range []string{"missing", "mixed id", "mixed version", "mixed digest", "schema", "origin", "input", "write verification", "oversized"} {
		t.Run(name, func(t *testing.T) {
			surface := newFakeSurface()
			service, err := NewService(nil, Runner{Surface: surface})
			if err != nil {
				t.Fatal(err)
			}
			value := validRecipe(surface.origin)
			value.Steps = value.Steps[:1]
			request := RunRequest{Recipe: &value, Inputs: map[string]string{"month": "September"}}
			switch name {
			case "missing":
				request.Recipe = nil
			case "mixed id":
				request.ID = value.ID
			case "mixed version":
				request.Version = value.Version
			case "mixed digest":
				request.Digest, _ = Digest(value)
			case "schema":
				value.SchemaVersion = 999
			case "origin":
				value.Origins = []string{"https://other.example.test"}
			case "input":
				request.Inputs = nil
			case "write verification":
				value.Risk = "external_write"
				value.Steps[0].Effect = "external_write"
				value.Steps[0].IdempotencyKey = "fill-month"
				value.Steps[0].Postcondition = &Event{Kind: "element.value", Target: value.Steps[0].Target, Match: "${input:month}", TimeoutMS: 1000}
			case "oversized":
				value.Metadata = map[string]string{"large": strings.Repeat("x", 1<<20)}
			}
			if _, err := service.RunRecipe(context.Background(), request); err == nil {
				t.Fatal("invalid request ran")
			}
			if surface.fills != 0 || surface.clicks != 0 {
				t.Fatal("invalid request actuated the browser")
			}
		})
	}
}
