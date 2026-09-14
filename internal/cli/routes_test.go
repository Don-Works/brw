package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The CLI must not be able to grow a surface the daemon does not serve. The
// route set is read out of internal/http's own mux registration rather than
// copied here, so adding a verb without a route fails this test instead of
// 404ing in somebody's shell.
func TestEveryVerbBindsToARouteTheDaemonServes(t *testing.T) {
	routes := daemonRoutes(t)
	// A parse that silently matched nothing would pass every verb, so anchor on
	// the order of magnitude the daemon actually registers.
	if len(routes) < 30 {
		t.Fatalf("parsed only %d routes from internal/http/server.go; the mux registration shape must have changed", len(routes))
	}

	for _, v := range verbs() {
		pattern := v.method + " " + v.path
		if !routes[pattern] {
			t.Errorf("verb %q binds to %q, which internal/http does not register", v.name, pattern)
		}
	}
}

func TestVerbTableIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	builtins := map[string]bool{}
	for _, word := range builtinCommands() {
		builtins[word] = true
	}
	for _, v := range verbs() {
		if seen[v.name] {
			t.Errorf("verb %q is registered twice", v.name)
		}
		seen[v.name] = true
		if v.summary == "" || v.build == nil || v.render == nil {
			t.Errorf("verb %q is missing a summary, build or render", v.name)
		}
		tokens := strings.Fields(v.name)
		if len(tokens) == 0 {
			// Every name is indexed at [0] by the dispatcher, the usage text and
			// both completion scripts, so a blank one panics rather than fails.
			t.Errorf("a verb has a blank name: %+v", v.summary)
			continue
		}
		if builtins[tokens[0]] {
			// A verb that shadows a built-in would be unreachable, and worse,
			// would escape the route check above by never being dispatched.
			t.Errorf("verb %q starts with the built-in word %q", v.name, tokens[0])
		}
		if v.exactBody && v.method != http.MethodPost {
			// The strict-schema routes this flag exists for are POST-only; a GET
			// would carry its arguments in the query, which this flag does not
			// keep the context out of.
			t.Errorf("verb %q claims exactBody on a %s route", v.name, v.method)
		}
	}
}

// A verb bound to an artifact route has to declare exactBody, or the generic
// POST path folds context values into a body the daemon decodes with
// DisallowUnknownFields and answers 400 to.
func TestArtifactVerbsUseTheExactBodyPath(t *testing.T) {
	for _, v := range verbs() {
		if strings.HasPrefix(v.path, "/api/artifacts/") && !v.exactBody {
			t.Errorf("verb %q binds to %q without exactBody", v.name, v.path)
		}
	}
}

// daemonRoutes reads every mux.HandleFunc pattern registered by
// internal/http/server.go.
func daemonRoutes(t *testing.T) map[string]bool {
	t.Helper()
	path := filepath.Join("..", "http", "server.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	routes := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "HandleFunc" {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		pattern, err := strconv.Unquote(literal.Value)
		if err != nil {
			return true
		}
		// Go 1.22 patterns are "METHOD /path"; a pattern with no method
		// accepts every method.
		fields := strings.Fields(pattern)
		switch len(fields) {
		case 1:
			for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
				routes[method+" "+fields[0]] = true
			}
		case 2:
			routes[pattern] = true
		}
		return true
	})
	return routes
}
