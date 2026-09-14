// Package stepscan reads the case labels out of a switch in brw's own source.
//
// It exists so a policy table can be tested against the code it decides for
// rather than against a second hand-written list. The plan and batch runners
// dispatch on a step's action with a switch; a consent rule or a content-boundary
// rule that classifies those actions is only as complete as the list it was
// written from, and a verb added to the switch with no row in the table is a step
// no rule decides. Reading the switch is what turns "we remembered" into "the
// test fails".
package stepscan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// SwitchCases returns every string case label of the switch on field inside the
// named function of a Go file. Two functions may share a name in one file only
// if they are methods on different types, which brw's runners are not, so the
// first match is the one.
func SwitchCases(path, function, field string) ([]string, error) {
	fileSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var found *ast.FuncDecl
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == function {
			found = fn
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%s declares no function %s; the source this table is checked against has moved", path, function)
	}
	var labels []string
	ast.Inspect(found, func(node ast.Node) bool {
		stmt, ok := node.(*ast.SwitchStmt)
		if !ok || !switchesOn(stmt.Tag, field) {
			return true
		}
		for _, item := range stmt.Body.List {
			clause, ok := item.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range clause.List {
				literal, ok := expr.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(literal.Value)
				if err == nil {
					labels = append(labels, value)
				}
			}
		}
		return true
	})
	if len(labels) == 0 {
		return nil, fmt.Errorf("%s: %s has no switch on %s with string cases", path, function, field)
	}
	return labels, nil
}

// switchesOn reports whether a switch tag selects the named field, whatever the
// receiver is called (step.Action, st.Action).
func switchesOn(tag ast.Expr, field string) bool {
	selector, ok := tag.(*ast.SelectorExpr)
	return ok && strings.EqualFold(selector.Sel.Name, field)
}
