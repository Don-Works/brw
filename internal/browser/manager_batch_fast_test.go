package browser

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/Don-Works/brw/internal/snapshot"
)

func TestDirectBatchUsesRawPrimitives(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "manager.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var batch *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "executeBatchStep" {
			batch = fn
			break
		}
	}
	if batch == nil {
		t.Fatal("missing executeBatchStep")
	}
	var observedCalls []string
	ast.Inspect(batch.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := selector.X.(*ast.Ident)
		if !ok || ident.Name != "m" {
			return true
		}
		switch selector.Sel.Name {
		case "Click", "Type", "Fill", "Select", "Press", "Scroll", "Hover":
			observedCalls = append(observedCalls, selector.Sel.Name)
		}
		return true
	})
	if len(observedCalls) != 0 {
		t.Fatalf("executeBatchStep must use raw primitives and one final observation, found observed wrappers: %v", observedCalls)
	}
}

func TestDirectBatchActsSequentiallyAndReturnsOneFinalObservation(t *testing.T) {
	m := newHeadlessManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opened, err := m.Open(ctx, "about:blank")
	if err != nil {
		t.Fatal(err)
	}
	tabCtx := WithTabID(ctx, opened.Tab.ID)
	if _, err := m.Evaluate(tabCtx, `document.body.innerHTML='<input aria-label="Name"><button>Apply</button><div id="done"></div>'; document.querySelector('button').onclick=function(){document.querySelector('#done').textContent=document.querySelector('input').value}; true`); err != nil {
		t.Fatal(err)
	}
	snap, err := m.Snapshot(tabCtx, snapshot.SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var inputRef, buttonRef string
	for _, el := range snap.Elements {
		switch {
		case el.Role == "textbox":
			inputRef = el.Ref
		case el.Role == "button":
			buttonRef = el.Ref
		}
	}
	if inputRef == "" || buttonRef == "" {
		t.Fatalf("missing refs: %+v", snap.Elements)
	}
	result, err := m.ExecuteBatch(tabCtx, []BatchStep{
		{Action: "fill", Ref: inputRef, Text: "Ada"},
		{Action: "click", Ref: buttonRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.StepsCompleted != 2 || result.TabID != opened.Tab.ID || len(result.Changed) == 0 {
		t.Fatalf("batch result = %+v", result)
	}
	value, err := m.Evaluate(tabCtx, `document.querySelector('#done').textContent`)
	if err != nil || !strings.Contains(strings.TrimSpace(value.(string)), "Ada") {
		t.Fatalf("final DOM value = %#v, err=%v", value, err)
	}
}
