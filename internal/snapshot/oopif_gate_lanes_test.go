package snapshot

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// frameReadGateParam is the name every function that reaches into a cross-origin
// frame takes its consent gate under. Naming it once is what lets the scan below
// tell "this lane asked" from "this lane has a function value in scope".
const frameReadGateParam = "allowOrigin"

// TestEveryFrameTargetAttachAsksTheFrameReadGate enumerates the lanes that reach
// INTO a cross-origin iframe, and fails on one that reaches in without asking.
//
// The gate was added to include_frames and not to the click path, and the click
// path is the same two CDP calls in the same third party's document: attach a
// session to the frame's own target, evaluate brw's scripts there. Whether the
// gate runs was decided per lane, by whoever wrote that lane, which is how the
// second one came to have no gate at all for a release.
//
// So the property is anchored at the one call that makes a lane a lane.
// attachFrameTarget is the only way into a frame's document from this package,
// and every call to it has to sit in a function that took the frame-read gate as
// a parameter and called it first. A third lane added next year fails here until
// it does the same, with no edit to this test.
func TestEveryFrameTargetAttachAsksTheFrameReadGate(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	attaches := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			attachedAt := callPositions(fn.Body, "attachFrameTarget")
			if len(attachedAt) == 0 {
				continue
			}
			attaches += len(attachedAt)
			if !takesFrameReadGate(fn) {
				t.Errorf("%s.%s attaches a session to a cross-origin frame's own target without taking a %s parameter, so whoever calls it has no way to decide about that third party's document", name, fn.Name.Name, frameReadGateParam)
				continue
			}
			gatedAt := callPositions(fn.Body, frameReadGateParam)
			for _, attach := range attachedAt {
				if !anyBefore(gatedAt, attach) {
					t.Errorf("%s.%s attaches to a cross-origin frame's target at line %d without calling %s first: the call was authorized against the origin the TAB is showing, which is not a grant to read or actuate what that page embeds", name, fn.Name.Name, fset.Position(attach).Line, frameReadGateParam)
				}
			}
		}
	}
	if attaches == 0 {
		t.Fatalf("the scan found no call to attachFrameTarget; it has been renamed or moved, so this test is proving nothing")
	}
}

// callPositions returns the position of every call to a plain identifier named
// callee inside body.
func callPositions(body *ast.BlockStmt, callee string) []token.Pos {
	var found []token.Pos
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, isIdent := call.Fun.(*ast.Ident); isIdent && ident.Name == callee {
			found = append(found, call.Lparen)
		}
		return true
	})
	return found
}

// takesFrameReadGate reports whether fn declares the gate as a func-typed
// parameter. A local variable of the same name would satisfy the call scan on its
// own, and a lane that gates itself against a function it made up has decided
// nothing.
func takesFrameReadGate(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, field := range fn.Type.Params.List {
		if _, isFunc := field.Type.(*ast.FuncType); !isFunc {
			continue
		}
		for _, name := range field.Names {
			if name.Name == frameReadGateParam {
				return true
			}
		}
	}
	return false
}

func anyBefore(positions []token.Pos, limit token.Pos) bool {
	for _, pos := range positions {
		if pos < limit {
			return true
		}
	}
	return false
}
