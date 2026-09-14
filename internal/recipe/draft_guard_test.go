package recipe

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const guardFunc = "GuardTraceActionForCompilation"

// The refusal is only worth having if it is applied by every compiler, not by
// the one that happened to be written first. A second trace-to-recipe compiler
// that refuses a credential action through some other check — a redaction flag,
// a field-name regex — refuses it incidentally, and stops refusing it the day
// that other check changes.
//
// So this enumerates the package rather than naming callers. A trace-to-recipe
// compilation is recognised by its shape: it consumes recorded trace actions
// and produces a Recipe. Helpers that transform already-filtered actions are
// not entry points and are not the place for the refusal.
func TestEveryTraceCompilerCallsTheCredentialGuard(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == guardFunc {
				continue
			}
			if !takesTraceActions(fn) || !producesRecipe(fn) {
				continue
			}
			checked++
			if !callsGuard(fn) {
				t.Errorf("%s.%s takes recorded trace actions and never calls %s; a compiler that refuses a credential-sourced action some other way refuses it by accident",
					name, fn.Name.Name, guardFunc)
			}
		}
	}
	// Nothing matched would mean the scan is looking for the wrong shape, not
	// that the package is clean.
	if checked == 0 {
		t.Fatal("found no function turning []TraceAction into a Recipe; the scan is not reaching the compilers")
	}
}

func takesTraceActions(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, param := range fn.Type.Params.List {
		slice, ok := param.Type.(*ast.ArrayType)
		if !ok || slice.Len != nil {
			continue
		}
		if element, ok := slice.Elt.(*ast.Ident); ok && element.Name == "TraceAction" {
			return true
		}
	}
	return false
}

// producesRecipe accepts Recipe, []Recipe and *Recipe in any result position.
func producesRecipe(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, result := range fn.Type.Results.List {
		expr := result.Type
		switch typed := expr.(type) {
		case *ast.ArrayType:
			expr = typed.Elt
		case *ast.StarExpr:
			expr = typed.X
		}
		if ident, ok := expr.(*ast.Ident); ok && ident.Name == "Recipe" {
			return true
		}
	}
	return false
}

func callsGuard(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == guardFunc {
			found = true
		}
		return !found
	})
	return found
}
