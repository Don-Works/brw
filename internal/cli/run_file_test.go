package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/recipe"
)

func TestRunFileForwardsRecipeAndReportsDigest(t *testing.T) {
	isolateLocks(t)
	value := recipe.Recipe{
		SchemaVersion: 1, ID: "fixture.inline", Version: "1.0.0",
		Name: "Fixture", Description: "PRIVATE_RECIPE_BODY", Intents: []string{"test inline"},
		Origins: []string{"https://example.test"}, Risk: "read_only",
		Inputs: map[string]recipe.Input{"month": {Required: true}},
		Steps:  []recipe.Step{{ID: "pause", Action: "timer", TimerMS: 1}},
	}
	body, _ := json.Marshal(value)
	path := filepath.Join(t.TempDir(), "recipe.json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	digest, _ := recipe.Digest(value)
	daemon := newRunDaemon(t, &runDaemon{respond: func(w http.ResponseWriter, body []byte) {
		var request recipe.RunRequest
		if err := json.Unmarshal(body, &request); err != nil || request.Recipe == nil || request.Recipe.ID != value.ID || request.ID != "" || request.Version != "" || request.Digest != "" || request.Inputs["month"] != "private-month" {
			t.Errorf("request: %s, %v", body, err)
		}
		json.NewEncoder(w).Encode(recipe.RunResult{RecipeID: value.ID, RecipeVersion: value.Version, RecipeDigest: digest, Status: "done"})
	}})
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"run", "--file", path, "--input", "month=private-month", "--daemon", daemon.server.URL}, &stdout, &stderr)
	var report runReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || code != ExitOK || report.Recipe.Digest != digest || report.Recipe.ID != value.ID {
		t.Fatalf("code=%d stdout=%s stderr=%s err=%v", code, &stdout, &stderr, err)
	}
	for _, secret := range []string{"PRIVATE_RECIPE_BODY", "private-month"} {
		if strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatal("private recipe content echoed")
		}
	}
}

func TestRunFileRefusesMixedAndInvalidSources(t *testing.T) {
	for _, test := range []struct {
		name  string
		body  string
		extra []string
	}{
		{name: "id", body: `{}`, extra: []string{"fixture.inline"}},
		{name: "version", body: `{}`, extra: []string{"--recipe-version", "1.0.0"}},
		{name: "digest", body: `{}`, extra: []string{"--digest", strings.Repeat("a", 64)}},
		{name: "unknown", body: `{"unknown":true}`},
		{name: "trailing", body: `{} {}`},
		{name: "oversized", body: strings.Repeat(" ", (1<<20)+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "recipe.json")
			if err := os.WriteFile(path, []byte(test.body), 0600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			args := append([]string{"run", "--file", path, "--daemon", "http://127.0.0.1:1"}, test.extra...)
			if code := Run(context.Background(), args, &stdout, &stderr); code != ExitUsage {
				t.Fatalf("exit=%d stdout=%s stderr=%s", code, &stdout, &stderr)
			}
		})
	}
}
