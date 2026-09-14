package httpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	httpapi "github.com/Don-Works/brw/internal/http"
	"github.com/Don-Works/brw/internal/recipe"
)

const failureBundleID = "art_00112233445566778899aabbccddeeff"

// failingRecipeAPI is a browser host whose run fails and collected evidence,
// which is the only shape that carries failure_bundle_artifact_id.
type failingRecipeAPI struct{ recipe.API }

func (failingRecipeAPI) RunRecipe(_ context.Context, request recipe.RunRequest) (recipe.RunResult, error) {
	return recipe.RunResult{
			RecipeID: request.ID, RecipeVersion: request.Version, RecipeDigest: request.Digest,
			Status: "failed", FailureBundle: failureBundleID,
			Steps: []recipe.StepResult{{ID: "pay", Status: "failed"}},
		},
		errors.New(`step "pay": element detached before the click landed; failure evidence bundle ` + failureBundleID)
}

// TestFailedRunCarriesTheEvidenceBundleAcrossTheProxy runs the real daemon
// handler behind the real proxy client. A failed run is the only run that
// reports failure_bundle_artifact_id, so an error writer that reduces the
// failure to its message would leave the field the tool description promises
// unreachable on this transport while it works on direct CDP.
func TestFailedRunCarriesTheEvidenceBundleAcrossTheProxy(t *testing.T) {
	daemon := httpapi.New("", strictHandlerController{})
	daemon.SetRecipeAPI(failingRecipeAPI{})
	server := httptest.NewServer(daemon.Handler())
	t.Cleanup(server.Close)

	client, err := New(server.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.RunRecipe(browser.WithTabID(context.Background(), "77"), recipe.RunRequest{
		ID: "billing.pay", Version: "1.0.0", Digest: strings.Repeat("d", 64),
	})
	if err == nil {
		t.Fatal("the run was supposed to fail")
	}
	if !strings.Contains(err.Error(), "element detached") {
		t.Fatalf("error = %q, want the real failure preserved", err)
	}
	if result.FailureBundle != failureBundleID {
		t.Fatalf("failure_bundle_artifact_id = %q, want %q; the evidence is unreachable through this transport",
			result.FailureBundle, failureBundleID)
	}
	if result.Status != "failed" || len(result.Steps) != 1 || result.Steps[0].ID != "pay" {
		t.Fatalf("failed run result = %+v, want the run detail decoded alongside the error", result)
	}

	// A successful run is untouched: it still answers 200 with the result alone.
	okDaemon := httpapi.New("", strictHandlerController{})
	okDaemon.SetRecipeAPI(succeedingRecipeAPI{})
	okServer := httptest.NewServer(okDaemon.Handler())
	t.Cleanup(okServer.Close)
	okClient, err := New(okServer.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	done, err := okClient.RunRecipe(browser.WithTabID(context.Background(), "77"), recipe.RunRequest{
		ID: "billing.pay", Version: "1.0.0", Digest: strings.Repeat("d", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "done" || done.FailureBundle != "" {
		t.Fatalf("successful run result = %+v", done)
	}
}

// TestLongFailedRunStillCarriesTheEvidenceBundle bounds the fix. A recipe may
// declare up to 500 steps, and a refused body sized for an error STRING would
// truncate that result — and a truncated body is not decoded at all, so the
// bundle id would go missing on exactly the long runs most worth diagnosing.
func TestLongFailedRunStillCarriesTheEvidenceBundle(t *testing.T) {
	daemon := httpapi.New("", strictHandlerController{})
	daemon.SetRecipeAPI(longFailingRecipeAPI{steps: 500})
	server := httptest.NewServer(daemon.Handler())
	t.Cleanup(server.Close)

	client, err := New(server.URL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.RunRecipe(browser.WithTabID(context.Background(), "77"), recipe.RunRequest{
		ID: "billing.pay", Version: "1.0.0", Digest: strings.Repeat("d", 64),
	})
	if err == nil {
		t.Fatal("the run was supposed to fail")
	}
	if result.FailureBundle != failureBundleID {
		t.Fatalf("failure_bundle_artifact_id = %q after a 500-step run, want %q",
			result.FailureBundle, failureBundleID)
	}
	if len(result.Steps) != 500 {
		t.Fatalf("steps = %d, want the whole run detail decoded", len(result.Steps))
	}
}

type longFailingRecipeAPI struct {
	recipe.API
	steps int
}

func (a longFailingRecipeAPI) RunRecipe(_ context.Context, request recipe.RunRequest) (recipe.RunResult, error) {
	result := recipe.RunResult{
		RecipeID: request.ID, Status: "failed", FailureBundle: failureBundleID,
		Steps: make([]recipe.StepResult, 0, a.steps),
	}
	for index := range a.steps {
		result.Steps = append(result.Steps, recipe.StepResult{
			ID: fmt.Sprintf("step-with-a-realistically-long-identifier-%03d", index),
			// A completed step carries every field, so the body is as large as a
			// real long run makes it rather than as small as a stub makes it.
			Status: "done", Attempts: 2, DurationMS: 1234,
		})
	}
	result.Steps[a.steps-1].Status = "failed"
	return result, errors.New(`step "pay": element detached before the click landed; failure evidence bundle ` + failureBundleID)
}

type succeedingRecipeAPI struct{ recipe.API }

func (succeedingRecipeAPI) RunRecipe(_ context.Context, request recipe.RunRequest) (recipe.RunResult, error) {
	return recipe.RunResult{RecipeID: request.ID, Status: "done"}, nil
}
