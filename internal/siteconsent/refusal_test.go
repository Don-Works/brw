package siteconsent

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

var refusalInstances = map[string]error{
	"NotGrantedError":           &NotGrantedError{Origin: "https://example.test", Scope: ScopeRead},
	"DeniedError":               &DeniedError{Origin: "https://example.test", Scope: ScopeRead, When: time.Unix(0, 0)},
	"CategoryBlockedError":      &CategoryBlockedError{Origin: "https://example.test", Scope: ScopeRead, Category: Category{Name: "fixture"}},
	"AdminBlockedError":         &AdminBlockedError{Origin: "https://example.test", Scope: ScopeRead, Entry: "example.test"},
	"ConfirmationRequiredError": &ConfirmationRequiredError{Tool: "brw_click", Origin: "https://example.test"},
	"ConfirmationDeclinedError": &ConfirmationDeclinedError{Tool: "brw_click", Origin: "https://example.test"},
	"CannotDecideError":         &CannotDecideError{Tool: "brw_click", Reason: "no tab"},
	"LocalTargetError":          &LocalTargetError{Scheme: "file", Target: "/tmp/x"},
}

func TestEveryRefusalTypeIsRecognised(t *testing.T) {
	declared := errorTypesInPackage(t)
	if len(declared) < 5 {
		t.Fatalf("found only %d error types in this package; the scan is not reading the source", len(declared))
	}
	for _, name := range declared {
		instance, ok := refusalInstances[name]
		if !ok {
			t.Errorf("%s is an error type in this package but refusalInstances has no value for it; add one and make sure IsRefusal recognises it", name)
			continue
		}
		if !IsRefusal(instance) {
			t.Errorf("IsRefusal does not recognise %s, so a scheduler would treat this refusal as an infrastructure failure and retry it", name)
		}

		if !IsRefusal(fmt.Errorf("brw_click on https://example.test: %w", instance)) {
			t.Errorf("IsRefusal loses %s once it is wrapped", name)
		}
	}
	for name := range refusalInstances {
		if !slices.Contains(declared, name) {
			t.Errorf("refusalInstances names %s, which this package does not declare", name)
		}
	}

	if !IsRefusal(ErrPromptUnanswerable) {
		t.Error("IsRefusal does not recognise ErrPromptUnanswerable")
	}
	if !IsRefusal(fmt.Errorf("ask: %w", ErrPromptUnanswerable)) {
		t.Error("IsRefusal loses ErrPromptUnanswerable once it is wrapped")
	}

	for _, notARefusal := range []error{
		errors.New("bridge transport closed"),
		errors.New("no tab: target closed"),
		nil,
	} {
		if IsRefusal(notARefusal) {
			t.Errorf("IsRefusal(%v) = true", notARefusal)
		}
	}
}

func errorTypesInPackage(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	types := map[string]bool{}
	withErrorMethod := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, declaration := range file.Decls {
			switch decl := declaration.(type) {
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if ok && strings.HasSuffix(typeSpec.Name.Name, "Error") {
						types[typeSpec.Name.Name] = true
					}
				}
			case *ast.FuncDecl:
				if decl.Name.Name != "Error" || decl.Recv == nil || len(decl.Recv.List) == 0 {
					continue
				}
				withErrorMethod[receiverTypeName(decl.Recv.List[0].Type)] = true
			}
		}
	}
	var out []string
	for name := range types {
		if withErrorMethod[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func receiverTypeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(typed.X)
	case *ast.Ident:
		return typed.Name
	default:
		return ""
	}
}
