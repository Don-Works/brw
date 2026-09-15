package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/recipe"
)

// privateProviderRoot writes one private recipe into a 0700 directory outside
// any checkout, which is what a directory provider requires of its root.
func privateProviderRoot(t *testing.T) (string, recipe.Recipe) {
	t.Helper()
	visible := true
	value := recipe.Recipe{
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
	return root, value
}

// TestBaselineRouteAnswersAProxyingDaemon is the hop that keeps the baseline
// routing rule true on a --upstream-http deployment.
//
// brw_baseline has no HTTP route of its own: it runs on whichever daemon the
// agent is talking to, while the private recipe provider lives on the browser
// host. Without this route a proxy cannot tell a public fixture from a capture
// of a page the provider's recipes reach, and writes both to its own
// --baseline-root.
func TestBaselineRouteAnswersAProxyingDaemon(t *testing.T) {
	root, value := privateProviderRoot(t)
	owned, err := recipe.Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := recipe.NewDirectoryProvider(context.Background(), recipe.DirectoryConfig{Root: root})
	if err != nil {
		t.Fatalf("directory provider: %v", err)
	}

	host := New("", &fakeController{})
	host.SetBaselineRouter(provider)
	upstream := httptest.NewServer(host.Handler())
	defer upstream.Close()

	proxy, err := httpclient.New(upstream.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	const unowned = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	tests := []struct {
		name    string
		digest  string
		pageURL string
		want    recipe.BaselineDestination
	}{
		{name: "a recipe the host's provider owns, with no page (list and delete capture nothing)", digest: owned, want: recipe.BaselineProvider},
		{
			name: "a recipe the host's provider owns, on a page it declares", digest: owned,
			pageURL: value.Origins[0] + "/invoices", want: recipe.BaselineProvider,
		},
		{
			name: "a recipe the host's provider owns, on a page it never visits", digest: owned,
			pageURL: "https://mail.unrelated.test/inbox/secret-thread", want: recipe.BaselineRefusedPageOutsideRecipe,
		},
		{name: "a public fixture", digest: unowned, pageURL: "https://fixtures.example.test/report", want: recipe.BaselineLocal},
		{
			name: "an invented digest on a page the provider's recipes reach", digest: unowned,
			pageURL: "https://billing.example.test/invoices?month=3", want: recipe.BaselineRefusedProviderReachesPage,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := proxy.RouteBaseline(context.Background(), tc.digest, tc.pageURL)
			if err != nil {
				t.Fatalf("RouteBaseline: %v", err)
			}
			if got.Destination() != tc.want {
				t.Fatalf("RouteBaseline = %q, want %q", got.Destination(), tc.want)
			}
		})
	}
}

// TestBaselineRouteOnAHostWithNoProviderOwnsNothing: a browser host with no
// private recipes has nothing to protect, and answering with an error would
// stop a proxy gating public fixtures against it.
func TestBaselineRouteOnAHostWithNoProviderOwnsNothing(t *testing.T) {
	host := New("", &fakeController{})
	upstream := httptest.NewServer(host.Handler())
	defer upstream.Close()

	proxy, err := httpclient.New(upstream.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	route, err := proxy.RouteBaseline(context.Background(), strings.Repeat("ab", 32), "https://billing.example.test/invoices")
	if err != nil {
		t.Fatalf("RouteBaseline against a host with no provider: %v", err)
	}
	if route.Destination() != recipe.BaselineLocal {
		t.Fatalf("a host with no recipe provider answered %q, want the local root", route.Destination())
	}
}

// TestBaselineRouteDisclosesOnlyTheDestination. The route exists so a proxy can
// decide where a capture belongs; a recipe body, an id or a digest list coming
// back would make it a way to read the private corpus over HTTP.
func TestBaselineRouteDisclosesOnlyTheDestination(t *testing.T) {
	root, value := privateProviderRoot(t)
	owned, err := recipe.Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := recipe.NewDirectoryProvider(context.Background(), recipe.DirectoryConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	host := New("", &fakeController{})
	host.SetBaselineRouter(provider)

	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"recipe_digest":"` + owned + `","page_url":"https://billing.example.test/invoices"}`)
	host.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/baselines/route", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var answer map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(answer) != 1 || answer["destination"] != string(recipe.BaselineProvider) {
		t.Fatalf("answer = %v, want exactly the destination word", answer)
	}
	for _, secret := range []string{value.ID, value.Name, value.Description, "download-invoices"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("the routing answer carried %q out of the private provider: %s", secret, rec.Body.String())
		}
	}
}
