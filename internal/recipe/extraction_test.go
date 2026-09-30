package recipe

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/artifact"
)

func extractionRecipe() Recipe {
	value := validRecipe("https://billing.example.test")
	value.Inputs = nil
	value.Steps = []Step{{ID: "summary", Action: "capture", Capture: &CaptureSpec{Kind: "extraction_json", Extract: &artifact.ExtractionSpec{Source: "section", Section: "Summary", MaxChars: 200, MaxBytes: 4096}}}}
	return value
}

func TestExtractionRecipeValidationAndOmittedFieldCompatibility(t *testing.T) {
	value := extractionRecipe()
	if err := Validate(value); err != nil {
		t.Fatal(err)
	}
	old := validRecipe("https://billing.example.test")
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"extract"`) || strings.Contains(string(data), `"outputs"`) {
		t.Fatalf("old serialized recipe changed: %s", data)
	}
	roundtrip, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := Digest(old)
	after, _ := Digest(roundtrip)
	if before != after {
		t.Fatalf("digest changed after roundtrip: %s != %s", before, after)
	}
	for _, mutate := range []func(*Recipe){
		func(v *Recipe) { v.Steps[0].Capture.Name = "${input:secret}" },
		func(v *Recipe) { v.Steps[0].Capture.Extract.Section = "${input:secret}" },
		func(v *Recipe) { v.Steps[0].Capture.Extract.MaxBytes = 0 },
		func(v *Recipe) {
			v.Steps = append(v.Steps, Step{ID: "other", Action: "capture", Capture: &CaptureSpec{Kind: "extraction_json", Name: "summary", Extract: v.Steps[0].Capture.Extract}})
		},
		func(v *Recipe) { v.Inputs = map[string]Input{"password": {Secret: true}} },
		func(v *Recipe) {
			v.Steps = append(v.Steps, Step{ID: "login", Action: "fill", Effect: "read", Target: &Target{Role: "textbox", Name: "Password"}, Value: "secret://login"})
		},
	} {
		candidate := extractionRecipe()
		mutate(&candidate)
		if err := Validate(candidate); err == nil {
			t.Fatalf("accepted invalid extraction recipe: %+v", candidate)
		}
	}
}

func TestExtractionRecipeCloneIsolatesSelectors(t *testing.T) {
	value := extractionRecipe()
	value.Steps[0].Capture.Extract.Headers = []string{"Month"}
	value.Steps[0].Capture.Extract.Fields = []string{"price"}
	cloned := cloneRecipe(value)
	cloned.Steps[0].Capture.Extract.Headers[0] = "Changed"
	cloned.Steps[0].Capture.Extract.Fields[0] = "brand"
	cloned.Steps[0].Capture.Extract.Section = "Changed"
	if value.Steps[0].Capture.Extract.Headers[0] != "Month" || value.Steps[0].Capture.Extract.Fields[0] != "price" || value.Steps[0].Capture.Extract.Section != "Summary" {
		t.Fatal("clone changed the source extraction")
	}
}

func TestExtractionRunnerReturnsOnlyNamedMetadataAfterSuccess(t *testing.T) {
	surface := newFakeSurface()
	surface.artifact = artifact.Meta{ID: "art_0123456789abcdef0123456789abcdef", Kind: "extraction_json"}
	value := extractionRecipe()
	value.Origins = []string{surface.origin}
	result, err := (Runner{Surface: surface}).Run(context.Background(), value, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outputs["summary"].ID != surface.artifact.ID || len(result.Artifacts) != 1 {
		t.Fatalf("missing metadata output: %+v", result)
	}
	surface.captureErr = context.Canceled
	result, err = (Runner{Surface: surface}).Run(context.Background(), value, nil)
	if err == nil || len(result.Outputs) != 0 || len(result.Artifacts) != 0 {
		t.Fatalf("failed capture returned outputs: %+v err=%v", result, err)
	}
}
