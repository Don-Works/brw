package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/snapshot"
)

// bundleFakeBrowser answers every question a failure bundle asks. It carries a
// page sentinel in each part so a test can prove the manifest never inlines one.
type bundleFakeBrowser struct {
	browser.Controller
	trace    browser.TraceResult
	console  []browser.ConsoleMessage
	network  []snapshot.CapturedRequest
	page     snapshot.PageSnapshot
	shot     browser.Screenshot
	origin   string
	consoleN int
	networkN int
}

func (f *bundleFakeBrowser) GetTrace() browser.TraceResult { return f.trace }

func (f *bundleFakeBrowser) ConsoleMessages(context.Context) ([]browser.ConsoleMessage, error) {
	f.consoleN++
	return append([]browser.ConsoleMessage(nil), f.console...), nil
}

func (f *bundleFakeBrowser) NetworkCapture(context.Context, string) ([]snapshot.CapturedRequest, error) {
	f.networkN++
	out := make([]snapshot.CapturedRequest, len(f.network))
	for index, item := range f.network {
		out[index] = item
		out[index].RequestHeaders = map[string]string{}
		for name, value := range item.RequestHeaders {
			out[index].RequestHeaders[name] = value
		}
	}
	return out, nil
}

func (f *bundleFakeBrowser) Snapshot(context.Context, snapshot.SnapshotOptions) (snapshot.PageSnapshot, error) {
	return f.page, nil
}

func (f *bundleFakeBrowser) Screenshot(context.Context) (browser.Screenshot, error) {
	return f.shot, nil
}

func (f *bundleFakeBrowser) Evaluate(context.Context, string) (any, error) { return f.origin, nil }

func (f *bundleFakeBrowser) DocumentIdentity(context.Context) (browser.DocumentIdentity, error) {
	return browser.DocumentIdentity{ID: "bundle-doc", Origin: f.origin}, nil
}

const (
	bundlePageSentinel      = "PAGE-BODY-SENTINEL"
	bundleCredentialValue   = "fixture-authorization-value-one"
	bundleBenignHeaderName  = "X-Request-Id"
	bundleBenignHeaderValue = "fixture-request-id-one"
)

func newBundleService(t *testing.T, store *Store, policy FailureCapturePolicy) (*Service, *bundleFakeBrowser) {
	t.Helper()
	fake := &bundleFakeBrowser{
		origin: "https://billing.example.test",
		trace: browser.TraceResult{Entries: []browser.TraceEntry{
			{Action: "open", OK: true, TabID: "tab-a", Text: bundlePageSentinel},
			{Action: "fill", OK: true, TabID: "tab-a", Redacted: true, Value: bundleCredentialValue},
			{Action: "click", OK: false, TabID: "tab-a", Error: "element not found"},
			{Action: "click", OK: true, TabID: "tab-other"},
		}, Count: 4},
		console: []browser.ConsoleMessage{
			{Level: "error", Text: "Uncaught TypeError: cannot read properties of null"},
			{Level: "warn", Text: "deprecated api"},
			{Level: "error", Text: "second failure"},
		},
		network: []snapshot.CapturedRequest{{
			Method: "POST", URL: "https://billing.example.test/api/pay", Status: 500,
			Completed: true, DurationMS: 42,
			RequestHeaders: map[string]string{
				"Authorization":        bundleCredentialValue,
				bundleBenignHeaderName: bundleBenignHeaderValue,
			},
			RequestBody:     "account=" + bundlePageSentinel,
			ResponseSnippet: bundlePageSentinel,
		}},
		page: snapshot.PageSnapshot{URL: "https://billing.example.test/pay", Title: bundlePageSentinel},
		shot: browser.Screenshot{MIMEType: "image/png", Data: onePixelPNG(t)},
	}
	service, err := NewService(store, fake)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetFailureCapturePolicy(policy); err != nil {
		t.Fatal(err)
	}
	return service, fake
}

func readWholeArtifact(t *testing.T, store *Store, id string) []byte {
	t.Helper()
	var out []byte
	for offset := int64(0); ; {
		window, _, more, err := store.Read(id, offset, 64<<10)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		out = append(out, window...)
		if !more {
			return out
		}
		offset += int64(len(window))
	}
}

// TestFailureBundleReturnsOneManifestOfResolvableParts is the acceptance path:
// one forced failure produces exactly one manifest, the manifest holds ids and
// no payload, every id it names resolves, and every part expires on its own TTL.
func TestFailureBundleReturnsOneManifestOfResolvableParts(t *testing.T) {
	store := newTestStore(t, 1<<20, 8<<20)
	service, _ := newBundleService(t, store, FailureCaptureRecipe)
	if err := service.SetFailureBundleTTL(10 * time.Minute); err != nil {
		t.Fatal(err)
	}
	ctx := browser.WithTabID(
		browser.WithAllowedOrigins(context.Background(), []string{"https://billing.example.test"}), "tab-a")

	meta, err := service.CaptureFailureBundle(ctx, FailureBundleOptions{
		Reason: `step "pay": click failed`, RecipeID: "billing.pay", RecipeVersion: "1.0.0",
		FailedStep: "pay", RecipeOptIn: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Kind != "manifest" || meta.MIMEType != "application/json" {
		t.Fatalf("manifest meta = %+v", meta)
	}

	raw := readWholeArtifact(t, store, meta.ID)
	if bytes.Contains(raw, []byte(bundlePageSentinel)) || bytes.Contains(raw, []byte(bundleCredentialValue)) {
		t.Fatalf("manifest inlined payload: %s", raw)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != ManifestSchemaVersion || manifest.FailedStep != "pay" || manifest.RecipeID != "billing.pay" {
		t.Fatalf("manifest = %+v", manifest)
	}
	if len(manifest.Missing) != 0 {
		t.Fatalf("bundle lost parts: %+v", manifest.Missing)
	}

	wantRoles := []string{"action_trace", "console", "network", "semantic_snapshot", "screenshot"}
	if len(manifest.Entries) != len(wantRoles) {
		t.Fatalf("entries = %d, want %d: %+v", len(manifest.Entries), len(wantRoles), manifest.Entries)
	}
	for index, entry := range manifest.Entries {
		if entry.Role != wantRoles[index] {
			t.Fatalf("entry %d role = %q, want %q", index, entry.Role, wantRoles[index])
		}
		part, err := service.ArtifactInfo(ctx, entry.ArtifactID)
		if err != nil {
			t.Fatalf("manifest entry %q does not resolve: %v", entry.Role, err)
		}
		if part.SizeBytes == 0 {
			t.Fatalf("manifest entry %q is empty", entry.Role)
		}
		if !part.ExpiresAt.Equal(part.CreatedAt.Add(10 * time.Minute)) {
			t.Fatalf("entry %q expiry = %s, created %s; want a 10m evidence TTL", entry.Role, part.ExpiresAt, part.CreatedAt)
		}
		chunk, err := service.ReadArtifact(ctx, entry.ArtifactID, 0, 512)
		if err != nil || chunk.TotalBytes != part.SizeBytes {
			t.Fatalf("brw_artifact_read on %q: chunk=%+v err=%v", entry.Role, chunk, err)
		}
	}

	// Every part is gone once its TTL passes, manifest included.
	store.now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	for _, entry := range manifest.Entries {
		if _, err := store.Info(entry.ArtifactID); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("entry %q outlived its TTL: %v", entry.Role, err)
		}
	}
	if _, err := store.Info(meta.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest outlived its TTL: %v", err)
	}
}

// TestFailureBundleNetworkMetadataDropsDenylistedHeaders asserts both halves:
// the credential header is gone, and the ordinary header is still there — so
// the test cannot pass by capturing nothing.
func TestFailureBundleNetworkMetadataDropsDenylistedHeaders(t *testing.T) {
	store := newTestStore(t, 1<<20, 8<<20)
	service, _ := newBundleService(t, store, FailureCaptureAll)
	ctx := context.Background()

	meta, err := service.CaptureFailureBundle(ctx, FailureBundleOptions{Reason: "forced failure"})
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(readWholeArtifact(t, store, meta.ID), &manifest); err != nil {
		t.Fatal(err)
	}
	var networkID string
	for _, entry := range manifest.Entries {
		if entry.Role == "network" {
			networkID = entry.ArtifactID
		}
	}
	if networkID == "" {
		t.Fatalf("no network evidence in %+v", manifest.Entries)
	}

	raw := readWholeArtifact(t, store, networkID)
	if bytes.Contains(raw, []byte(bundleCredentialValue)) {
		t.Fatalf("credential value survived into network metadata: %s", raw)
	}
	if bytes.Contains(raw, []byte(bundlePageSentinel)) {
		t.Fatalf("request/response bodies survived into network metadata: %s", raw)
	}
	var captured BundleNetwork
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	if len(captured.Requests) != 1 {
		t.Fatalf("requests = %+v", captured.Requests)
	}
	request := captured.Requests[0]
	if _, present := request.Headers["Authorization"]; present {
		t.Fatalf("denylisted header present in captured metadata: %+v", request.Headers)
	}
	if request.Headers[bundleBenignHeaderName] != bundleBenignHeaderValue {
		t.Fatalf("ordinary header was dropped too, so the assertion above proves nothing: %+v", request.Headers)
	}
	if len(request.WithheldHeaders) != 1 || request.WithheldHeaders[0] != "authorization" {
		t.Fatalf("withheld headers = %v, want the denylisted name recorded as withheld", request.WithheldHeaders)
	}
	if request.Status != 500 || request.Method != "POST" {
		t.Fatalf("bounded metadata lost the diagnosis: %+v", request)
	}

	// Redacted trace values must not reappear either.
	for _, entry := range manifest.Entries {
		if bytes.Contains(readWholeArtifact(t, store, entry.ArtifactID), []byte(bundleCredentialValue)) {
			t.Fatalf("credential value reached bundle part %q", entry.Role)
		}
	}
}

func TestFailureBundlePolicyGatesEveryWrite(t *testing.T) {
	tests := []struct {
		name       string
		policy     FailureCapturePolicy
		optIn      bool
		wantBundle bool
	}{
		{name: "default off ignores an opted-in recipe", policy: FailureCaptureOff, optIn: true},
		{name: "recipe policy without opt-in", policy: FailureCaptureRecipe},
		{name: "recipe policy with opt-in", policy: FailureCaptureRecipe, optIn: true, wantBundle: true},
		{name: "all policy without opt-in", policy: FailureCaptureAll, wantBundle: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t, 1<<20, 8<<20)
			service, fake := newBundleService(t, store, test.policy)
			meta, err := service.CaptureFailureBundle(context.Background(), FailureBundleOptions{
				Reason: "forced failure", RecipeOptIn: test.optIn,
			})
			if !test.wantBundle {
				if !errors.Is(err, ErrFailureBundlesDisabled) {
					t.Fatalf("error = %v, want ErrFailureBundlesDisabled", err)
				}
				if fake.consoleN != 0 || fake.networkN != 0 {
					t.Fatalf("disabled policy still probed the browser (console=%d network=%d)", fake.consoleN, fake.networkN)
				}
				assertStoreIsEmpty(t, store)
				return
			}
			if err != nil || meta.ID == "" {
				t.Fatalf("meta=%+v err=%v", meta, err)
			}
		})
	}
}

func assertStoreIsEmpty(t *testing.T, store *Store) {
	t.Helper()
	entries, err := os.ReadDir(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".blob") || strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("capture-off failure wrote %q", entry.Name())
		}
	}
}

// TestFailureBundleRecordsPartsItCouldNotCollect keeps a partial bundle honest.
// A bundle that silently omits the screenshot tells its reader the page had none.
func TestFailureBundleRecordsPartsItCouldNotCollect(t *testing.T) {
	store := newTestStore(t, 1<<20, 8<<20)
	service, fake := newBundleService(t, store, FailureCaptureAll)
	// The page navigated away before the evidence could be collected, which is
	// exactly what a recipe origin boundary must refuse to capture across.
	fake.page = snapshot.PageSnapshot{URL: "https://elsewhere.example.test/", Title: "elsewhere"}
	ctx := browser.WithAllowedOrigins(context.Background(), []string{"https://billing.example.test"})

	meta, err := service.CaptureFailureBundle(ctx, FailureBundleOptions{Reason: "forced failure"})
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(readWholeArtifact(t, store, meta.ID), &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Missing) != 1 || manifest.Missing[0].Role != "semantic_snapshot" {
		t.Fatalf("missing = %+v, want the snapshot recorded as uncollected", manifest.Missing)
	}
	for _, entry := range manifest.Entries {
		if entry.Role == "semantic_snapshot" {
			t.Fatal("snapshot from a disallowed origin was stored anyway")
		}
	}
}

// TestFailureBundleEncryptsRecipeEvidenceWhenConfigured ties the two features
// together: evidence from a private-recipe run is the most sensitive thing brw
// stores, so it is what the recipe encryption policy is for.
func TestFailureBundleEncryptsRecipeEvidenceWhenConfigured(t *testing.T) {
	store := newEncryptedTestStore(t, 1<<20, 8<<20)
	service, _ := newBundleService(t, store, FailureCaptureAll)
	if err := service.SetEncryptionPolicy(EncryptRecipe); err != nil {
		t.Fatal(err)
	}
	ctx := browser.WithAllowedOrigins(context.Background(), []string{"https://billing.example.test"})

	meta, err := service.CaptureFailureBundle(ctx, FailureBundleOptions{Reason: "forced failure"})
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(readWholeArtifact(t, store, meta.ID), &manifest); err != nil {
		t.Fatal(err)
	}
	for _, entry := range manifest.Entries {
		part, err := store.Info(entry.ArtifactID)
		if err != nil {
			t.Fatal(err)
		}
		if !part.Encrypted {
			t.Fatalf("recipe evidence part %q was stored in the clear", entry.Role)
		}
	}
	assertNoPlaintextOnDisk(t, store.Root(), "Uncaught TypeError")

	// A capture outside a recipe run is untouched by the recipe policy.
	plain, err := service.CaptureFailureBundle(context.Background(), FailureBundleOptions{Reason: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := store.Info(plain.ID); err != nil || info.Encrypted {
		t.Fatalf("non-recipe bundle encrypted = %v (err %v), want false under the recipe policy", info.Encrypted, err)
	}
}
