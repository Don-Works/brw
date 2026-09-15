package siteconsent

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// refusalInstances is one live value per refusal this package produces. The
// enumeration test below reads the package's own source for every type that
// implements error and fails on one this table does not carry, so a refusal
// added next week cannot quietly classify as an infrastructure failure and get
// retried forever by a scheduler.
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

// TestEveryRefusalTypeIsRecognised enumerates the domain out of the source
// rather than trusting a hand-kept list. A type in this package whose name ends
// in Error and that has an Error() method is a refusal by construction; if
// IsRefusal does not recognise it, a scheduled run reports a policy decision as
// a transient fault.
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
		// A refusal has to survive being wrapped: every surface between the gate
		// and the caller adds context to it.
		if !IsRefusal(fmt.Errorf("brw_click on https://example.test: %w", instance)) {
			t.Errorf("IsRefusal loses %s once it is wrapped", name)
		}
	}
	for name := range refusalInstances {
		if !contains(declared, name) {
			t.Errorf("refusalInstances names %s, which this package does not declare", name)
		}
	}

	// The sentinel is not a type, so the scan above cannot see it.
	if !IsRefusal(ErrPromptUnanswerable) {
		t.Error("IsRefusal does not recognise ErrPromptUnanswerable")
	}
	if !IsRefusal(fmt.Errorf("ask: %w", ErrPromptUnanswerable)) {
		t.Error("IsRefusal loses ErrPromptUnanswerable once it is wrapped")
	}

	// And the other direction: an ordinary failure must not read as a policy
	// decision, or a scheduler stops retrying something it should retry.
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

// errorTypesInPackage returns every type declared in this package whose name
// ends in Error and which has a value or pointer method named Error.
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

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
