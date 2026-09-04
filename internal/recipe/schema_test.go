package recipe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validRecipe(origin string) Recipe {
	visible := true
	return Recipe{
		SchemaVersion: SchemaVersion,
		ID:            "example.billing.download-invoices",
		Version:       "1.2.3",
		Name:          "Download billing invoices",
		Description:   "Download monthly statements from the billing portal.",
		Intents:       []string{"download invoices", "fetch billing receipts", "get monthly statements"},
		Origins:       []string{origin},
		Risk:          "read_only",
		Inputs:        map[string]Input{"month": {Required: true}},
		Steps: []Step{
			{ID: "choose_month", Action: "fill", Effect: "read", Target: &Target{Role: "textbox", Name: "Month", Visible: &visible}, Value: "${input:month}"},
			{ID: "download", Action: "click", Effect: "read", Target: &Target{Role: "button", TestID: "download-invoices", Visible: &visible}, Postcondition: &Event{Kind: "download.completed", Match: "invoices.zip", TimeoutMS: 10_000}},
			{ID: "await_ready", Action: "wait_event", Event: &Event{Kind: "page.ready", TimeoutMS: 10_000}},
			{ID: "save_text", Action: "capture", Capture: &CaptureSpec{Kind: "text", Redaction: "standard"}},
		},
	}
}

func TestValidateAcceptsStructuredRefFreeRecipe(t *testing.T) {
	if err := Validate(validRecipe("https://billing.example.test")); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAcceptsTemplatedTargetsAndExactElementValues(t *testing.T) {
	value := validRecipe("https://billing.example.test")
	value.Risk = "external_write"
	value.Inputs = map[string]Input{
		"person":  {Required: true},
		"message": {Required: true},
	}
	value.Steps = []Step{
		{
			ID: "prepare", Action: "fill", Value: "${input:message}",
			Target:         &Target{Role: "textbox", NameContains: "${input:person}"},
			Effect:         "external_write",
			IdempotencyKey: "draft:${input:person}:${input:message}",
			Postcondition: &Event{
				Kind: "element.value", Match: "${input:message}", TimeoutMS: 1_000,
				Target: &Target{Role: "textbox", NameContains: "${input:person}"},
			},
		},
		{
			ID: "send", Action: "press", Key: "Enter",
			Target:         &Target{Role: "textbox", NameContains: "${input:person}"},
			Effect:         "external_write",
			IdempotencyKey: "send-current-draft:${input:person}",
			Postcondition: &Event{
				Kind: "element.value", Match: "", TimeoutMS: 1_000,
				Target: &Target{Role: "textbox", NameContains: "${input:person}"},
			},
		},
		{
			ID: "capture", Action: "capture",
			Capture: &CaptureSpec{Kind: "screenshot", Target: &Target{Role: "region", NameContains: "${input:person}"}},
		},
	}
	if err := Validate(value); err != nil {
		t.Fatal(err)
	}

	value.Steps[0].Target.NameContains = "${input:undeclared}"
	if err := Validate(value); err == nil || !strings.Contains(err.Error(), "undeclared") {
		t.Fatalf("undeclared target template error = %v", err)
	}
}

func TestValidateElementValueContainsRequiresContent(t *testing.T) {
	value := validRecipe("https://billing.example.test")
	value.Steps[2].Event = &Event{
		Kind: "element.value_contains", Match: "${input:month}", TimeoutMS: 100,
		Target: &Target{Role: "textbox", Name: "Month"},
	}
	if err := Validate(value); err != nil {
		t.Fatal(err)
	}
	value.Steps[2].Event.Match = ""
	if err := Validate(value); err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("empty value_contains error=%v", err)
	}
}

func TestValidateRequiresExplicitConsistentEffects(t *testing.T) {
	target := func() *Target { return &Target{Role: "textbox", Name: "Month"} }
	steps := []Step{
		{ID: "click", Action: "click", Target: target()},
		{ID: "fill", Action: "fill", Target: target(), Value: "May"},
		{ID: "type", Action: "type", Target: target(), Value: "May"},
		{ID: "select", Action: "select", Target: target(), Value: "May"},
		{ID: "press", Action: "press", Target: target(), Key: "Enter"},
		{ID: "navigate", Action: "navigate_to", URL: "https://billing.example.test/invoices"},
	}
	for _, step := range steps {
		t.Run("omitted_"+step.Action, func(t *testing.T) {
			value := validRecipe("https://billing.example.test")
			value.Inputs = nil
			value.Steps = []Step{step}
			err := Validate(value)
			if err == nil || !strings.Contains(err.Error(), "explicitly declare effect") {
				t.Fatalf("Validate error=%v", err)
			}
			data, marshalErr := json.Marshal(value)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if _, parseErr := Parse(data); parseErr == nil || !strings.Contains(parseErr.Error(), "explicitly declare effect") {
				t.Fatalf("schema-v1 Parse error=%v", parseErr)
			}
		})
	}

	t.Run("unknown_effect", func(t *testing.T) {
		value := validRecipe("https://billing.example.test")
		value.Steps[0].Effect = "write"
		if err := Validate(value); err == nil || !strings.Contains(err.Error(), "effect must be read or external_write") {
			t.Fatalf("Validate error=%v", err)
		}
	})

	t.Run("write_effect_under_read_only_risk", func(t *testing.T) {
		value := validRecipe("https://billing.example.test")
		value.Steps[1].Effect = "external_write"
		value.Steps[1].IdempotencyKey = "download:${input:month}"
		value.Steps[1].Postcondition = &Event{Kind: "text.present", Match: "Download started", TimeoutMS: 1_000}
		if err := Validate(value); err == nil || !strings.Contains(err.Error(), "recipe risk external_write") {
			t.Fatalf("Validate error=%v", err)
		}
	})

	t.Run("effect_on_non_browser_action", func(t *testing.T) {
		value := validRecipe("https://billing.example.test")
		value.Steps = []Step{{ID: "wait", Action: "timer", TimerMS: 1, Effect: "read"}}
		if err := Validate(value); err == nil || !strings.Contains(err.Error(), "only valid on browser actions") {
			t.Fatalf("Validate error=%v", err)
		}
	})
}

func TestValidateRejectsUnsafeOrNondeterministicForms(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Recipe)
		want   string
	}{
		{"schema", func(r *Recipe) { r.SchemaVersion = 99 }, "schema_version"},
		{"unnamespaced id", func(r *Recipe) { r.ID = "download" }, "namespaced"},
		{"version", func(r *Recipe) { r.Version = "latest" }, "semantic"},
		{"wildcard origin", func(r *Recipe) { r.Origins = []string{"https://*.example.test"} }, "exact"},
		{"insecure origin", func(r *Recipe) { r.Origins = []string{"http://billing.example.test"} }, "HTTPS"},
		{"raw ref", func(r *Recipe) { r.Steps[1].Target = &Target{Role: "button", Name: "e17"} }, "observation refs"},
		{"weak target", func(r *Recipe) { r.Steps[1].Target = &Target{Role: "button"} }, "accessible name"},
		{"undeclared input", func(r *Recipe) { r.Steps[0].Value = "${input:account}" }, "undeclared"},
		{"invalid template", func(r *Recipe) { r.Steps[0].Value = "${secret:account}" }, "invalid template"},
		{"unknown event", func(r *Recipe) { r.Steps[2].Event.Kind = "hope.it.finished" }, "unsupported event"},
		{"unbounded event", func(r *Recipe) { r.Steps[2].Event.TimeoutMS = 0 }, "timeout_ms"},
		{"transient standalone event", func(r *Recipe) {
			r.Steps[2].Event = &Event{Kind: "tab.opened", Match: "report", TimeoutMS: 1000}
		}, "must be a postcondition"},
		{"ignored event target", func(r *Recipe) {
			r.Steps[2].Event.Target = &Target{Role: "button", Name: "Ignored"}
		}, "only valid for element events"},
		{"long timer", func(r *Recipe) { r.Steps = append(r.Steps, Step{ID: "sleep", Action: "timer", TimerMS: 60_001}) }, "timer_ms"},
		{"write without recipe risk", func(r *Recipe) {
			r.Steps[1].Effect = "external_write"
			r.Steps[1].IdempotencyKey = "download:${input:month}"
			r.Steps[1].Postcondition = &Event{Kind: "text.present", Match: "started", TimeoutMS: 1000}
		}, "recipe risk"},
		{"retry without guard", func(r *Recipe) { r.Steps[1].MaxAttempts = 2 }, "idempotency_key"},
		{"automatic retry of external write", func(r *Recipe) {
			r.Risk = "external_write"
			r.Steps[1].Effect = "external_write"
			r.Steps[1].MaxAttempts = 2
			r.Steps[1].IdempotencyKey = "download:${input:month}"
		}, "may not be automatically retried"},
		{"generic ready cannot prove a write", func(r *Recipe) {
			r.Risk = "external_write"
			r.Steps[1].Effect = "external_write"
			r.Steps[1].IdempotencyKey = "download:${input:month}"
			r.Steps[1].Postcondition = &Event{Kind: "page.ready", TimeoutMS: 1000}
		}, "cannot prove an external write"},
		{"transient event cannot deduplicate a write", func(r *Recipe) {
			r.Risk = "external_write"
			r.Steps[1].Effect = "external_write"
			r.Steps[1].IdempotencyKey = "download:${input:month}"
			r.Steps[1].Postcondition = &Event{Kind: "network.response", Match: "/commit", TimeoutMS: 1000}
		}, "durable state postcondition"},
		{"negative attempts", func(r *Recipe) { r.Steps[1].MaxAttempts = -1 }, "max_attempts"},
		{"invalid optional postcondition", func(r *Recipe) {
			r.Steps[1].Postcondition = &Event{Kind: "element.visible", TimeoutMS: 100}
		}, "invalid postcondition"},
		{"element value without target", func(r *Recipe) {
			r.Steps[2].Event = &Event{Kind: "element.value", Match: "ready", TimeoutMS: 100}
		}, "requires a target"},
		{"templated role", func(r *Recipe) {
			r.Steps[0].Target.Role = "${input:month}"
		}, "role may not interpolate"},
		{"ignored action field", func(r *Recipe) { r.Steps[3].Key = "Enter" }, "only valid for press"},
		{"oversized key", func(r *Recipe) {
			r.Steps = []Step{{ID: "press", Action: "press", Key: strings.Repeat("x", 101), Target: &Target{Role: "textbox", Name: "Month"}}}
		}, "key is too long"},
		{"retry on timer", func(r *Recipe) {
			r.Steps = append(r.Steps, Step{ID: "retry_timer", Action: "timer", TimerMS: 10, MaxAttempts: 2})
		}, "only valid on browser actions"},
		{"literal secret", func(r *Recipe) { r.Metadata = map[string]string{"note": "api_key=supersecretvalue"} }, "literal secret"},
		{"video without bound", func(r *Recipe) { r.Steps[3].Capture = &CaptureSpec{Kind: "video"} }, "video capture"},
		{"video frame bomb", func(r *Recipe) {
			r.Steps[3].Capture = &CaptureSpec{Kind: "video", DurationMS: 30_000, FPS: 30}
		}, "300 frames"},
		{"ephemeral capture ref", func(r *Recipe) { r.Steps[3].Capture.Ref = "e17" }, "ephemeral"},
		{"capture field confusion", func(r *Recipe) {
			r.Steps[3].Capture.DownloadGUID = "download-guid"
		}, "only valid for download"},
		{"foreign navigate", func(r *Recipe) { r.Steps = []Step{{ID: "go", Action: "navigate_to", URL: "https://evil.example.test"}} }, "allowlist"},
		{"credentialed navigate", func(r *Recipe) {
			credentialedURL := strings.Join([]string{"https://user:credential", "billing.example.test/path"}, "@")
			r.Steps = []Step{{ID: "go", Action: "navigate_to", URL: credentialedURL}}
		}, "absolute URL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := validRecipe("https://billing.example.test")
			test.mutate(&value)
			err := Validate(value)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want=%q", err, test.want)
			}
		})
	}
}

func TestGuardedWriteAndStrictParseDigest(t *testing.T) {
	value := validRecipe("https://billing.example.test")
	value.Risk = "external_write"
	value.Steps[1].Effect = "external_write"
	value.Steps[1].MaxAttempts = 1
	value.Steps[1].IdempotencyKey = "download:${input:month}"
	value.Steps[1].Postcondition = &Event{Kind: "text.present", Match: "Download started", TimeoutMS: 2_000}
	if err := Validate(value); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(value)
	withUnknown := append(append([]byte(nil), data[:len(data)-1]...), []byte(`,"surprise":true}`)...)
	if _, err := Parse(withUnknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error=%v", err)
	}
	if _, err := Parse(append(data, []byte(` {}`)...)); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing error=%v", err)
	}
	before, _ := Digest(value)
	value.Steps[1].Target.TestID = "send-money"
	after, _ := Digest(value)
	if before == after {
		t.Fatal("digest did not detect executable change")
	}
}

func TestParseRejectsOversizedRawDocumentBeforeDecode(t *testing.T) {
	value := validRecipe("https://billing.example.test")
	data, _ := json.Marshal(value)
	data = append(data, bytes.Repeat([]byte(" "), (1<<20)-len(data)+1)...)
	if _, err := Parse(data); err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("oversized parse error = %v", err)
	}
}

func TestDirectoryCatalogStaysOutsideRepositoryAndFetchesExactDigest(t *testing.T) {
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateRoot(filepath.Join(repository, "private"), repository); err == nil {
		t.Fatal("recipe root inside repository accepted")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	value := validRecipe("https://billing.example.test")
	data, _ := json.Marshal(value)
	if err := os.WriteFile(filepath.Join(root, "invoice.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadDirectory(context.Background(), DirectoryConfig{Root: root, RepositoryRoot: repository})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := catalog.Search(context.Background(), "download monthly billing invoices", "https://billing.example.test", 5)
	if err != nil || len(matches) != 1 || matches[0].ID != value.ID {
		t.Fatalf("matches=%+v err=%v", matches, err)
	}
	fetched, err := catalog.Fetch(context.Background(), matches[0].ID, matches[0].Version, matches[0].Digest)
	if err != nil || fetched.ID != value.ID {
		t.Fatalf("fetch=%+v err=%v", fetched, err)
	}
	if _, err := catalog.Fetch(context.Background(), matches[0].ID, matches[0].Version, strings.Repeat("0", 64)); err == nil {
		t.Fatal("digest mismatch accepted")
	}
}

func TestDirectoryProviderHotReloadsAndSearchesOnlyLatestVersion(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	provider, err := NewDirectoryProvider(context.Background(), DirectoryConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := provider.Search(context.Background(), "download invoices", "", 10)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty search=%+v err=%v", empty, err)
	}

	oldRecipe := validRecipe("https://billing.example.test")
	oldRecipe.Version = "1.9.0"
	oldData, _ := json.Marshal(oldRecipe)
	if err := os.WriteFile(filepath.Join(root, "old.json"), oldData, 0o600); err != nil {
		t.Fatal(err)
	}
	oldMatches, err := provider.Search(context.Background(), "download invoices", "", 10)
	if err != nil || len(oldMatches) != 1 || oldMatches[0].Version != "1.9.0" {
		t.Fatalf("old matches=%+v err=%v", oldMatches, err)
	}

	newRecipe := oldRecipe
	newRecipe.Version = "1.10.0"
	newRecipe.Description = "Updated semantic targets for the billing portal."
	newData, _ := json.Marshal(newRecipe)
	if err := os.WriteFile(filepath.Join(root, "new.json"), newData, 0o600); err != nil {
		t.Fatal(err)
	}
	newMatches, err := provider.Search(context.Background(), "download invoices", "", 10)
	if err != nil || len(newMatches) != 1 || newMatches[0].Version != "1.10.0" {
		t.Fatalf("new matches=%+v err=%v", newMatches, err)
	}
	// Updating the searchable head does not invalidate an already-pinned older
	// version; immutable fetch remains available by its original digest.
	fetched, err := provider.Fetch(context.Background(), oldMatches[0].ID, oldMatches[0].Version, oldMatches[0].Digest)
	if err != nil || fetched.Version != "1.9.0" {
		t.Fatalf("old pinned fetch=%+v err=%v", fetched, err)
	}
}

func TestSemanticVersionOrderingForRecipeRepairs(t *testing.T) {
	tests := []struct {
		left, right string
		want        int
	}{
		{"1.10.0", "1.9.9", 1},
		{"2.0.0", "2.0.0-rc.2", 1},
		{"2.0.0-rc.10", "2.0.0-rc.2", 1},
		{"2.0.0-alpha", "2.0.0-beta", -1},
		{"2.0.0-rc.01", "2.0.0-rc.1", 0},
		{"1.0.0+build.2", "1.0.0+build.1", 0},
	}
	for _, test := range tests {
		got := compareSemanticVersions(test.left, test.right)
		if got < 0 {
			got = -1
		} else if got > 0 {
			got = 1
		}
		if got != test.want {
			t.Errorf("compareSemanticVersions(%q, %q)=%d want %d", test.left, test.right, got, test.want)
		}
	}
}

func TestDirectoryCatalogRejectsBroadRootsAndExposedNestedDirectories(t *testing.T) {
	if _, err := LoadDirectory(context.Background(), DirectoryConfig{Root: string(filepath.Separator)}); err == nil || !strings.Contains(err.Error(), "too broad") {
		t.Fatalf("filesystem recipe root error = %v", err)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "exposed")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDirectory(context.Background(), DirectoryConfig{Root: root}); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("exposed nested directory error = %v", err)
	}
}

func TestDirectoryCatalogRejectsAnyGitCheckoutWithoutRepositoryHint(t *testing.T) {
	repository := t.TempDir()
	if err := os.Mkdir(filepath.Join(repository, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(repository, "private", "recipes")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDirectory(context.Background(), DirectoryConfig{Root: root}); err == nil || !strings.Contains(err.Error(), "git checkout") {
		t.Fatalf("recipe root inside an arbitrary checkout error=%v", err)
	}
	prospective := filepath.Join(repository, "not-created", "recipes")
	if err := PreparePrivateDirectory(prospective); err == nil || !strings.Contains(err.Error(), "git checkout") {
		t.Fatalf("prospective recipe root inside checkout error=%v", err)
	}
	if _, err := os.Stat(prospective); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected prospective root was created: %v", err)
	}
}

func TestDirectoryCatalogRejectsNestedGitCheckoutInsidePrivateRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "copied-worktree")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(nested, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDirectory(context.Background(), DirectoryConfig{Root: root}); err == nil || !strings.Contains(err.Error(), "nested git checkout") {
		t.Fatalf("nested checkout inside private root error=%v", err)
	}
	if _, err := NewDirectoryProvider(context.Background(), DirectoryConfig{Root: root}); err == nil || !strings.Contains(err.Error(), "nested git checkout") {
		t.Fatalf("live provider accepted nested checkout: %v", err)
	}
}

func TestEnsureOutsideGitCheckoutAcceptsOrdinaryPrivateFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "draft.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureOutsideGitCheckout(path); err != nil {
		t.Fatalf("ordinary private file rejected: %v", err)
	}
}

func TestPreparePrivateDirectoryRejectsSymlinkWithoutChangingTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "recipes")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := PreparePrivateDirectory(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink recipe root error=%v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("rejected symlink changed target permissions to %o", got)
	}
}

func TestCatalogIndexesCandidatesDeterministicallyAndReturnsImmutableRecipes(t *testing.T) {
	recipes := make([]Recipe, 100)
	for index := range recipes {
		value := validRecipe("https://catalog.example.test")
		value.ID = fmt.Sprintf("example.catalog.recipe-%03d", index)
		value.Name = fmt.Sprintf("Common lookup %03d", index)
		value.Description = "Indexed common lookup automation."
		value.Intents = []string{"common lookup"}
		recipes[index] = value
	}
	catalog, err := NewCatalog(context.Background(), recipes, nil)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := catalog.Search(context.Background(), "common lookup", "https://catalog.example.test", 5)
	if err != nil || len(matches) != 5 {
		t.Fatalf("matches=%+v err=%v", matches, err)
	}
	for index, match := range matches {
		want := fmt.Sprintf("example.catalog.recipe-%03d", index)
		if match.ID != want {
			t.Fatalf("match %d = %q, want %q", index, match.ID, want)
		}
	}

	first, err := catalog.Fetch(context.Background(), matches[0].ID, matches[0].Version, matches[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	first.Origins[0] = "https://mutated.example.test"
	first.Steps[0].Target.Name = "Mutated"
	first.Inputs["month"] = Input{Description: "Mutated"}
	again, err := catalog.Fetch(context.Background(), matches[0].ID, matches[0].Version, matches[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	if again.Origins[0] != "https://catalog.example.test" || again.Steps[0].Target.Name != "Month" || again.Inputs["month"].Description != "" {
		t.Fatalf("fetched recipe shared mutable catalog state: %+v", again)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := catalog.Search(cancelled, "common lookup", "", 5); err == nil {
		t.Fatal("cancelled search did not stop")
	}
}

func TestCatalogFetchDeepClonesCaptureTargets(t *testing.T) {
	visible := true
	value := validRecipe("https://billing.example.test")
	value.Steps[3].Capture = &CaptureSpec{
		Kind:   "screenshot",
		Target: &Target{Role: "region", Name: "Invoice results", Visible: &visible},
	}
	catalog, err := NewCatalog(context.Background(), []Recipe{value}, nil)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	first, err := catalog.Fetch(context.Background(), value.ID, value.Version, digest)
	if err != nil {
		t.Fatal(err)
	}
	first.Steps[3].Capture.Target.Name = "Mutated target"
	*first.Steps[3].Capture.Target.Visible = false

	again, err := catalog.Fetch(context.Background(), value.ID, value.Version, digest)
	if err != nil {
		t.Fatal(err)
	}
	if again.Steps[3].Capture.Target.Name != "Invoice results" || again.Steps[3].Capture.Target.Visible == nil || !*again.Steps[3].Capture.Target.Visible {
		t.Fatalf("capture target shared mutable catalog state: %+v", again.Steps[3].Capture.Target)
	}
}

type conceptEmbedder struct{}

func (conceptEmbedder) Embed(_ context.Context, text string) ([]float64, error) {
	text = strings.ToLower(text)
	vector := make([]float64, 4)
	for index, words := range [][]string{{"invoice", "bill", "statement"}, {"download", "fetch", "get"}, {"receipt", "purchase"}, {"upload", "attach", "submit"}} {
		for _, word := range words {
			if strings.Contains(text, word) {
				vector[index]++
			}
		}
	}
	return vector, nil
}

func TestPluggableSemanticEmbedderRanksParaphrase(t *testing.T) {
	invoice := validRecipe("https://billing.example.test")
	receipt := validRecipe("https://expenses.example.test")
	receipt.ID = "example.expenses.upload-receipts"
	receipt.Name = "Upload expense receipts"
	receipt.Description = "Attach purchase receipts to an expense report."
	receipt.Intents = []string{"submit expenses", "upload purchase receipts"}
	catalog, err := NewCatalog(context.Background(), []Recipe{receipt, invoice}, conceptEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	matches, err := catalog.Search(context.Background(), "get my bills from the account portal", "", 2)
	if err != nil || len(matches) < 1 || matches[0].ID != invoice.ID {
		t.Fatalf("matches=%+v err=%v", matches, err)
	}
}

func TestHTTPProviderPinsIdentityAndNeverAcceptsSubstitution(t *testing.T) {
	value := validRecipe("https://billing.example.test")
	digest, _ := Digest(value)
	providerToken := strings.Join([]string{"test", "token"}, "-")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+providerToken {
			t.Error("missing provider authorization")
		}
		switch r.URL.Path {
		case "/v1/recipes/search":
			_ = json.NewEncoder(w).Encode(map[string]any{"matches": []Match{{
				ID: value.ID, Version: value.Version, Name: value.Name, Description: value.Description,
				Origins: value.Origins, Risk: value.Risk, Digest: digest, Score: 1,
			}}})
		case "/v1/recipes/fetch":
			_ = json.NewEncoder(w).Encode(map[string]any{"recipe": value})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider, err := NewHTTPProvider(HTTPProviderConfig{BaseURL: server.URL, Token: providerToken})
	if err != nil {
		t.Fatal(err)
	}
	if matches, err := provider.Search(context.Background(), "invoice", "", 1); err != nil || len(matches) != 1 {
		t.Fatalf("search=%+v err=%v", matches, err)
	}
	if _, err := provider.Fetch(context.Background(), value.ID, value.Version, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Fetch(context.Background(), value.ID, value.Version, strings.Repeat("0", 64)); err == nil {
		t.Fatal("substituted digest accepted")
	}
	if _, err := provider.Fetch(context.Background(), "../../untrusted", value.Version, digest); err == nil || !strings.Contains(err.Error(), "invalid recipe identity") {
		t.Fatalf("invalid provider identity was accepted: %v", err)
	}
	if _, err := NewHTTPProvider(HTTPProviderConfig{BaseURL: "http://recipes.example.test"}); err == nil {
		t.Fatal("non-loopback plaintext provider accepted")
	}
}

func TestHTTPProviderRejectsConflictingDigestsForOneVersion(t *testing.T) {
	value := validRecipe("https://billing.example.test")
	digest, err := Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	match := Match{
		ID: value.ID, Version: value.Version, Name: value.Name, Description: value.Description,
		Origins: value.Origins, Risk: value.Risk, Digest: digest, Score: 1,
	}
	conflict := match
	conflict.Digest = strings.Repeat("f", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"matches": []Match{match, conflict}})
	}))
	defer server.Close()
	provider, err := NewHTTPProvider(HTTPProviderConfig{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Search(context.Background(), "invoice", "", 2); err == nil || !strings.Contains(err.Error(), "duplicate match") {
		t.Fatalf("conflicting id@version digests were accepted: %v", err)
	}
}

func TestHTTPProviderRejectsUnsafeSearchMetadata(t *testing.T) {
	value := validRecipe("https://billing.example.test")
	digest, _ := Digest(value)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"matches": []Match{{
			ID: value.ID, Version: value.Version, Name: value.Name,
			Description: "token=should-never-cross-search", Origins: value.Origins,
			Risk: value.Risk, Digest: digest, Score: 1,
		}}})
	}))
	defer server.Close()
	provider, err := NewHTTPProvider(HTTPProviderConfig{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Search(context.Background(), "invoice", "", 1); err == nil || !strings.Contains(err.Error(), "disclosure-safe") {
		t.Fatalf("unsafe provider metadata accepted: %v", err)
	}
}

func TestHTTPProviderHasBoundedRequestTimeoutWithCustomClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{"matches": []Match{}})
	}))
	defer server.Close()
	provider, err := NewHTTPProvider(HTTPProviderConfig{
		BaseURL: server.URL, Client: server.Client(), RequestTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = provider.Search(context.Background(), "invoice", "", 1)
	if err == nil || time.Since(started) > 80*time.Millisecond {
		t.Fatalf("timeout err=%v elapsed=%s", err, time.Since(started))
	}
}

func TestHTTPProviderRefusesRedirectsBeforeForwardingAuthorization(t *testing.T) {
	reachedTarget := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reachedTarget = true
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	providerToken := strings.Join([]string{"synthetic", "credential"}, "-")
	provider, err := NewHTTPProvider(HTTPProviderConfig{BaseURL: redirect.URL, Token: providerToken})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Search(context.Background(), "invoice", "", 1); err == nil || !strings.Contains(err.Error(), "redirects are not allowed") {
		t.Fatalf("provider redirect was not rejected: %v", err)
	}
	if reachedTarget {
		t.Fatal("provider followed redirect to the substituted endpoint")
	}
	if _, err := NewHTTPProvider(HTTPProviderConfig{BaseURL: redirect.URL + "?route=other"}); err == nil {
		t.Fatal("provider URL with query was accepted")
	}
}

func FuzzParseNeverPanics(f *testing.F) {
	valid, _ := json.Marshal(validRecipe("https://billing.example.test"))
	f.Add(valid)
	f.Add([]byte(`{"schema_version":1,"id":"e17"}`))
	f.Add([]byte{0, 1, 2, 255})
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = Parse(data) })
}
