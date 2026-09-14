package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
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
		if _, ok := routes[pattern]; !ok {
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
			// The dispatcher and both completion scripts skip a nameless entry
			// rather than index it, so this report is all that is left to say
			// the verb exists but can never be typed.
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

// A verb bound to a route the daemon decodes with DisallowUnknownFields has to
// declare exactBody, or the generic path folds tab_id into a body that route
// answers 400 to. Which routes those are is read out of internal/http's own
// handlers rather than matched on a path prefix: the artifact routes are not
// the only strict ones, and the next verb to reach for a recipe route would
// otherwise take the same 400 with nothing here to catch it.
func TestVerbsOnStrictlyDecodedRoutesUseTheExactBodyPath(t *testing.T) {
	strict := strictRoutes(t)
	// A parse that resolved nothing would pass every verb, so anchor on routes
	// whose shape this test is written against.
	for pattern, want := range map[string]strictRoute{
		"POST /api/artifacts/read":   {strict: true},
		"POST /api/artifacts/delete": {strict: true},
		"POST /api/recipes/search":   {strict: true},
		// These two declare tab_id themselves, so --tab reaches them and the
		// generic path is the right one.
		"POST /api/recipes/run":       {strict: true, acceptsTabID: true},
		"POST /api/artifacts/capture": {strict: true, acceptsTabID: true},
		"GET /api/page/read":          {},
	} {
		if got := strict[pattern]; got != want {
			t.Fatalf("%s decodes as %+v, want %+v; the handler shape internal/http uses must have changed", pattern, got, want)
		}
	}

	for _, v := range verbs() {
		route := strict[v.method+" "+v.path]
		switch {
		case route.strict && !route.acceptsTabID && !v.exactBody:
			t.Errorf("verb %q binds to %q, which declares no tab_id and rejects unknown fields, without exactBody", v.name, v.path)
		case v.exactBody && !route.strict:
			t.Errorf("verb %q claims exactBody on %q, which the daemon does not decode strictly", v.name, v.path)
		}
	}
}

// strictRoute is what internal/http does with one route's request body.
type strictRoute struct {
	// strict marks a handler that decodes with DisallowUnknownFields.
	strict bool
	// acceptsTabID marks a strict schema that declares tab_id, which is the one
	// field the generic client path folds in from --tab.
	acceptsTabID bool
}

// strictRoutes reads each registered route's handler and reports how it decodes
// the request body.
func strictRoutes(t *testing.T) map[string]strictRoute {
	t.Helper()
	files := daemonFiles(t)
	handlers := map[string]*ast.FuncDecl{}
	structs := map[string]*ast.StructType{}
	for _, file := range files {
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				handlers[decl.Name.Name] = decl
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if structType, ok := typeSpec.Type.(*ast.StructType); ok {
						structs[typeSpec.Name.Name] = structType
					}
				}
			}
		}
	}

	routes := map[string]strictRoute{}
	for pattern, handler := range daemonRoutes(t) {
		decl := handlers[handler]
		if decl == nil || decl.Body == nil {
			continue
		}
		target, ok := strictDecodeTarget(decl)
		if !ok {
			continue
		}
		routes[pattern] = strictRoute{strict: true, acceptsTabID: declaresTabID(decl, target, structs)}
	}
	return routes
}

// strictDecodeTarget names the variable a handler decodes strictly into, if it
// decodes strictly at all.
func strictDecodeTarget(decl *ast.FuncDecl) (string, bool) {
	var target string
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 3 {
			return true
		}
		name, ok := call.Fun.(*ast.Ident)
		if !ok || (name.Name != "decodeStrict" && name.Name != "decodeArtifactRequest") {
			return true
		}
		unary, ok := call.Args[2].(*ast.UnaryExpr)
		if !ok || unary.Op != token.AND {
			return true
		}
		if ident, ok := unary.X.(*ast.Ident); ok {
			target = ident.Name
		}
		return false
	})
	return target, target != ""
}

// declaresTabID reports whether the request schema decoded into target carries a
// tab_id field. A schema this walk cannot resolve counts as not declaring one:
// requiring exactBody is the safe answer, since it is what the artifact routes
// already use.
func declaresTabID(decl *ast.FuncDecl, target string, structs map[string]*ast.StructType) bool {
	var schema *ast.StructType
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for _, name := range spec.Names {
			if name.Name != target {
				continue
			}
			switch declared := spec.Type.(type) {
			case *ast.StructType:
				schema = declared
			case *ast.Ident:
				schema = structs[declared.Name]
			}
		}
		return true
	})
	if schema == nil {
		return false
	}
	for _, field := range schema.Fields.List {
		if field.Tag != nil && strings.Contains(field.Tag.Value, `json:"tab_id"`) {
			return true
		}
	}
	return false
}

// daemonRoutes maps every mux.HandleFunc pattern internal/http registers to the
// name of the handler method it registers, so a test can ask what that handler
// does with the request body as well as whether the route exists.
func daemonRoutes(t *testing.T) map[string]string {
	t.Helper()
	routes := map[string]string{}
	for _, file := range daemonFiles(t) {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
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
			handler := ""
			if method, ok := call.Args[1].(*ast.SelectorExpr); ok {
				handler = method.Sel.Name
			}
			// Go 1.22 patterns are "METHOD /path"; a pattern with no method
			// accepts every method.
			fields := strings.Fields(pattern)
			switch len(fields) {
			case 1:
				for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
					routes[method+" "+fields[0]] = handler
				}
			case 2:
				routes[pattern] = handler
			}
			return true
		})
	}
	return routes
}

// daemonFiles parses internal/http's own source, tests excluded.
func daemonFiles(t *testing.T) []*ast.File {
	t.Helper()
	dir := filepath.Join("..", "http")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatalf("no source parsed from %s", dir)
	}
	return files
}
