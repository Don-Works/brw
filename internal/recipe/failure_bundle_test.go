package recipe

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/artifact"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// evidenceBrowser is a browser.Controller that resolves the recipe's target,
// fails the click, and then answers every question the evidence collection asks.
type evidenceBrowser struct {
	browser.Controller
	origin string
}

func (b *evidenceBrowser) DocumentIdentity(context.Context) (browser.DocumentIdentity, error) {
	return browser.DocumentIdentity{ID: "evidence-doc", Origin: b.origin}, nil
}

func (b *evidenceBrowser) Evaluate(context.Context, string) (any, error) { return b.origin, nil }

func (b *evidenceBrowser) Find(context.Context, snapshot.FindOptions) (snapshot.FindResult, error) {
	return snapshot.FindResult{Elements: []snapshot.Element{
		{Ref: "e1", Role: "button", Name: "Pay now", Visible: true},
	}}, nil
}

func (b *evidenceBrowser) Click(context.Context, string) (browser.ActionResult, error) {
	return browser.ActionResult{}, errors.New("element detached before the click landed")
}

func (b *evidenceBrowser) GetTrace() browser.TraceResult {
	return browser.TraceResult{Entries: []browser.TraceEntry{
		{Action: "click", Ref: "e1", OK: false, Error: "element detached before the click landed"},
	}, Count: 1}
}

func (b *evidenceBrowser) ConsoleMessages(context.Context) ([]browser.ConsoleMessage, error) {
	return []browser.ConsoleMessage{{Level: "error", Text: "payment widget threw"}}, nil
}

func (b *evidenceBrowser) NetworkCapture(context.Context, string) ([]snapshot.CapturedRequest, error) {
	return []snapshot.CapturedRequest{{
		Method: "POST", URL: b.origin + "/api/pay", Status: 502, Completed: true,
	}}, nil
}

func (b *evidenceBrowser) Snapshot(context.Context, snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	return snapshot.PageSnapshot{URL: b.origin + "/pay", Title: "Pay"}, nil
}

func (b *evidenceBrowser) Screenshot(context.Context) (browser.Screenshot, error) {
	return browser.Screenshot{MIMEType: "image/png", Data: []byte(
		"\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")}, nil
}

func failingRecipe(captureOnFailure bool) Recipe {
	return Recipe{
		SchemaVersion: SchemaVersion, ID: "billing.pay", Version: "1.0.0",
		Name: "Pay", Description: "Pay the outstanding balance",
		Intents: []string{"pay the balance"}, Origins: []string{"https://billing.example.test"},
		Risk: "read_only", CaptureOnFailure: captureOnFailure,
		Steps: []Step{{
			ID: "pay", Action: "click", Effect: "read",
			Target: &Target{Role: "button", Name: "Pay now"},
		}},
	}
}

func newEvidenceRunner(t *testing.T, policy artifact.FailureCapturePolicy) (Runner, *artifact.Store) {
	t.Helper()
	store, err := artifact.NewStore(artifact.Config{
		Root: t.TempDir(), MaxArtifactBytes: 1 << 20, MaxTotalBytes: 8 << 20, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := artifact.NewService(store, &evidenceBrowser{origin: "https://billing.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetFailureCapturePolicy(policy); err != nil {
		t.Fatal(err)
	}
	surface := &BrowserSurface{
		Browser:   &evidenceBrowser{origin: "https://billing.example.test"},
		Artifacts: service,
	}
	return Runner{Surface: surface, MaxDuration: time.Minute}, store
}

// TestFailedRunReportsOnlyTheManifestID runs a real recipe through the real
// runner against a browser whose click fails, and checks the shape of what comes
// back: one manifest id, in both the result and the error, and an error that
// grew by a handle rather than by a payload.
func TestFailedRunReportsOnlyTheManifestID(t *testing.T) {
	runner, store := newEvidenceRunner(t, artifact.FailureCaptureRecipe)

	baseline, baselineErr := runner.Run(context.Background(), failingRecipe(false), nil)
	if baselineErr == nil {
		t.Fatal("the recipe was supposed to fail")
	}
	if baseline.FailureBundle != "" {
		t.Fatalf("a recipe that did not opt in got a bundle: %q", baseline.FailureBundle)
	}

	result, err := runner.Run(context.Background(), failingRecipe(true), nil)
	if err == nil {
		t.Fatal("the recipe was supposed to fail")
	}
	if result.Status != "failed" || result.FailureBundle == "" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.HasPrefix(result.FailureBundle, "art_") {
		t.Fatalf("failure bundle id = %q", result.FailureBundle)
	}
	if !strings.Contains(err.Error(), result.FailureBundle) {
		t.Fatalf("error %q does not name the bundle %q", err, result.FailureBundle)
	}
	if !strings.Contains(err.Error(), "element detached") {
		t.Fatalf("the bundle replaced the real failure: %q", err)
	}

	// The evidence is reachable by id and nothing else: the error may not grow by
	// more than the handle. 500 tokens is roughly 2000 bytes of English; the
	// addition here is the id plus a short phrase.
	growth := len(err.Error()) - len(baselineErr.Error())
	if growth <= 0 || growth > 200 {
		t.Fatalf("error grew by %d bytes (from %q to %q)", growth, baselineErr, err)
	}

	manifest := readManifest(t, store, result.FailureBundle)
	if manifest.FailedStep != "pay" || manifest.RecipeID != "billing.pay" || manifest.RecipeVersion != "1.0.0" {
		t.Fatalf("manifest = %+v", manifest)
	}
	if len(manifest.Entries) == 0 {
		t.Fatalf("manifest names no evidence: %+v", manifest)
	}
	for _, entry := range manifest.Entries {
		if _, err := store.Info(entry.ArtifactID); err != nil {
			t.Fatalf("manifest entry %q does not resolve: %v", entry.Role, err)
		}
	}
}

// TestFailedRunWritesNothingWhenCaptureIsOff is the default. The daemon policy
// wins over the recipe's request, and nothing reaches disk.
func TestFailedRunWritesNothingWhenCaptureIsOff(t *testing.T) {
	runner, store := newEvidenceRunner(t, artifact.FailureCaptureOff)

	result, err := runner.Run(context.Background(), failingRecipe(true), nil)
	if err == nil {
		t.Fatal("the recipe was supposed to fail")
	}
	if result.FailureBundle != "" {
		t.Fatalf("capture-off run produced a bundle: %q", result.FailureBundle)
	}
	if strings.Contains(err.Error(), "art_") {
		t.Fatalf("capture-off error mentions an artifact: %q", err)
	}
	entries, readErr := os.ReadDir(store.Root())
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".blob") || strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("capture-off failure wrote %q", entry.Name())
		}
	}
}

// TestSucceedingRunCollectsNoEvidence keeps the cost where it belongs: a run
// that works must not pay for diagnostics it does not need.
func TestSucceedingRunCollectsNoEvidence(t *testing.T) {
	runner, store := newEvidenceRunner(t, artifact.FailureCaptureAll)
	value := failingRecipe(true)
	value.Steps = []Step{{ID: "wait", Action: "timer", TimerMS: 1}}

	result, err := runner.Run(context.Background(), value, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "done" || result.FailureBundle != "" {
		t.Fatalf("result = %+v", result)
	}
	entries, readErr := os.ReadDir(store.Root())
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".blob") {
			t.Fatalf("successful run wrote %q", entry.Name())
		}
	}
}

func readManifest(t *testing.T, store *artifact.Store, id string) artifact.Manifest {
	t.Helper()
	var raw []byte
	for offset := int64(0); ; {
		window, _, more, err := store.Read(id, offset, 64<<10)
		if err != nil {
			t.Fatalf("read manifest: %v", err)
		}
		raw = append(raw, window...)
		if !more {
			break
		}
		offset += int64(len(window))
	}
	var manifest artifact.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v", err)
	}
	return manifest
}
