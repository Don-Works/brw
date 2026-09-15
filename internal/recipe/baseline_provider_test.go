package recipe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/baseline"
	"github.com/Don-Works/brw/internal/snapshot"
)

// baselineFixtureImage is a small PNG with a distinguishable pixel, so a
// round-trip that silently substituted a blank image would be caught.
func baselineFixtureImage(t *testing.T, shade uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, color.RGBA{R: shade, G: uint8(x * 20), B: uint8(y * 20), A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func baselineFixtureEnvironment() baseline.Environment {
	return baseline.Environment{
		BrowserBuild: "Chromium/152.0.0.0", ViewportWidth: 1280, ViewportHeight: 800,
		DevicePixelRatio: 2, Locale: "en-gb", OS: runtime.GOOS,
	}
}

func baselineFixtureRecord(t *testing.T, digest string, step int, shade uint8) baseline.Record {
	t.Helper()
	key := baseline.Key{RecipeDigest: digest, StepIndex: step, Environment: baselineFixtureEnvironment()}
	return baseline.Record{
		Key:         key,
		Environment: key.Environment,
		Tree: snapshot.AriaTree{Nodes: []snapshot.AriaNode{
			{Role: "heading", Name: "Signed in as fixture operator"},
			{Role: "button", Name: "Request export"},
		}},
		IgnoreRegions: []baseline.IgnoreRegion{{Name: "clock", X: 1, Y: 1, Width: 2, Height: 2}},
		CreatedAt:     time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC),
		Screenshot:    baselineFixtureImage(t, shade),
	}
}

// privateRecipeRoot is a 0700 directory outside any checkout, which is what a
// directory provider requires of its root.
func privateRecipeRoot(t *testing.T, value Recipe) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "recipe.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// fakeBaselineProvider is the provider side of the HTTPS wire format: it stores
// exactly what it is sent and serves back exactly that. It is a stand-in for an
// operator's provider, not a second implementation of the rules — the rules
// live in encodeBaseline/decodeBaseline and are what the tests exercise.
type fakeBaselineProvider struct {
	owned   map[string]bool
	records map[string]baselineWire
	// mutate lets a test make the provider answer with something other than
	// what it was given, which is the only way to check that the client refuses
	// a bad answer rather than storing it.
	mutate func(baselineWire) baselineWire
	puts   int
}

func newFakeBaselineProvider(ownedDigest string) *fakeBaselineProvider {
	return &fakeBaselineProvider{
		owned:   map[string]bool{strings.ToLower(ownedDigest): true},
		records: map[string]baselineWire{},
	}
}

func (f *fakeBaselineProvider) key(digest string, step int, fingerprint string) string {
	return strings.ToLower(digest) + "/" + fingerprint + "/" + string(rune('0'+step))
}

func (f *fakeBaselineProvider) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	decode := func(w http.ResponseWriter, r *http.Request, into any) bool {
		if err := json.NewDecoder(r.Body).Decode(into); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return false
		}
		return true
	}
	mux.HandleFunc("/v1/baselines/owner", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RecipeDigest string `json:"recipe_digest"`
		}
		if !decode(w, r, &req) {
			return
		}
		writeFakeJSON(w, map[string]any{"owns": f.owned[strings.ToLower(req.RecipeDigest)]})
	})
	mux.HandleFunc("/v1/baselines/put", func(w http.ResponseWriter, r *http.Request) {
		var wire baselineWire
		if !decode(w, r, &wire) {
			return
		}
		f.puts++
		f.records[f.key(wire.RecipeDigest, wire.StepIndex, wire.Fingerprint)] = wire
		writeFakeJSON(w, map[string]any{"stored": true})
	})
	mux.HandleFunc("/v1/baselines/fetch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RecipeDigest string `json:"recipe_digest"`
			StepIndex    int    `json:"step_index"`
			Fingerprint  string `json:"environment_fingerprint"`
		}
		if !decode(w, r, &req) {
			return
		}
		wire, ok := f.records[f.key(req.RecipeDigest, req.StepIndex, req.Fingerprint)]
		if !ok {
			writeFakeJSON(w, map[string]any{"found": false})
			return
		}
		if f.mutate != nil {
			wire = f.mutate(wire)
		}
		writeFakeJSON(w, map[string]any{"found": true, "baseline": wire})
	})
	mux.HandleFunc("/v1/baselines/environments", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RecipeDigest string `json:"recipe_digest"`
			StepIndex    int    `json:"step_index"`
		}
		if !decode(w, r, &req) {
			return
		}
		environments := []baseline.Environment{}
		for _, wire := range f.records {
			if strings.EqualFold(wire.RecipeDigest, req.RecipeDigest) && wire.StepIndex == req.StepIndex {
				environments = append(environments, wire.Environment)
			}
		}
		writeFakeJSON(w, map[string]any{"environments": environments})
	})
	mux.HandleFunc("/v1/baselines/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RecipeDigest string `json:"recipe_digest"`
			StepIndex    int    `json:"step_index"`
			Fingerprint  string `json:"environment_fingerprint"`
		}
		if !decode(w, r, &req) {
			return
		}
		key := f.key(req.RecipeDigest, req.StepIndex, req.Fingerprint)
		_, ok := f.records[key]
		delete(f.records, key)
		writeFakeJSON(w, map[string]any{"deleted": ok})
	})
	return mux
}

func writeFakeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// baselineImplementations builds each shipped provider with one recipe in it,
// so every assertion below runs against both. A capability implemented on one
// transport and quietly missing on the other is the failure mode this table
// exists for.
func baselineImplementations(t *testing.T) []struct {
	name     string
	store    BaselineStore
	provider Provider
	owned    string
	fake     *fakeBaselineProvider
} {
	t.Helper()
	value := validRecipe("https://billing.example.test")
	digest, err := Digest(value)
	if err != nil {
		t.Fatal(err)
	}

	directory, err := NewDirectoryProvider(context.Background(), DirectoryConfig{Root: privateRecipeRoot(t, value)})
	if err != nil {
		t.Fatalf("directory provider: %v", err)
	}

	fake := newFakeBaselineProvider(digest)
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)
	remote, err := NewHTTPProvider(HTTPProviderConfig{BaseURL: server.URL, RequestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("http provider: %v", err)
	}

	return []struct {
		name     string
		store    BaselineStore
		provider Provider
		owned    string
		fake     *fakeBaselineProvider
	}{
		{name: "directory", store: directory, provider: directory, owned: digest},
		{name: "https", store: remote, provider: remote, owned: digest, fake: fake},
	}
}

// TestBothProvidersStoreAndServeABaseline is the write path T3B4JJ is about: a
// baseline of a private recipe's page lands with the provider that owns the
// recipe, and comes back byte-identical under the same key.
func TestBothProvidersStoreAndServeABaseline(t *testing.T) {
	for _, implementation := range baselineImplementations(t) {
		t.Run(implementation.name, func(t *testing.T) {
			ctx := context.Background()
			record := baselineFixtureRecord(t, implementation.owned, 2, 200)

			owns, err := implementation.store.OwnsRecipe(ctx, implementation.owned)
			if err != nil || !owns {
				t.Fatalf("OwnsRecipe(own digest) = %v, %v; want true", owns, err)
			}
			foreign := strings.Repeat("ab", 32)
			if owns, err := implementation.store.OwnsRecipe(ctx, foreign); err != nil || owns {
				t.Fatalf("OwnsRecipe(foreign digest) = %v, %v; want false", owns, err)
			}

			if _, found, err := implementation.store.LoadBaseline(ctx, record.Key); err != nil || found {
				t.Fatalf("a baseline was found before one was written: found=%v err=%v", found, err)
			}
			if err := implementation.store.PutBaseline(ctx, record); err != nil {
				t.Fatalf("PutBaseline: %v", err)
			}

			loaded, found, err := implementation.store.LoadBaseline(ctx, record.Key)
			if err != nil || !found {
				t.Fatalf("LoadBaseline after put: found=%v err=%v", found, err)
			}
			if !bytes.Equal(loaded.Screenshot, record.Screenshot) {
				t.Fatalf("screenshot changed in transit: %d bytes out, %d back", len(record.Screenshot), len(loaded.Screenshot))
			}
			if loaded.Key.ID() != record.Key.ID() {
				t.Fatalf("baseline id = %s, want %s", loaded.Key.ID(), record.Key.ID())
			}
			if loaded.Environment.Fingerprint() != record.Key.Environment.Normalize().Fingerprint() {
				t.Fatal("the environment fingerprint did not survive the round trip")
			}
			if len(loaded.Tree.Nodes) != len(record.Tree.Nodes) || loaded.Tree.Nodes[1].Name != "Request export" {
				t.Fatalf("the ARIA half of the baseline did not survive: %+v", loaded.Tree)
			}
			if len(loaded.IgnoreRegions) != 1 || loaded.IgnoreRegions[0].Name != "clock" {
				t.Fatalf("ignore regions did not survive: %+v", loaded.IgnoreRegions)
			}

			environments, err := implementation.store.BaselineEnvironments(ctx, implementation.owned, 2)
			if err != nil || len(environments) != 1 {
				t.Fatalf("BaselineEnvironments = %+v, %v; want the one just written", environments, err)
			}
			if environments[0].Fingerprint() != record.Key.Environment.Normalize().Fingerprint() {
				t.Fatal("BaselineEnvironments reported an environment that is not the stored one")
			}
			// Another step of the same recipe is a different baseline, not an
			// overwrite of this one.
			if err := implementation.store.PutBaseline(ctx, baselineFixtureRecord(t, implementation.owned, 3, 40)); err != nil {
				t.Fatalf("PutBaseline(step 3): %v", err)
			}
			if environments, err := implementation.store.BaselineEnvironments(ctx, implementation.owned, 2); err != nil || len(environments) != 1 {
				t.Fatalf("step 2 environments after writing step 3 = %+v, %v", environments, err)
			}

			if err := implementation.store.DeleteBaseline(ctx, record.Key); err != nil {
				t.Fatalf("DeleteBaseline: %v", err)
			}
			if _, found, err := implementation.store.LoadBaseline(ctx, record.Key); err != nil || found {
				t.Fatalf("baseline still present after delete: found=%v err=%v", found, err)
			}
			if location := implementation.store.BaselineLocation(); !strings.Contains(location, "provider") {
				t.Fatalf("BaselineLocation = %q, which does not say the baseline is with the provider", location)
			}
		})
	}
}

// TestBothProvidersRunACheckThroughTheStorageAdapter drives baseline.Check
// against each provider, which is the path brw_baseline actually takes. A
// capability that round-trips but cannot back a check is not a write path.
func TestBothProvidersRunACheckThroughTheStorageAdapter(t *testing.T) {
	for _, implementation := range baselineImplementations(t) {
		t.Run(implementation.name, func(t *testing.T) {
			ctx := context.Background()
			storage := ProviderBaselines(ctx, implementation.store)
			record := baselineFixtureRecord(t, implementation.owned, 0, 120)
			options := baseline.CheckOptions{
				Key:        record.Key,
				Screenshot: record.Screenshot,
				Tree:       record.Tree,
			}

			// A first check writes nothing and fails: a gate that passes because
			// it has never seen the page is not a gate.
			missing, err := baseline.Check(storage, options)
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if missing.Status != baseline.StatusMissing || !missing.Failed {
				t.Fatalf("first check = %+v, want missing/failed", missing)
			}
			if _, found, err := implementation.store.LoadBaseline(ctx, record.Key); err != nil || found {
				t.Fatalf("a check wrote a baseline: found=%v err=%v", found, err)
			}

			recorded, err := baseline.Check(storage, withUpdate(options))
			if err != nil || recorded.Status != baseline.StatusRecorded {
				t.Fatalf("update = %+v, %v; want recorded", recorded, err)
			}
			match, err := baseline.Check(storage, options)
			if err != nil || match.Status != baseline.StatusMatch || match.Failed {
				t.Fatalf("check against the stored baseline = %+v, %v; want match", match, err)
			}

			// A changed page fails against the baseline held by the provider,
			// which is the only thing that makes the stored copy a gate.
			changed := options
			changed.Screenshot = baselineFixtureImage(t, 10)
			diff, err := baseline.Check(storage, changed)
			if err != nil {
				t.Fatalf("check changed page: %v", err)
			}
			if diff.Status != baseline.StatusDiff || !diff.Failed {
				t.Fatalf("changed page = %+v, want diff/failed", diff)
			}
		})
	}
}

func withUpdate(options baseline.CheckOptions) baseline.CheckOptions {
	options.Update = true
	return options
}

// TestDirectoryProviderBaselineDoesNotBreakRecipeDiscovery: a stored baseline is
// a .json file, and every .json file under a recipe root is parsed as a recipe.
// Without the reserved subtree, recording one baseline would take the whole
// private corpus offline.
func TestDirectoryProviderBaselineDoesNotBreakRecipeDiscovery(t *testing.T) {
	ctx := context.Background()
	value := validRecipe("https://billing.example.test")
	digest, err := Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	root := privateRecipeRoot(t, value)
	provider, err := NewDirectoryProvider(ctx, DirectoryConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.PutBaseline(ctx, baselineFixtureRecord(t, digest, 0, 77)); err != nil {
		t.Fatalf("PutBaseline: %v", err)
	}

	// The record really is inside the provider's root — that is the point of
	// the feature — and it really is a .json file.
	records := 0
	err = filepath.WalkDir(filepath.Join(root, BaselineRoot), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			records++
		}
		return nil
	})
	if err != nil || records == 0 {
		t.Fatalf("walked the provider's baseline subtree: %d json records, err=%v", records, err)
	}

	matches, err := provider.Search(ctx, "download monthly billing invoices", "", 5)
	if err != nil {
		t.Fatalf("search after a baseline was written: %v", err)
	}
	if len(matches) != 1 || matches[0].Digest != digest {
		t.Fatalf("search after a baseline was written returned %+v", matches)
	}
	if _, err := provider.Fetch(ctx, value.ID, value.Version, digest); err != nil {
		t.Fatalf("fetch after a baseline was written: %v", err)
	}
}

// TestHTTPProviderRefusesABaselineThatIsNotTheOneAskedFor: the provider is
// authenticated, not trusted. A record for another step, another environment or
// with an image that is not one would be compared against the live page and
// reported as a regression in it.
func TestHTTPProviderRefusesABaselineThatIsNotTheOneAskedFor(t *testing.T) {
	value := validRecipe("https://billing.example.test")
	digest, err := Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(baselineWire) baselineWire
		want   string
	}{
		{
			name: "another step of the same recipe",
			mutate: func(wire baselineWire) baselineWire {
				wire.StepIndex++
				return wire
			},
			want: "different key",
		},
		{
			name: "an environment that is not the one requested",
			mutate: func(wire baselineWire) baselineWire {
				wire.Environment.DevicePixelRatio = 1
				wire.Fingerprint = wire.Environment.Fingerprint()
				return wire
			},
			want: "different key",
		},
		{
			name: "a fingerprint that does not describe its own environment",
			mutate: func(wire baselineWire) baselineWire {
				wire.Fingerprint = strings.Repeat("c", 64)
				return wire
			},
			want: "does not describe its environment",
		},
		{
			name: "an image that is not an image",
			mutate: func(wire baselineWire) baselineWire {
				wire.ScreenshotPNG = "bm90IGFuIGltYWdl"
				return wire
			},
			want: "not a decodable image",
		},
		{
			name: "no image at all",
			mutate: func(wire baselineWire) baselineWire {
				wire.ScreenshotPNG = ""
				return wire
			},
			want: "no screenshot",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeBaselineProvider(digest)
			fake.mutate = tc.mutate
			server := httptest.NewServer(fake.handler(t))
			defer server.Close()
			provider, err := NewHTTPProvider(HTTPProviderConfig{BaseURL: server.URL, RequestTimeout: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			record := baselineFixtureRecord(t, digest, 1, 90)
			if err := provider.PutBaseline(ctx, record); err != nil {
				t.Fatalf("PutBaseline: %v", err)
			}
			_, found, err := provider.LoadBaseline(ctx, record.Key)
			if err == nil {
				t.Fatalf("a substituted baseline was accepted (found=%v)", found)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// TestProvidersRefuseAnUnusableBaselineBeforeItLeavesTheMachine: both
// implementations round-trip a record through the same wire format on the way
// out, so a baseline one accepts is one the other accepts. A write that is
// stored and can never be read back is worse than a refused one.
func TestProvidersRefuseAnUnusableBaselineBeforeItLeavesTheMachine(t *testing.T) {
	broken := []struct {
		name   string
		record func(baseline.Record) baseline.Record
	}{
		{"no screenshot", func(r baseline.Record) baseline.Record { r.Screenshot = nil; return r }},
		{"a screenshot that is not an image", func(r baseline.Record) baseline.Record {
			r.Screenshot = []byte("not an image")
			return r
		}},
		{"a digest that is not a digest", func(r baseline.Record) baseline.Record {
			r.Key.RecipeDigest = "../../etc"
			return r
		}},
		{"an incomplete environment", func(r baseline.Record) baseline.Record {
			r.Key.Environment.Locale = ""
			return r
		}},
		{"a negative step", func(r baseline.Record) baseline.Record { r.Key.StepIndex = -1; return r }},
	}
	for _, implementation := range baselineImplementations(t) {
		for _, tc := range broken {
			t.Run(implementation.name+"/"+tc.name, func(t *testing.T) {
				record := tc.record(baselineFixtureRecord(t, implementation.owned, 0, 30))
				if err := implementation.store.PutBaseline(context.Background(), record); err == nil {
					t.Fatal("an unusable baseline was stored")
				}
				if implementation.fake != nil && implementation.fake.puts != 0 {
					t.Fatalf("the provider was sent %d unusable baselines; it should have been refused locally", implementation.fake.puts)
				}
			})
		}
	}
}

// TestDirectoryProviderBaselineRootRefusesACheckout keeps the store's own
// refusal reachable through the provider: a private recipe root that is inside
// a Git working tree cannot become a place baselines are written.
func TestDirectoryProviderBaselineRootRefusesACheckout(t *testing.T) {
	value := validRecipe("https://billing.example.test")
	digest, err := Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	root := privateRecipeRoot(t, value)
	provider, err := NewDirectoryProvider(context.Background(), DirectoryConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	// The checkout appears after the provider is up, which is the realistic
	// order: `git init` in a directory that already held recipes.
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	err = provider.PutBaseline(context.Background(), baselineFixtureRecord(t, digest, 0, 60))
	if err == nil {
		t.Fatal("a baseline was written into a Git working tree")
	}
	if !errors.Is(err, baseline.ErrRootInsideRepository) {
		t.Fatalf("error = %v, want the repository refusal", err)
	}
}

// TestReservedBaselineDirRefusesRecipesRatherThanHidingThem: the reserved
// subtree is skipped by recipe discovery, so anything an operator put there
// would stop being served with no error. A root that refuses to load and says
// why is the lesser failure.
func TestReservedBaselineDirRefusesRecipesRatherThanHidingThem(t *testing.T) {
	ctx := context.Background()
	value := validRecipe("https://billing.example.test")
	root := privateRecipeRoot(t, value)
	reserved := filepath.Join(root, BaselineRoot, "misplaced")
	if err := os.MkdirAll(reserved, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reserved, "invoices.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = NewDirectoryProvider(ctx, DirectoryConfig{Root: root})
	if err == nil {
		t.Fatal("a recipe hidden in the reserved baseline subtree was accepted")
	}
	if !strings.Contains(err.Error(), "reserved for regression baselines") {
		t.Fatalf("error = %v, want the reserved-name refusal", err)
	}
}

// TestReservedBaselineDirAcceptsWhatTheStoreWrites pins the guard to the record
// name baseline.Store actually uses. If the store renamed its record file, the
// guard above would refuse the provider's own writes on the next reload.
func TestReservedBaselineDirAcceptsWhatTheStoreWrites(t *testing.T) {
	ctx := context.Background()
	value := validRecipe("https://billing.example.test")
	digest, err := Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	root := privateRecipeRoot(t, value)
	provider, err := NewDirectoryProvider(ctx, DirectoryConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.PutBaseline(ctx, baselineFixtureRecord(t, digest, 0, 55)); err != nil {
		t.Fatalf("PutBaseline: %v", err)
	}
	// The reload is where the guard runs, and a provider that refused its own
	// baseline would only fail here.
	if _, err := provider.Search(ctx, "download monthly billing invoices", "", 5); err != nil {
		t.Fatalf("search after the provider wrote its own baseline: %v", err)
	}
	if err := checkReservedBaselineDir(filepath.Join(root, BaselineRoot)); err != nil {
		t.Fatalf("the guard refuses what baseline.Store writes: %v", err)
	}
}
