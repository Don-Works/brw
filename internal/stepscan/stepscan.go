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
	constants := stringConstants(parsed)
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
				if value, ok := caseValue(expr, constants); ok {
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

// caseValue resolves one case label to the string it matches: a literal
// directly, or a named string constant declared in the same file. A switch that
// names its verbs as constants is the same list as one that spells them inline,
// and a scan that only reads literals would report such a switch as empty -
// which reads as "nothing to classify" rather than as "the source moved".
func caseValue(expr ast.Expr, constants map[string]string) (string, bool) {
	switch node := expr.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(node.Value)
		return value, err == nil
	case *ast.Ident:
		value, ok := constants[node.Name]
		return value, ok
	}
	return "", false
}

// stringConstants collects the file's top-level string constants by name.
func stringConstants(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, decl := range file.Decls {
		group, ok := decl.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range value.Names {
				if index >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[index].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				if unquoted, err := strconv.Unquote(literal.Value); err == nil {
					out[name.Name] = unquoted
				}
			}
		}
	}
	return out
}

// switchesOn reports whether a switch tag selects the named field, whatever the
// receiver is called (step.Action, st.Action) and through whatever normalising
// calls wrap it (strings.ToLower(strings.TrimSpace(p.Action))). Unwrapping the
// calls is what lets a table be checked against a switch that lowercases its
// subject first, which every verb a user types goes through.
func switchesOn(tag ast.Expr, field string) bool {
	switch node := tag.(type) {
	case *ast.SelectorExpr:
		return strings.EqualFold(node.Sel.Name, field)
	case *ast.CallExpr:
		for _, arg := range node.Args {
			if switchesOn(arg, field) {
				return true
			}
		}
	case *ast.ParenExpr:
		return switchesOn(node.X, field)
	}
	return false
}
