// Package stepscan reads the case labels out of a switch in brw's own source.
package stepscan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// SwitchCases returns every string case label of the switch on field inside the named function of a Go file.
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
