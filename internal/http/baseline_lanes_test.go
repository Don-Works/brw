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

type baselineLane struct {
	name   string
	router recipe.BaselineRouter
}

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
