package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/httpclient"
	"github.com/Don-Works/brw/internal/recipe"
)

// The routing rule is a property of the pair — this recipe, this page — and not
// of the lane the question arrived on. Every implementation of
// recipe.BaselineRouter therefore has to answer it the same way, so this file
// drives all of them through one matrix and fails when a new one appears that
// nobody classified. A rule enforced on the lanes that existed when it was
// written is a rule the next transport walks around.

// baselineLane is one implementation of recipe.BaselineRouter under test,
// keyed by the "package.Type" the source declares it as.
type baselineLane struct {
	name   string
	router recipe.BaselineRouter
}

// ownerAnswer is the /v1/baselines/owner handler a real HTTPS provider serves:
// it holds one recipe, and answers both halves of the question about it.
func ownerAnswer(ownedDigest string, ownedOrigins []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RecipeDigest string `json:"recipe_digest"`
			Origin       string `json:"origin"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		owns := strings.EqualFold(req.RecipeDigest, ownedDigest)
		onOwnedOrigin := false
		for _, origin := range ownedOrigins {
			if req.Origin != "" && strings.EqualFold(req.Origin, origin) {
				onOwnedOrigin = true
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"owns":        owns,
			"owns_origin": onOwnedOrigin,
			"covers_page": owns && onOwnedOrigin,
		})
	}
}

// baselineLanes builds every shipped router over the same one private recipe.
func baselineLanes(t *testing.T, ownedDigest string, value recipe.Recipe, root string) []baselineLane {
	t.Helper()

	directory, err := recipe.NewDirectoryProvider(context.Background(), recipe.DirectoryConfig{Root: root})
	if err != nil {
		t.Fatalf("directory provider: %v", err)
	}

	providerMux := http.NewServeMux()
	providerMux.HandleFunc("/v1/baselines/owner", ownerAnswer(ownedDigest, value.Origins))
	providerServer := httptest.NewServer(providerMux)
	t.Cleanup(providerServer.Close)
	remote, err := recipe.NewHTTPProvider(recipe.HTTPProviderConfig{BaseURL: providerServer.URL, RequestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("https provider: %v", err)
	}

	// The proxy lane: a browser host holding the directory provider, and a
	// daemon that can only ask it. brw_baseline runs on the asking daemon.
	host := New("", &fakeController{})
	host.SetBaselineRouter(directory)
	hostServer := httptest.NewServer(host.Handler())
	t.Cleanup(hostServer.Close)
	proxy, err := httpclient.New(hostServer.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("proxy controller: %v", err)
	}

	return []baselineLane{
		{name: "recipe.DirectoryProvider", router: directory},
		{name: "recipe.HTTPProvider", router: remote},
		{name: "httpclient.Controller", router: proxy},
	}
}

// TestEveryBaselineLaneRoutesOnTheRecipeAndThePage drives the matrix through
// every router, and fails on a router the matrix does not cover.
func TestEveryBaselineLaneRoutesOnTheRecipeAndThePage(t *testing.T) {
	root, value := privateProviderRoot(t)
	owned, err := recipe.Digest(value)
	if err != nil {
		t.Fatal(err)
	}
	const unowned = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	ownedPage := value.Origins[0] + "/invoices?month=3"
	const unrelatedPage = "https://mail.unrelated.test/inbox/secret-thread"

	lanes := baselineLanes(t, owned, value, root)
	classified := map[string]bool{}
	for _, lane := range lanes {
		classified[lane.name] = true
	}
	for _, declared := range baselineRouterImplementations(t) {
		if !classified[declared] {
			t.Fatalf("%s implements recipe.BaselineRouter and is not in this matrix: add it to baselineLanes, or the routing rule holds on every lane but that one", declared)
		}
	}

	cases := []struct {
		name    string
		digest  string
		pageURL string
		want    recipe.BaselineDestination
	}{
		{
			name:   "the provider's recipe, on a page it declares",
			digest: owned, pageURL: ownedPage, want: recipe.BaselineProvider,
		},
		{
			name:   "the provider's recipe, on a page it never visits",
			digest: owned, pageURL: unrelatedPage, want: recipe.BaselineRefusedPageOutsideRecipe,
		},
		{
			name:   "an invented digest, on a page the provider's recipes reach",
			digest: unowned, pageURL: ownedPage, want: recipe.BaselineRefusedProviderReachesPage,
		},
		{
			name:   "an invented digest, on a page nothing claims",
			digest: unowned, pageURL: "https://fixtures.example.test/report", want: recipe.BaselineLocal,
		},
		{
			name:   "the provider's recipe with no page, which list and delete are",
			digest: owned, pageURL: "", want: recipe.BaselineProvider,
		},
		{
			// A page with no origin is not the same as no page. A lane that
			// reduced the URL to an origin before passing it on would lose the
			// difference, and an owned digest would decide on its own again.
			name:   "the provider's recipe, on a page with no origin at all",
			digest: owned, pageURL: "about:blank", want: recipe.BaselineRefusedPageOutsideRecipe,
		},
	}
	for _, lane := range lanes {
		for _, tc := range cases {
			t.Run(lane.name+"/"+tc.name, func(t *testing.T) {
				route, err := lane.router.RouteBaseline(context.Background(), tc.digest, tc.pageURL)
				if err != nil {
					t.Fatalf("RouteBaseline: %v", err)
				}
				if route.Destination() != tc.want {
					t.Fatalf("RouteBaseline = %q, want %q", route.Destination(), tc.want)
				}
			})
		}
	}
}

// baselineRouterImplementations lists every type in this module that declares a
// RouteBaseline method, as "package.Type".
//
// Read from the source rather than from a hand-kept list, because a hand-kept
// list is what a new lane is added without touching.
func baselineRouterImplementations(t *testing.T) []string {
	t.Helper()
	root := moduleRoot(t)
	found := map[string]bool{}
	fileSet := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		// Test files are skipped: a stand-in written for one test is not a lane
		// a deployment routes through.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv == nil || function.Name.Name != "RouteBaseline" {
				continue
			}
			if len(function.Recv.List) != 1 {
				continue
			}
			found[parsed.Name.Name+"."+receiverTypeName(function.Recv.List[0].Type)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan for baseline routers: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no RouteBaseline implementations found: the scan is looking in the wrong place, so it can never fail on an unclassified one")
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func receiverTypeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(typed.X)
	case *ast.Ident:
		return typed.Name
	case *ast.IndexExpr:
		return receiverTypeName(typed.X)
	default:
		return ""
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("no go.mod above the test's working directory")
		}
		directory = parent
	}
}
