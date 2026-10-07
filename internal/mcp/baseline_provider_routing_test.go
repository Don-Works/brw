package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/baseline"
	"github.com/Don-Works/brw/internal/recipe"
)

func privateRecipe() recipe.Recipe {
	visible := true
	return recipe.Recipe{
		SchemaVersion: recipe.SchemaVersion,
		ID:            "example.billing.download-invoices",
		Version:       "1.2.3",
		Name:          "Download billing invoices",
		Description:   "Download monthly statements from the billing portal.",
		Intents:       []string{"download invoices", "fetch billing receipts"},
		Origins:       []string{"https://billing.example.test"},
		Risk:          "read_only",
		Steps: []recipe.Step{{
			ID: "download", Action: "click", Effect: "read",
			Target: &recipe.Target{Role: "button", TestID: "download-invoices", Visible: &visible},
		}},
	}
}

type httpsProviderFixture struct {
	mu    sync.Mutex
	owned string

	ownedOrigin string
	stored      map[string]json.RawMessage
	handler     http.Handler
}

func newHTTPSProviderFixture(t *testing.T, ownedDigest string) *httpsProviderFixture {
	t.Helper()
	fixture := &httpsProviderFixture{
		owned:       strings.ToLower(ownedDigest),
		ownedOrigin: privateRecipe().Origins[0],
		stored:      map[string]json.RawMessage{},
	}
	mux := http.NewServeMux()
	read := func(w http.ResponseWriter, r *http.Request) map[string]any {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return nil
		}
		return body
	}
	key := func(body map[string]any) string {
		digest, _ := body["recipe_digest"].(string)
		step, _ := body["step_index"].(float64)
		fingerprint, _ := body["environment_fingerprint"].(string)
		return fmt.Sprintf("%s/%d/%s", strings.ToLower(digest), int(step), fingerprint)
	}
	answer := func(w http.ResponseWriter, body any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
	mux.HandleFunc("/v1/baselines/owner", func(w http.ResponseWriter, r *http.Request) {
		body := read(w, r)
		if body == nil {
			return
		}
		digest, _ := body["recipe_digest"].(string)
		origin, _ := body["origin"].(string)
		owns := strings.EqualFold(digest, fixture.owned)
		onOwnedOrigin := origin != "" && strings.EqualFold(origin, fixture.ownedOrigin)
		answer(w, map[string]any{
			"owns":        owns,
			"owns_origin": onOwnedOrigin,

			"covers_page": owns && onOwnedOrigin,
		})
	})
	mux.HandleFunc("/v1/baselines/put", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fixture.mu.Lock()
		fixture.stored[key(body)] = raw
		fixture.mu.Unlock()
		answer(w, map[string]any{"stored": true})
	})
	mux.HandleFunc("/v1/baselines/fetch", func(w http.ResponseWriter, r *http.Request) {
		body := read(w, r)
		if body == nil {
			return
		}
		fixture.mu.Lock()
		raw, ok := fixture.stored[key(body)]
		fixture.mu.Unlock()
		if !ok {
			answer(w, map[string]any{"found": false})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"found":true,"baseline":%s}`, raw)
	})
	mux.HandleFunc("/v1/baselines/environments", func(w http.ResponseWriter, r *http.Request) {
		body := read(w, r)
		if body == nil {
			return
		}
		digest, _ := body["recipe_digest"].(string)
		step, _ := body["step_index"].(float64)
		environments := []any{}
		fixture.mu.Lock()
		for stored, raw := range fixture.stored {
			if !strings.HasPrefix(stored, fmt.Sprintf("%s/%d/", strings.ToLower(digest), int(step))) {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal(raw, &record); err == nil {
				environments = append(environments, record["environment"])
			}
		}
		fixture.mu.Unlock()
		answer(w, map[string]any{"environments": environments})
	})
	mux.HandleFunc("/v1/baselines/delete", func(w http.ResponseWriter, r *http.Request) {
		body := read(w, r)
		if body == nil {
			return
		}
		fixture.mu.Lock()
		stored := key(body)
		_, ok := fixture.stored[stored]
		delete(fixture.stored, stored)
		fixture.mu.Unlock()
		answer(w, map[string]any{"deleted": ok})
	})
	fixture.handler = mux
	return fixture
}

func (f *httpsProviderFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.stored)
}

type providerUnderTest struct {
	name  string
	store recipe.BaselineStore
	count func() int
}

func shippedProviders(t *testing.T, ownedDigest string) []providerUnderTest {
	t.Helper()
	value := privateRecipe()

	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "recipe.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := recipe.NewDirectoryProvider(context.Background(), recipe.DirectoryConfig{Root: root})
	if err != nil {
		t.Fatalf("directory provider: %v", err)
	}

	fixture := newHTTPSProviderFixture(t, ownedDigest)
	server := httptest.NewServer(fixture.handler)
	t.Cleanup(server.Close)
	remote, err := recipe.NewHTTPProvider(recipe.HTTPProviderConfig{BaseURL: server.URL, RequestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("https provider: %v", err)
	}

	return []providerUnderTest{
		{
			name:  "directory",
			store: directory,
			count: func() int {
				matches, err := filepath.Glob(filepath.Join(root, recipe.BaselineRoot, "*", "step-*", "*", "baseline.json"))
				if err != nil {
					t.Fatal(err)
				}
				return len(matches)
			},
		},
		{name: "https", store: remote, count: fixture.count},
	}
}

func TestBaselineRoutingAgainstBothShippedProviders(t *testing.T) {
	ownedDigest, err := recipe.Digest(privateRecipe())
	if err != nil {
		t.Fatal(err)
	}

	const publicDigest = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	for _, provider := range shippedProviders(t, ownedDigest) {
		t.Run(provider.name, func(t *testing.T) {

			controller := &pageController{label: "Download invoices", encoding: "jpeg", url: privateRecipe().Origins[0] + "/invoices"}
			s := baselineServer(t, controller)
			s.SetRecipeBaselines(provider.store)
			localRoot := s.baselines.Root()

			private := callBaselineTool(t, s, fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":0}`, ownedDigest))
			if status, _ := private["status"].(string); status != baseline.StatusRecorded {
				t.Fatalf("recording the private recipe's baseline = %+v", private)
			}
			if provider.count() != 1 {
				t.Fatalf("the %s provider holds %d baselines after recording one for a recipe it owns", provider.name, provider.count())
			}
			if got := localBaselineFiles(t, localRoot); got != 0 {
				t.Fatalf("%d private baselines were written to the local root", got)
			}

			controller.url = "https://fixtures.example.test/report"
			public := callBaselineTool(t, s, fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":0}`, publicDigest))
			if status, _ := public["status"].(string); status != baseline.StatusRecorded {
				t.Fatalf("recording the public fixture's baseline = %+v", public)
			}
			if provider.count() != 1 {
				t.Fatalf("a public fixture's baseline reached the %s provider: it now holds %d", provider.name, provider.count())
			}
			if got := localBaselineFiles(t, localRoot); got != 1 {
				t.Fatalf("%d baselines under the local root, want the public fixture's one", got)
			}

			for _, gate := range []struct{ digest, pageURL string }{
				{ownedDigest, privateRecipe().Origins[0] + "/invoices"},
				{publicDigest, "https://fixtures.example.test/report"},
			} {
				controller.url = gate.pageURL
				checked := callBaselineTool(t, s, fmt.Sprintf(`{"action":"check","recipe_digest":%q,"step_index":0}`, gate.digest))
				if status, _ := checked["status"].(string); status != baseline.StatusMatch {
					t.Fatalf("check for %s... = %+v, want match", gate.digest[:8], checked)
				}
			}

			controller.url = "https://mail.unrelated.test/inbox/secret-thread"
			args := fmt.Sprintf(`{"action":"update","recipe_digest":%q,"step_index":0}`, ownedDigest)
			result, rpcErr := s.callTool(context.Background(), baselineToolName, []byte(args))
			if rpcErr != nil {
				t.Fatalf("callTool: %+v", rpcErr)
			}
			failed, message := isToolError(t, result)
			if !failed || !strings.Contains(message, "does not visit the page") {
				t.Fatalf("capturing an unrelated page under an owned digest = %v / %q, want a refusal naming the page", failed, message)
			}
			if provider.count() != 1 || localBaselineFiles(t, localRoot) != 1 {
				t.Fatalf("the refused capture was stored anyway: %d with the %s provider, %d under the local root",
					provider.count(), provider.name, localBaselineFiles(t, localRoot))
			}
		})
	}
}
