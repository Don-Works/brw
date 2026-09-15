package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Don-Works/brw/internal/brwconfig"
)

// brwdFlag is one flag registration read out of main.go.
type brwdFlag struct {
	name string
	env  string
}

// registeredFlags reads every flag brwd registers and the environment variable
// each one reads, straight out of the registration calls. Reading the source is
// the point: a second hand-written list would be exactly the thing that drifts.
func registeredFlags(t *testing.T) []brwdFlag {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var flags []brwdFlag
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != "flag" {
			return true
		}
		// StringVar/BoolVar/IntVar/DurationVar take (target, name, default,
		// usage); Var takes (value, name, usage) and reads no environment.
		var nameIndex, defaultIndex int
		switch selector.Sel.Name {
		case "StringVar", "BoolVar", "IntVar", "DurationVar", "Int64Var", "Float64Var", "UintVar":
			nameIndex, defaultIndex = 1, 2
		case "Var":
			nameIndex, defaultIndex = 1, -1
		default:
			return true
		}
		if nameIndex >= len(call.Args) {
			return true
		}
		literal, ok := call.Args[nameIndex].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		name, err := strconv.Unquote(literal.Value)
		if err != nil {
			return true
		}
		entry := brwdFlag{name: name}
		if defaultIndex >= 0 && defaultIndex < len(call.Args) {
			entry.env = environmentVariableIn(call.Args[defaultIndex])
		}
		flags = append(flags, entry)
		return true
	})
	sort.Slice(flags, func(i, j int) bool { return flags[i].name < flags[j].name })
	return flags
}

// environmentVariableIn finds the environment variable a flag's default reads:
// os.Getenv("X"), or one of brwd's envDefault/envBool/envInt/envDuration
// helpers, whose first argument is the variable.
func environmentVariableIn(expr ast.Expr) string {
	found := ""
	ast.Inspect(expr, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "os" {
				name = "os." + fn.Sel.Name
			}
		}
		switch name {
		case "os.Getenv", "os.LookupEnv", "envDefault", "envBool", "envInt", "envDuration":
		default:
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		if value, err := strconv.Unquote(literal.Value); err == nil && strings.HasPrefix(value, "BRW_") {
			found = value
		}
		return true
	})
	return found
}

// TestEveryEnvironmentVariableBrwdReadsIsInTheConfigTable is the anti-drift
// guard for the one inversion that would be invisible.
//
// brwd reads its environment as each flag's DEFAULT, so by the time brw.json is
// applied an env-configured flag looks exactly like an untouched one. The env
// table is what tells them apart. A flag that reads an environment variable and
// is missing from that table has its environment value silently overwritten by
// the file, and nothing anywhere says so.
func TestEveryEnvironmentVariableBrwdReadsIsInTheConfigTable(t *testing.T) {
	flags := registeredFlags(t)
	if len(flags) < 40 {
		t.Fatalf("parsed only %d flag registrations from main.go; the shape this test reads has changed", len(flags))
	}
	table := brwconfig.EnvTable()
	names := map[string]bool{}
	for _, entry := range flags {
		names[entry.name] = true
		switch {
		case entry.env == "":
			if got, listed := table[entry.name]; listed {
				t.Errorf("brwconfig says --%s reads %s, but brwd reads no environment variable for it", entry.name, got)
			}
		case table[entry.name] != entry.env:
			t.Errorf("--%s reads %s in brwd, but brwconfig has %q; the file would override the environment for it",
				entry.name, entry.env, table[entry.name])
		}
	}
	for name, env := range table {
		if !names[name] {
			t.Errorf("brwconfig maps --%s to %s, but brwd has no such flag", name, env)
		}
	}
}

// TestEveryFlagAFileMayNotSetIsARealFlag: a deny-list entry that names nothing
// protects nothing, and reads as though it does.
func TestEveryFlagAFileMayNotSetIsARealFlag(t *testing.T) {
	names := map[string]bool{}
	for _, entry := range registeredFlags(t) {
		names[entry.name] = true
	}
	for name, reason := range brwconfig.NotConfigurable() {
		if !names[name] {
			t.Errorf("brwconfig refuses --%s in a config file (%q), but brwd has no such flag", name, reason)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("--%s is refused with no reason", name)
		}
	}
}
